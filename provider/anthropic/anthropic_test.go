package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/anthropic"
)

func TestStreamingToolUse(t *testing.T) {
	var got struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad", 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		for _, e := range []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":20}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"clock"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":\"CET\"}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = fmt.Fprintf(w, "event: x\ndata: %s\n\n", e)
		}
	}))
	defer srv.Close()
	p := &anthropic.Provider{BaseURL: srv.URL, APIKey: "k", Model: "claude"}
	req := model.Request{Messages: []model.Message{model.System("be brief"), model.User("time?"),
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a", Name: "clock"}, {ID: "b", Name: "clock"}}},
		model.ToolResult("a", "clock", "1"), model.ToolResult("b", "clock", "2")}}
	resp, err := model.Collect(p.Complete(context.Background(), req), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Checking" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "tu1" ||
		string(resp.ToolCalls[0].Arguments) != `{"tz":"CET"}` || resp.Finish != model.FinishToolCalls {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage.Input != 20 || resp.Usage.Output != 9 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if got.System != "be brief" || len(got.Messages) != 3 || len(got.Messages[2].Content) != 2 || got.MaxTokens != 4096 {
		t.Fatalf("encoded = %+v", got)
	}
}
