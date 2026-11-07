// Package openai is a model.Provider for any OpenAI-compatible
// /chat/completions endpoint, with streaming and tool calls.
//
// Parts go before the text of a user message: an image as an image_url data URL,
// WAV or MP3 audio as input_audio, and a document as a file where the endpoint
// takes files. Any other part goes as its text alternative.
//
// The endpoint is stateless and returns nothing that must go back on the next call, so
// this adapter never fills Opaque and ignores what another provider left there.
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
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

// Provider talks to an OpenAI-compatible API.
type Provider struct {
	// BaseURL up to and including /v1, e.g. https://api.openai.com/v1.
	BaseURL string
	APIKey  string
	// Model is used when the request does not name one.
	Model  string
	Client *http.Client
	// Headers are added to every request (e.g. OpenRouter attribution).
	Headers map[string]string
	// Files says the endpoint takes documents as file parts. Without it a document
	// goes as its text alternative.
	Files bool
}

// FromEnv builds a provider from OPENAI_BASE_URL (default
// https://api.openai.com/v1), OPENAI_API_KEY and OPENAI_MODEL.
func FromEnv() *Provider {
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &Provider{BaseURL: base, APIKey: os.Getenv("OPENAI_API_KEY"), Model: os.Getenv("OPENAI_MODEL")}
}

type wireFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type wireToolCall struct {
	Index    *int     `json:"index,omitempty"`
	ID       string   `json:"id,omitempty"`
	Type     string   `json:"type,omitempty"`
	Function wireFunc `json:"function"`
}

// wireMessage.Content is a *string, nil for an assistant message of only tool
// calls, or a []wirePart.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wirePart struct {
	Type       string     `json:"type"`
	Text       string     `json:"text,omitempty"`
	ImageURL   *wireURL   `json:"image_url,omitempty"`
	InputAudio *wireAudio `json:"input_audio,omitempty"`
	File       *wireFile  `json:"file,omitempty"`
}

type wireURL struct {
	URL string `json:"url"`
}

type wireAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type wireFile struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions map[string]any `json:"stream_options,omitempty"`
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// apiError is what compatible servers agree on. Code is a string on OpenAI and a number
// on some others, and a few send error as a bare string, which decodes to nothing here
// and leaves the status to decide.
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (e apiError) kind() string {
	if s, ok := e.Code.(string); ok && s != "" {
		return s
	}
	return e.Type
}

// classify reads the error code. A 429 for an empty account is not a rate limit:
// waiting cannot help, and a person has to fix it. Compatible servers word a refused
// part in many ways, so a refusal of a request that carried one is taken as the part's.
func classify(status int, e apiError, binary bool) model.ErrorClass {
	switch e.kind() {
	case "context_length_exceeded", "string_above_max_length":
		return model.ClassContextLength
	case "insufficient_quota", "billing_hard_limit_reached", "invalid_api_key":
		return model.ClassAuth
	case "rate_limit_exceeded":
		return model.ClassRateLimit
	case "server_error":
		return model.ClassTransient
	}
	if strings.Contains(strings.ToLower(e.Message), "maximum context length") {
		return model.ClassContextLength
	}
	if binary && httperr.Refused(status) {
		return model.ClassContent
	}
	if status == 0 && e.Type == "invalid_request_error" {
		return model.ClassInvalid
	}
	return httperr.Class(status)
}

// finishReason keeps the reasons the harness knows. Compatible servers send others, and
// an empty reason lets Collect work it out from the reply.
func finishReason(s string) model.FinishReason {
	switch r := model.FinishReason(s); r {
	case model.FinishStop, model.FinishLength, model.FinishToolCalls, model.FinishContentFilter, model.FinishRefusal:
		return r
	}
	if s == "function_call" {
		return model.FinishToolCalls
	}
	return ""
}

// encode also counts the parts it sends natively, which is what a refusal can be about.
func encode(req model.Request, def string, files bool) (wireRequest, int, error) {
	binary := 0
	w := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, Stream: true,
		StreamOptions: map[string]any{"include_usage": true}}
	if w.Model == "" {
		w.Model = def
	}
	for _, m := range req.Messages {
		content := m.Content
		wm := wireMessage{Role: string(m.Role), Content: &content, ToolCallID: m.ToolCallID}
		if m.Role == model.RoleAssistant && len(m.ToolCalls) > 0 && content == "" {
			wm.Content = nil
		}
		if len(m.Parts) > 0 {
			if m.Role != model.RoleUser {
				return w, 0, &model.Error{Class: model.ClassInvalid, Err: fmt.Errorf("openai: parts on a %s message", m.Role)}
			}
			parts := make([]wirePart, 0, len(m.Parts)+1)
			for _, p := range m.Parts {
				wp, native, err := encodePart(p, files)
				if err != nil {
					return w, 0, err
				}
				if native {
					binary++
				}
				parts = append(parts, wp)
			}
			if content != "" {
				parts = append(parts, wirePart{Type: "text", Text: content})
			}
			wm.Content = parts
		}
		for _, c := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: c.ID, Type: "function",
				Function: wireFunc{Name: c.Name, Arguments: string(c.Arguments)}})
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name, wt.Function.Description, wt.Function.Parameters = t.Name, t.Description, t.Parameters
		w.Tools = append(w.Tools, wt)
	}
	return w, binary, nil
}

