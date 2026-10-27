// Package anthropic is a model.Provider for the Anthropic Messages API, with
// streaming and tool use.
//
// Mapping: system messages become the top-level system prompt; tool results
// become user messages with tool_result blocks (consecutive ones merged);
// assistant tool calls become tool_use blocks.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"strings"

	"github.com/ecociel/agent-harness/model"
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

func encode(req model.Request, defModel string, defMax int) wireRequest {
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
			if m.Content != "" {
				add("assistant", block{Type: "text", Text: m.Content})
			}
			for _, c := range m.ToolCalls {
				in := c.Arguments
				if len(in) == 0 {
					in = json.RawMessage(`{}`)
				}
				add("assistant", block{Type: "tool_use", ID: c.ID, Name: c.Name, Input: in})
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
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock block `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
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

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
	return func(yield func(model.Event, error) bool) {
		body, err := json.Marshal(encode(req, p.Model, p.MaxTokens))
		if err != nil {
			yield(nil, err)
			return
		}
		base := p.BaseURL
		if base == "" {
			base = "https://api.anthropic.com"
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			yield(nil, err)
			return
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
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode/100 != 2 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			yield(nil, &StatusError{Code: resp.StatusCode, Body: string(msg)})
			return
		}
		var (
			finish  model.Finish
			toolIdx = map[int]int{} // content block index → tool call index
			stopped bool
			ierr    error
		)
		err = sse.Read(resp.Body, func(ev sse.Event) bool {
			if ev.Data == "" {
				return true
			}
			var se streamEvent
			if err := json.Unmarshal([]byte(ev.Data), &se); err != nil {
				ierr = fmt.Errorf("anthropic: bad event: %w", err)
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
				finish.Usage.Input = se.Message.Usage.InputTokens
				finish.Usage.Cached = se.Message.Usage.CacheReadInputTokens
			case "content_block_start":
				if se.ContentBlock.Type == "tool_use" {
					i := len(toolIdx)
					toolIdx[se.Index] = i
					return emit(model.ToolCallDelta{Index: i, ID: se.ContentBlock.ID, Name: se.ContentBlock.Name})
				}
			case "content_block_delta":
				switch se.Delta.Type {
				case "text_delta":
					return emit(model.TextDelta{Text: se.Delta.Text})
				case "input_json_delta":
					return emit(model.ToolCallDelta{Index: toolIdx[se.Index], Arguments: se.Delta.PartialJSON})
				}
			case "message_delta":
				finish.Usage.Output = se.Usage.OutputTokens
				finish.Reason = mapStop(se.Delta.StopReason)
			case "message_stop":
				return false
			case "error":
				msg := "unknown error"
				if se.Error != nil {
					msg = se.Error.Type + ": " + se.Error.Message
				}
				ierr = fmt.Errorf("anthropic: %s", msg)
				return false
			}
			return true
		})
		if stopped {
			return
		}
		if err == nil {
			err = ierr
		}
		if err != nil {
			yield(nil, err)
			return
		}
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
		return model.FinishContentFilter
	}
	return model.FinishStop
}

// StatusError is a non-2xx answer from the API.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("anthropic: HTTP %d: %s", e.Code, e.Body) }
