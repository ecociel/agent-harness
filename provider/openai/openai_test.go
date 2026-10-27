package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/openai"
)

func TestStreamingToolCallAndUsage(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"choices":[{"delta":{"content":"Let me "}}]}`,
			`{"choices":[{"delta":{"content":"check."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"clock","arguments":"{\"tz\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"CET\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := &openai.Provider{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"}
	req := model.Request{Messages: []model.Message{model.System("s"), model.User("time?"),
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "x", Name: "clock", Arguments: json.RawMessage(`{}`)}}},
		model.ToolResult("x", "clock", "12:00")},
		Tools: []model.ToolSpec{{Name: "clock", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	resp, err := model.Collect(p.Complete(context.Background(), req), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Let me check." || len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Arguments) != `{"tz":"CET"}` {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage.Input != 11 || resp.Usage.Output != 7 || resp.Finish != model.FinishToolCalls {
		t.Fatalf("usage/finish = %+v %s", resp.Usage, resp.Finish)
	}
	msgs := got["messages"].([]any)
	asst := msgs[2].(map[string]any)
	if asst["content"] != nil || asst["tool_calls"] == nil || got["model"] != "m" || got["stream"] != true {
		t.Fatalf("encoded request = %v", got)
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"rate"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := model.Collect((&openai.Provider{BaseURL: srv.URL}).Complete(context.Background(), model.Request{}), nil)
	var se *openai.StatusError
	if !errors.As(err, &se) || se.Code != 429 {
		t.Fatalf("err = %v", err)
	}
}
