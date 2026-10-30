package harness

import (
	"context"
	"encoding/json"
	"iter"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

const secret = "756.1234.5678.97"

// fill sets every string-like field reachable from v, stopping at journal
// identifiers and vocabulary, which carry no consumer text.
func fill(v reflect.Value, path string, visited *[]string) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), path, visited)
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				fill(v.Field(i), path+"."+f.Name, visited)
			}
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 { // json.RawMessage
			v.SetBytes([]byte(secret))
			*visited = append(*visited, path)
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), path+"[]", visited)
	case reflect.String:
		if v.Type() == reflect.TypeOf("") {
			v.SetString(secret)
			*visited = append(*visited, path)
		}
	}
}

// identifiers lists the strings a redactor deliberately does not see: run and
// call identity, tool and model names, and hook names. A new string field fails
// this test until someone decides which side it is on.
var identifiers = regexp.MustCompile(`\.(RunID|RootRun|ParentRun|Agent|Hook|Tool|Offered\[\]|Name|ID|ToolCallID|Source|Target|Key|Run|Kind|Wait|By|Model|Outcome)$|\.Tools\[\]$|\.Decision\.Wait\.`)

func TestRedactionCoversEveryString(t *testing.T) {
	var ev Event
	var paths []string
	fill(reflect.ValueOf(&ev).Elem(), "Event", &paths)
	ev.redact(func(s *string) {
		if !strings.Contains(*s, secret) {
			t.Errorf("visited twice: %q", *s)
		}
		*s = "[x]"
	})
	got := reflect.ValueOf(ev)
	for _, p := range paths {
		if identifiers.MatchString(p) {
			continue
		}
		if s := lookup(got, strings.TrimPrefix(p, "Event.")); strings.Contains(s, secret) {
			t.Errorf("%s reaches the journal unredacted", p)
		}
	}
}

func lookup(v reflect.Value, path string) string {
	for _, part := range strings.Split(path, ".") {
		idx := strings.HasSuffix(part, "[]")
		part = strings.TrimSuffix(part, "[]")
		for v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		v = v.FieldByName(part)
		if idx {
			v = v.Index(0)
		}
	}
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() == reflect.Slice {
		return string(v.Bytes())
	}
	return v.String()
}

type pii struct{ seen *[]Kind }

func (pii) Name() string { return "pii" }
func (p pii) Redact(_ context.Context, k Kind, s string) (string, error) {
	if p.seen != nil {
		*p.seen = append(*p.seen, k)
	}
	return strings.ReplaceAll(s, secret, "[AHV]"), nil
}

// Everything observable about a run: the store, the sinks, the hooks, the tools,
// the next request and the result.
func TestRedactorRunsBeforeEverything(t *testing.T) {
	ctx := context.Background()
	j := NewMemJournal()
	var sunk []Event
	ctx = WithEvents(ctx, func(e Event) { sunk = append(sunk, e) })
	var hookSaw, toolSaw, modelSaw strings.Builder
	leak := tool.Func(tool.Spec{Name: "leak"}, func(_ context.Context, args json.RawMessage) (string, error) {
		toolSaw.Write(args)
		return "customer " + secret, nil
	})
	spy := hookFunc{beforeModel: func(r Run, req *model.Request) {
		for _, m := range req.Messages {
			modelSaw.WriteString(m.Content)
		}
		b, _ := json.Marshal(r.Messages)
		hookSaw.Write(b)
	}}
	a := &Agent{Journal: j, Tools: tool.NewRegistry(leak), Hooks: []Hook{pii{}, spy},
		Provider: fake.Script(
			model.Response{Text: "looking up " + secret, ToolCalls: []model.ToolCall{{ID: "1", Name: "leak", Arguments: json.RawMessage(`{"ahv":"` + secret + `"}`)}}},
			fake.Text("found "+secret)),
	}
	res, err := a.Run(ctx, Input{RunID: "r", Messages: []model.Message{model.User("my AHV is " + secret)}})
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	stored, _ := j.Load(ctx, "r")
	for name, v := range map[string]any{"journal": stored, "sinks": sunk, "result": res} {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), secret) {
			t.Errorf("%s holds the raw value", name)
		}
	}
	for name, s := range map[string]string{"hook": hookSaw.String(), "tool": toolSaw.String(), "model": modelSaw.String()} {
		if strings.Contains(s, secret) {
			t.Errorf("%s saw the raw value", name)
		}
	}
	if !strings.Contains(toolSaw.String(), "[AHV]") {
		t.Errorf("tool saw %q", toolSaw.String())
	}
}

