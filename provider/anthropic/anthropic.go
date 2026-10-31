// Package anthropic is a model.Provider for the Anthropic Messages API, with
// streaming and tool use.
//
// Mapping: system messages become the top-level system prompt; tool results
// become user messages with tool_result blocks (consecutive ones merged);
// assistant tool calls become tool_use blocks.
//
// Thinking is on by default on current models, and a signature covers everything
// before its block. So a reply with thinking keeps its block order in Opaque, and an
// assistant message goes back in that order. Opaque holds no reply text, only how many
// bytes of Message.Content each text block took, so what goes back is always what the
// history says: a hook or redactor that changed the reply cannot be undone by a replay.
// A message that no longer fits its order goes back without thinking.
//
// A history edit such as compaction leaves later signatures invalid. The API answers
// 400 before any output, unbilled, and Complete repeats that one call with all thinking
// stripped. The model loses that reasoning, and the history keeps its blocks, so this
// costs one extra request on each later turn.
package anthropic

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/internal/httperr"
	"github.com/ecociel/agent-harness/provider/internal/sse"
)

// Version is the anthropic-version header value.
const Version = "2023-06-01"

// Provider talks to the Anthropic Messages API.
type Provider struct {
	// BaseURL without /v1 (default https://api.anthropic.com).
	BaseURL string
	APIKey  string
	// Model is used when the request does not name one.
	Model string
	// MaxTokens is the default output cap (the API requires one; default 4096).
	MaxTokens int
	Client    *http.Client
}

// FromEnv builds a provider from ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL and
// ANTHROPIC_MODEL.
func FromEnv() *Provider {
	return &Provider{BaseURL: os.Getenv("ANTHROPIC_BASE_URL"), APIKey: os.Getenv("ANTHROPIC_API_KEY"),
		Model: os.Getenv("ANTHROPIC_MODEL")}
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	// Thinking is a pointer because a block with its text omitted must still send "".
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`
	Data      string  `json:"data,omitempty"`
}

func (b block) isThinking() bool { return b.Type == "thinking" || b.Type == "redacted_thinking" }

// saved is one block of a reply, as Opaque keeps it. Thinking goes back as it came. A
// text block keeps only its length and a tool_use nothing, because those are in the
// message.
type saved struct {
	Type      string  `json:"type"`
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`
	Data      string  `json:"data,omitempty"`
	Len       int     `json:"len,omitempty"`
}

// opaque is the shape of Message.Opaque. The key says whose it is, so a message from
// another provider is left alone.
type opaque struct {
	Anthropic []saved `json:"anthropic"`
}

// seal is the Opaque for a reply, or nil if it had no thinking to send back.
func seal(blocks []saved) json.RawMessage {
	for _, b := range blocks {
		if (block{Type: b.Type}).isThinking() {
			raw, err := json.Marshal(opaque{blocks})
			if err != nil {
				return nil
			}
			return raw
		}
	}
	return nil
}

// replay rebuilds an assistant message in the order it was produced. It reports false
// if the message has no order to follow, or no longer fits the one it has.
func replay(m model.Message) ([]block, bool) {
	var o opaque
	if len(m.Opaque) == 0 || json.Unmarshal(m.Opaque, &o) != nil {
		return nil, false
	}
	var out []block
	text, calls := m.Content, m.ToolCalls
	for _, b := range o.Anthropic {
		switch {
		case b.Type == "thinking":
			out = append(out, block{Type: b.Type, Thinking: cmp.Or(b.Thinking, new(string)), Signature: b.Signature})
		case b.Type == "redacted_thinking":
			out = append(out, block{Type: b.Type, Data: b.Data})
		case b.Type == "text" && b.Len == 0:
			// The API refuses an empty text block, and there is nothing to keep.
		case b.Type == "text" && b.Len <= len(text):
			out = append(out, block{Type: "text", Text: text[:b.Len]})
			text = text[b.Len:]
		case b.Type == "tool_use" && len(calls) > 0:
			out = append(out, toolUse(calls[0]))
			calls = calls[1:]
		default:
			return nil, false
		}
	}
	return out, text == "" && len(calls) == 0
}

