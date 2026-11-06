package compact_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/compact"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func longHistory() []model.Message {
	var hist []model.Message
	for range 20 {
		hist = append(hist, model.User(strings.Repeat("question ", 20)), model.Assistant(strings.Repeat("answer ", 20)))
	}
	return append(hist, model.User("latest"))
}

func TestCompactsLongHistory(t *testing.T) {
	hist := longHistory()
	p := fake.Script(fake.Text("ok"))
	a := &harness.Agent{Instructions: "sys", Provider: p,
		Hooks: []harness.Hook{&compact.Compactor{MaxTokens: 200, KeepLast: 3}}}
	res, err := a.Run(context.Background(), harness.Input{Messages: hist})
	if err != nil {
		t.Fatal(err)
	}
	sent := p.Requests()[0].Messages
	if sent[0].Role != model.RoleSystem || !strings.HasPrefix(sent[1].Content, "[Summary") || sent[1].Source != "compact" {
		t.Fatalf("request starts %v / %+v", sent[0].Role, sent[1])
	}
	if len(sent) != 5 || sent[len(sent)-1].Content != "latest" {
		t.Fatalf("sent %d messages", len(sent))
	}
	if len(res.Messages) >= len(hist) {
		t.Fatal("compaction did not persist into the run history")
	}
}

func TestAShortHistoryIsLeftAlone(t *testing.T) {
	p := fake.Script(fake.Text("ok"))
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{&compact.Compactor{MaxTokens: 1 << 20}}}
	if _, err := a.Run(context.Background(), harness.Input{Messages: longHistory()}); err != nil {
		t.Fatal(err)
	}
	if n := len(p.Requests()[0].Messages); n != 41 {
		t.Fatalf("%d messages", n)
	}
}

func look() tool.Tool {
	return tool.Func(tool.Spec{Name: "look", Effect: tool.EffectRead}, func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("seen ", 100), nil
	})
}

// A long agent turn has one user message, so a cut that needs a user message would
// never fire. The cut must still not part a call from its result, which the core would
// refuse by failing the run.
func TestALongToolLoopCompactsWithoutSplittingARound(t *testing.T) {
	var script []model.Response
	for range 8 {
		script = append(script, model.Response{ToolCalls: []model.ToolCall{
			{ID: "a", Name: "look", Arguments: json.RawMessage(`{}`)}, {ID: "b", Name: "look", Arguments: json.RawMessage(`{}`)}}})
	}
	p := fake.Script(append(script, fake.Text("done"))...)
	a := &harness.Agent{Provider: p, MaxTurns: 20, Tools: tool.NewRegistry(look()),
		Hooks: []harness.Hook{&compact.Compactor{MaxTokens: 300, KeepLast: 2}}}
	res, err := a.Run(context.Background(), harness.Prompt("go"))
	if err != nil || res.Output != "done" {
		t.Fatalf("%v %+v", err, res)
	}
	for _, m := range res.Messages[:1] {
		if !strings.HasPrefix(m.Content, "[Summary") {
			t.Fatalf("no compaction: %+v", m)
		}
	}
}

// Worth a real recording: the summary is paid for once, however often workers crash.
func TestASummaryIsComputedOnce(t *testing.T) {
	calls := 0
	h := &compact.Compactor{MaxTokens: 200, KeepLast: 3, Summarize: func(context.Context, []model.Message) (string, error) {
		calls++
		return "the gist", nil
	}}
	j := harness.NewMemJournal()
	dying := fake.NewFunc(func(model.Request) (model.Response, error) {
		return model.Response{}, errors.New("no reply")
	})
	in := harness.Input{RunID: "r", Messages: longHistory()}
	a := &harness.Agent{Name: "a", Journal: j, Provider: dying, Hooks: []harness.Hook{h}}
	if _, err := a.Run(context.Background(), in); err == nil {
		t.Fatal("want a failure")
	}
	if calls != 1 {
		t.Fatalf("%d summaries", calls)
	}
	evs, err := j.Load(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs, func(e harness.Event) bool { return e.Kind == harness.KindHistoryEdited && e.Hook == "compact" }) {
		t.Fatal("the edit was not recorded")
	}
}