type hookFunc struct {
	beforeModel func(Run, *model.Request)
}

func (hookFunc) Name() string { return "spy" }
func (h hookFunc) BeforeModel(_ context.Context, r Run, req *model.Request) error {
	h.beforeModel(r, req)
	return nil
}

// A secret split across two chunks is invisible to a per-chunk filter. The ring
// redacts the whole reply, so no delta can carry a piece of it.
func TestDeltasNeverCarryRawTextEvenAcrossChunks(t *testing.T) {
	var deltas strings.Builder
	ctx := WithEvents(context.Background(), func(e Event) {
		if e.Kind == KindDelta {
			deltas.WriteString(e.Text)
		}
	})
	split := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			_ = yield(model.TextDelta{Text: "id " + secret[:7]}, nil) &&
				yield(model.TextDelta{Text: secret[7:] + " end"}, nil) &&
				yield(model.Finish{Reason: model.FinishStop}, nil)
		}
	})
	a := &Agent{Provider: split, Hooks: []Hook{pii{}}}
	res, err := a.Run(ctx, Prompt("x"))
	if err != nil || res.Output != "id [AHV] end" {
		t.Fatalf("%v %+v", err, res)
	}
	if deltas.String() != "id [AHV] end" {
		t.Fatalf("deltas %q", deltas.String())
	}
}

type breakingRedactor struct{}

func (breakingRedactor) Name() string { return "broken" }
func (breakingRedactor) Redact(context.Context, Kind, string) (string, error) {
	return "", context.DeadlineExceeded
}

func TestFailedRedactorNeverLeaksAndFailsTheRun(t *testing.T) {
	j := NewMemJournal()
	a := &Agent{Journal: j, Provider: fake.Script(fake.Text("x")), Hooks: []Hook{breakingRedactor{}}}
	_, err := a.Run(context.Background(), Input{RunID: "r", Messages: []model.Message{model.User(secret)}})
	if err == nil {
		t.Fatal("want an error")
	}
	stored, _ := j.Load(context.Background(), "r")
	b, _ := json.Marshal(stored)
	if strings.Contains(string(b), secret) {
		t.Fatal("raw value stored")
	}
}

type asker struct{}

func (asker) Name() string { return "asker" }
func (asker) BeforeTool(context.Context, Run, Call) Decision {
	return Await(Wait{Kind: "approval", Detail: "approve for " + secret})
}

func TestWaitDetailIsRedactedForSinksToo(t *testing.T) {
	var sunk []Event
	ctx := WithEvents(context.Background(), func(e Event) { sunk = append(sunk, e) })
	echo := tool.Func(tool.Spec{Name: "echo"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	a := &Agent{Journal: NewMemJournal(), Tools: tool.NewRegistry(echo), Hooks: []Hook{pii{}, asker{}},
		Provider: fake.Script(fake.Call("echo", nil))}
	res, err := a.Run(ctx, Prompt("x"))
	if err != nil || res.Waits[0].Detail != "approve for [AHV]" {
		t.Fatalf("%v %+v", err, res)
	}
	for _, e := range sunk {
		if e.Decision != nil && e.Decision.Wait != nil && strings.Contains(e.Decision.Wait.Detail, secret) {
			t.Fatal("a sink saw the raw wait detail")
		}
	}
}

// failsOn is a redactor that dies at one event, for the runs that have nowhere
// legal left to record a failure.
type failsOn struct{ kind Kind }

func (failsOn) Name() string { return "fails" }
func (f failsOn) Redact(_ context.Context, k Kind, s string) (string, error) {
	if k == f.kind {
		return "", context.DeadlineExceeded
	}
	return s, nil
}

func TestRedactorFailureAtAnyEventEnds(t *testing.T) {
	for _, k := range []Kind{KindRunStarted, KindModelDone, KindToolDone, KindOutputChecked, KindRunEnded} {
		echo := tool.Func(tool.Spec{Name: "echo"}, func(context.Context, json.RawMessage) (string, error) { return "x", nil })
		a := &Agent{Journal: NewMemJournal(), Tools: tool.NewRegistry(echo), Hooks: []Hook{failsOn{k}},
			Provider: fake.Script(fake.Call("echo", nil), fake.Text("done"))}
		if _, err := a.Run(context.Background(), Prompt("x")); err == nil {
			t.Errorf("%s: want an error", k)
		}
	}
}
