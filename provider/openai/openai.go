// Package openai is a model.Provider for any OpenAI-compatible
// /chat/completions endpoint, with streaming and tool calls.
package openai

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

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
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
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func encode(req model.Request, def string) wireRequest {
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
	return w
}

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
	return func(yield func(model.Event, error) bool) {
		body, err := json.Marshal(encode(req, p.Model))
		if err != nil {
			yield(nil, err)
			return
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield(nil, err)
			return
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
			stopped bool
			ierr    error
		)
		err = sse.Read(resp.Body, func(ev sse.Event) bool {
			if ev.Data == "[DONE]" {
				return false
			}
			var ch wireChunk
			if err := json.Unmarshal([]byte(ev.Data), &ch); err != nil {
				ierr = fmt.Errorf("openai: bad chunk: %w", err)
				return false
			}
			if ch.Error != nil {
				ierr = fmt.Errorf("openai: %s", ch.Error.Message)
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
					finish.Reason = model.FinishReason(c.FinishReason)
				}
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

// StatusError is a non-2xx answer from the API.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("openai: HTTP %d: %s", e.Code, e.Body) }