func TestAFailedSummaryKeepsTheHistory(t *testing.T) {
	p := fake.Script(fake.Text("ok"))
	h := &compact.Compactor{MaxTokens: 200, Summarize: func(context.Context, []model.Message) (string, error) {
		return "", errors.New("summariser down")
	}}
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{h}}
	res, err := a.Run(context.Background(), harness.Input{Messages: longHistory()})
	if err != nil || !res.OK() || len(p.Requests()[0].Messages) != 41 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestAFullContextCompactsAndRetriesOnce(t *testing.T) {
	n := 0
	p := fake.NewFunc(func(req model.Request) (model.Response, error) {
		n++
		if n == 1 || len(req.Messages) > 30 {
			return model.Response{}, &model.Error{Class: model.ClassContextLength, Err: errors.New("too long")}
		}
		return fake.Text("fits now"), nil
	})
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{&compact.Compactor{}}}
	res, err := a.Run(context.Background(), harness.Input{Messages: longHistory()})
	if err != nil || res.Output != "fits now" || n != 2 {
		t.Fatalf("%v %+v n=%d", err, res, n)
	}

	stuck := fake.NewFunc(func(model.Request) (model.Response, error) {
		n++
		return model.Response{}, &model.Error{Class: model.ClassContextLength, Err: errors.New("too long")}
	})
	n = 0
	a = &harness.Agent{Provider: stuck, Hooks: []harness.Hook{&compact.Compactor{}}}
	if _, err := a.Run(context.Background(), harness.Input{Messages: longHistory()}); err == nil || n != 2 {
		t.Fatalf("%v n=%d", err, n)
	}
}

// The kept turns' provider state depended on the history just replaced. Left in place
// it would earn a refused request on every later turn.
func TestKeptTurnsLoseTheirProviderState(t *testing.T) {
	hist := longHistory()
	hist[39].Opaque = json.RawMessage(`{"anthropic":[]}`)
	hist[1].Opaque = json.RawMessage(`{"anthropic":[]}`)
	p := fake.Script(fake.Text("ok"))
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{&compact.Compactor{MaxTokens: 200, KeepLast: 3}}}
	if _, err := a.Run(context.Background(), harness.Input{Messages: hist}); err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Requests()[0].Messages {
		if len(m.Opaque) != 0 {
			t.Fatalf("stale state went out: %+v", m)
		}
	}
}

func TestModelSummarizer(t *testing.T) {
	p := fake.Script(fake.Text("short"))
	got, err := compact.ModelSummarizer(p, "cheap")(context.Background(), []model.Message{model.User("long")})
	if err != nil || got != "short" || p.Requests()[0].Model != "cheap" {
		t.Fatalf("%v %q", err, got)
	}
}

func TestDigestKeepsTextValid(t *testing.T) {
	got, _ := compact.Digest(context.Background(), []model.Message{model.User(strings.Repeat("ä", 60))})
	if strings.ToValidUTF8(got, "") != got {
		t.Fatalf("%q", got)
	}
}

// A full context says the estimate was wrong, so a large KeepLast cannot stand in the
// way of making room.
func TestAFullContextTakesAtLeastHalf(t *testing.T) {
	n := 0
	p := fake.NewFunc(func(req model.Request) (model.Response, error) {
		if n++; n == 1 {
			return model.Response{}, &model.Error{Class: model.ClassContextLength, Err: errors.New("too long")}
		}
		if len(req.Messages) > 22 {
			t.Errorf("%d messages after compaction", len(req.Messages))
		}
		return fake.Text("ok"), nil
	})
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{&compact.Compactor{KeepLast: 40}}}
	if res, err := a.Run(context.Background(), harness.Input{Messages: longHistory()}); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
}

// A run that continues a conversation and carries pinned context compacts only what
// it inherited, because an edit across the pinned context fails the run.
func TestAChainedRunWithPinnedContextCompactsItsInheritance(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	first := &harness.Agent{Journal: j, Provider: fake.Script(fake.Text("ok"))}
	in := harness.Input{RunID: "c/0", Messages: longHistory()}
	if _, err := first.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	p := fake.Script(fake.Call("look", nil), fake.Call("look", nil), fake.Text("done"))
	a := &harness.Agent{Journal: j, Provider: p, Tools: tool.NewRegistry(look()),
		Hooks: []harness.Hook{&compact.Compactor{MaxTokens: 50, KeepLast: 1}}}
	in = harness.Prompt(strings.Repeat("next ", 100))
	in.RunID, in.After, in.Pinned = "c/1", "c/0", []model.Message{model.User("pinned")}
	res, err := a.Run(ctx, in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	for i, req := range p.Requests() {
		if sent := req.Messages; !strings.HasPrefix(sent[0].Content, "[Summary") || sent[1].Content != "pinned" {
			t.Fatalf("request %d starts %q, %q", i, sent[0].Content, sent[1].Content)
		}
	}
}