// audioFormats are the input_audio formats, by media type.
var audioFormats = map[string]string{"audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/mpeg": "mp3", "audio/mp3": "mp3"}

// encodePart sends p natively where it can, and otherwise as its text.
func encodePart(p model.Part, files bool) (wirePart, bool, error) {
	if p.Bytes != nil {
		switch {
		case p.Kind == model.PartImage:
			return wirePart{Type: "image_url", ImageURL: &wireURL{URL: dataURL(p)}}, true, nil
		case p.Kind == model.PartAudio && audioFormats[p.MediaType] != "":
			return wirePart{Type: "input_audio", InputAudio: &wireAudio{Data: base64.StdEncoding.EncodeToString(p.Bytes), Format: audioFormats[p.MediaType]}}, true, nil
		case p.Kind == model.PartDocument && files:
			return wirePart{Type: "file", File: &wireFile{Filename: p.Name, FileData: dataURL(p)}}, true, nil
		}
	}
	if p.Text == "" {
		return wirePart{}, false, &model.Error{Class: model.ClassContent, Err: fmt.Errorf("openai: cannot send %s part %s (%s) and it has no text", p.Kind, p.Ref, p.MediaType)}
	}
	return wirePart{Type: "text", Text: p.Text}, false, nil
}

func dataURL(p model.Part) string {
	return "data:" + p.MediaType + ";base64," + base64.StdEncoding.EncodeToString(p.Bytes)
}

// send makes one call and returns a 2xx response, or a classified error.
func (p *Provider) send(ctx context.Context, req model.Request) (*http.Response, error) {
	w, binary, err := encode(req, p.Model, p.Files)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(w)
	if err != nil {
		return nil, &model.Error{Class: model.ClassInvalid, Err: err}
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &model.Error{Class: model.ClassInvalid, Err: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if p.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		hreq.Header.Set(k, v)
	}
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
		Error apiError `json:"error"`
	}
	_ = json.Unmarshal(msg, &e) // a body that is not JSON leaves the status to decide
	return nil, &model.Error{Class: classify(resp.StatusCode, e.Error, binary > 0),
		RetryAfter: httperr.RetryAfter(resp.Header, time.Now()),
		Err:        &StatusError{Code: resp.StatusCode, Body: string(msg)}}
}

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
	return func(yield func(model.Event, error) bool) {
		resp, err := p.send(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var (
			finish  model.Finish
			ended   bool
			stopped bool
			ierr    error
		)
		err = sse.Read(resp.Body, func(ev sse.Event) bool {
			if ev.Data == "[DONE]" {
				return false
			}
			var ch wireChunk
			if err := json.Unmarshal([]byte(ev.Data), &ch); err != nil {
				ierr = &model.Error{Class: model.ClassTransient, Err: fmt.Errorf("openai: bad chunk: %w", err)}
				return false
			}
			if len(ch.Error) > 0 && string(ch.Error) != "null" {
				e := apiError{Message: string(ch.Error)}
				_ = json.Unmarshal(ch.Error, &e)
				ierr = &model.Error{Class: classify(0, e, false), Err: fmt.Errorf("openai: %s", e.Message)}
				return false
			}
			if ch.Usage != nil {
				finish.Usage = model.Usage{Input: ch.Usage.PromptTokens, Output: ch.Usage.CompletionTokens,
					Cached: ch.Usage.PromptTokensDetails.CachedTokens, Reasoning: ch.Usage.CompletionTokensDetails.ReasoningTokens}
			}
			for _, c := range ch.Choices {
				if c.Delta.Content != "" && !yield(model.TextDelta{Text: c.Delta.Content}, nil) {
					stopped = true
					return false
				}
				for _, tc := range c.Delta.ToolCalls {
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					if !yield(model.ToolCallDelta{Index: idx, ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments}, nil) {
						stopped = true
						return false
					}
				}
				if c.FinishReason != "" {
					finish.Reason, ended = finishReason(c.FinishReason), true
				}
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
			err = &model.Error{Class: model.ClassTransient, Err: errors.New("openai: stream ended before the reply did")}
		}
		if err != nil {
			var e *model.Error
			errors.As(err, &e)
			e.Usage = finish.Usage // billed, whatever became of the reply
			yield(nil, e)
			return
		}
		yield(finish, nil)
	}
}

// StatusError is a non-2xx answer from the API.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("openai: HTTP %d: %s", e.Code, e.Body) }