func toolUse(c model.ToolCall) block {
	in := c.Arguments
	if len(in) == 0 {
		in = json.RawMessage(`{}`)
	}
	return block{Type: "tool_use", ID: c.ID, Name: c.Name, Input: in}
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireRequest struct {
	Model       string     `json:"model"`
	MaxTokens   int        `json:"max_tokens"`
	System      string     `json:"system,omitempty"`
	Messages    []message  `json:"messages"`
	Tools       []wireTool `json:"tools,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	Stream      bool       `json:"stream"`
}

func encode(req model.Request, defModel string, defMax int, stripThinking bool) wireRequest {
	w := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, Stream: true}
	if w.Model == "" {
		w.Model = defModel
	}
	if w.MaxTokens == 0 {
		w.MaxTokens = defMax
	}
	if w.MaxTokens == 0 {
		w.MaxTokens = 4096
	}
	var sys []string
	add := func(role string, b block) {
		if n := len(w.Messages); n > 0 && w.Messages[n-1].Role == role {
			w.Messages[n-1].Content = append(w.Messages[n-1].Content, b)
			return
		}
		w.Messages = append(w.Messages, message{Role: role, Content: []block{b}})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case model.RoleSystem:
			sys = append(sys, m.Content)
		case model.RoleUser:
			add("user", block{Type: "text", Text: m.Content})
		case model.RoleTool:
			add("user", block{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content})
		case model.RoleAssistant:
			if blocks, ok := replay(m); ok {
				for _, b := range blocks {
					if !stripThinking || !b.isThinking() {
						add("assistant", b)
					}
				}
				continue
			}
			if m.Content != "" {
				add("assistant", block{Type: "text", Text: m.Content})
			}
			for _, c := range m.ToolCalls {
				add("assistant", toolUse(c))
			}
		}
	}
	w.System = strings.Join(sys, "\n\n")
	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		w.Tools = append(w.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return w
}

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock block `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// send makes one call and returns a 2xx response, or a classified error.
func (p *Provider) send(ctx context.Context, w wireRequest) (*http.Response, error) {
	body, err := json.Marshal(w)
	if err != nil {
		return nil, &model.Error{Class: model.ClassInvalid, Err: err}
	}
	base := p.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, &model.Error{Class: model.ClassInvalid, Err: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("x-api-key", p.APIKey)
	hreq.Header.Set("anthropic-version", Version)
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, &model.Error{Class: model.ClassTransient, Err: err}
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(msg, &e) // a body that is not JSON leaves the status to decide
	return nil, &model.Error{Class: classify(resp.StatusCode, e.Error.Type, e.Error.Message),
		RetryAfter: httperr.RetryAfter(resp.Header, time.Now()),
		Err:        &StatusError{Code: resp.StatusCode, Body: string(msg)}}
}

// signatureRefused reports the 400 that a history edit causes.
func signatureRefused(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusBadRequest &&
		strings.Contains(se.Body, "signature") && strings.Contains(se.Body, "thinking")
}

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
	return func(yield func(model.Event, error) bool) {
		resp, err := p.send(ctx, encode(req, p.Model, p.MaxTokens, false))
		if signatureRefused(err) {
			resp, err = p.send(ctx, encode(req, p.Model, p.MaxTokens, true))
		}
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var (
			finish     model.Finish
			blocks     []saved
			at         = map[int]int{} // content block index → position in blocks
			toolIdx    = map[int]int{} // content block index → tool call index
			ended      bool
			windowFull bool
			stopped    bool
			ierr       error
		)
		err = sse.Read(resp.Body, func(ev sse.Event) bool {
			if ev.Data == "" {
				return true
			}
			var se streamEvent
			if err := json.Unmarshal([]byte(ev.Data), &se); err != nil {
				ierr = &model.Error{Class: model.ClassTransient, Err: fmt.Errorf("anthropic: bad event: %w", err)}
				return false
			}
			emit := func(e model.Event) bool {
				if !yield(e, nil) {
					stopped = true
					return false
				}
				return true
			}
			switch se.Type {
			case "message_start":
				u := se.Message.Usage
				finish.Usage.Input = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				finish.Usage.Cached = u.CacheReadInputTokens
				finish.Usage.Output = u.OutputTokens
			case "content_block_start":
				b := se.ContentBlock
				at[se.Index] = len(blocks)
				blocks = append(blocks, saved{Type: b.Type, Thinking: b.Thinking, Signature: b.Signature, Data: b.Data})
				if b.Type == "tool_use" {
					i := len(toolIdx)
					toolIdx[se.Index] = i
					return emit(model.ToolCallDelta{Index: i, ID: b.ID, Name: b.Name})
				}
			case "content_block_delta":
				cur := &saved{}
				if i, ok := at[se.Index]; ok {
					cur = &blocks[i]
				}
				switch se.Delta.Type {
				case "text_delta":
					cur.Len += len(se.Delta.Text)
					return emit(model.TextDelta{Text: se.Delta.Text})
				case "input_json_delta":
					return emit(model.ToolCallDelta{Index: toolIdx[se.Index], Arguments: se.Delta.PartialJSON})
				case "thinking_delta":
					text := se.Delta.Thinking
					if cur.Thinking != nil {
						text = *cur.Thinking + text
					}
					cur.Thinking = &text
				case "signature_delta":
					cur.Signature += se.Delta.Signature
				}
			case "message_delta":
				finish.Usage.Output = se.Usage.OutputTokens
				finish.Reason = mapStop(se.Delta.StopReason)
				ended = se.Delta.StopReason != ""
				windowFull = se.Delta.StopReason == "model_context_window_exceeded"
			case "message_stop":
				return false
			case "error":
				class, msg := httperr.Class(0), "unknown error"
				if se.Error != nil {
					class, msg = classify(0, se.Error.Type, se.Error.Message), se.Error.Type+": "+se.Error.Message
				}
				ierr = &model.Error{Class: class, Err: fmt.Errorf("anthropic: %s", msg)}
				return false
			}
			return true
		})
		if stopped {
			return
		}
		switch {
		case err != nil:
			err = &model.Error{Class: model.ClassTransient, Err: err}
		case ierr != nil:
			err = ierr
		case !ended:
			err = &model.Error{Class: model.ClassTransient, Err: errors.New("anthropic: stream ended before the reply did")}
		case windowFull:
			err = &model.Error{Class: model.ClassContextLength, Err: errors.New("anthropic: the context window filled up mid-reply")}
		}
		if err != nil {
			var e *model.Error
			errors.As(err, &e)
			e.Usage = finish.Usage // billed, whatever became of the reply
			yield(nil, e)
			return
		}
		finish.Opaque = seal(blocks)
		yield(finish, nil)
	}
}

func mapStop(s string) model.FinishReason {
	switch s {
	case "tool_use":
		return model.FinishToolCalls
	case "max_tokens":
		return model.FinishLength
	case "refusal":
		return model.FinishRefusal
	}
	return model.FinishStop
}

// classify reads Anthropic's error type, which says more than the status. Billing is
// an auth failure because a person has to fix it, so retrying cannot help.
func classify(status int, kind, msg string) model.ErrorClass {
	switch kind {
	case "rate_limit_error":
		return model.ClassRateLimit
	case "overloaded_error", "api_error", "timeout_error":
		return model.ClassTransient
	case "authentication_error", "permission_error", "billing_error":
		return model.ClassAuth
	case "request_too_large":
		return model.ClassContextLength
	case "not_found_error":
		return model.ClassInvalid
	case "invalid_request_error":
		if m := strings.ToLower(msg); strings.Contains(m, "prompt is too long") || strings.Contains(m, "exceed context limit") {
			return model.ClassContextLength
		}
		return model.ClassInvalid
	}
	return httperr.Class(status)
}

// StatusError is a non-2xx answer from the API.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("anthropic: HTTP %d: %s", e.Code, e.Body) }
