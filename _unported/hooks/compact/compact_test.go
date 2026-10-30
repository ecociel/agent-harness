package compact_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/compact"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

func TestCompactsLongHistory(t *testing.T) {
	var hist []model.Message
	for i := 0; i < 20; i++ {
		hist = append(hist, model.User(strings.Repeat("question ", 20)), model.Assistant(strings.Repeat("answer ", 20)))
	}
	hist = append(hist, model.User("latest"))
	p := fake.Script(fake.Text("ok"))
	a := &harness.Agent{Instructions: "sys", Provider: p,
		Hooks: []harness.Hook{&compact.Compactor{MaxTokens: 200, KeepLast: 3}}}
	res, err := a.Run(context.Background(), harness.Input{Messages: hist})
	if err != nil {
		t.Fatal(err)
	}
	sent := p.Requests()[0].Messages
	if sent[0].Role != model.RoleSystem || !strings.HasPrefix(sent[1].Content, "[Summary") {
		t.Fatalf("request starts %v / %q", sent[0].Role, sent[1].Content)
	}
	if len(sent) > 6 || sent[len(sent)-1].Content != "latest" {
		t.Fatalf("sent %d messages", len(sent))
	}
	if len(res.Messages) >= len(hist) {
		t.Fatal("compaction did not persist into the run history")
	}
}
