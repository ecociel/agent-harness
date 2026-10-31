package offload_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/offload"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func big(text string) tool.Tool {
	return tool.Func(tool.Spec{Name: "dump", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		return text, nil
	})
}

// handleFrom reads the id back out of the handle, the way the model does.
func reader(t *testing.T, store offload.Store, text string) model.Provider {
	t.Helper()
	return fake.NewFunc(func(req model.Request) (model.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role != model.RoleTool {
			return fake.Call("dump", nil), nil
		}
		if last.Name == offload.ReaderName || strings.HasPrefix(last.Content, "x") {
			return fake.Text("read: " + last.Content), nil
		}
		var h struct {
			Offloaded string `json:"offloaded"`
		}
		if err := json.Unmarshal([]byte(last.Content), &h); err != nil || h.Offloaded == "" {
			t.Errorf("not a handle: %.80q", last.Content)
			return fake.Text("none"), nil
		}
		return fake.Call(offload.ReaderName, map[string]any{"id": h.Offloaded, "offset": len(text) - 10}), nil
	})
}

func TestLargeResultIsOffloadedAndReadable(t *testing.T) {
	store := offload.NewMemStore()
	text := strings.Repeat("x", 10000)
	a := &harness.Agent{Provider: reader(t, store, text), Tools: tool.NewRegistry(big(text), offload.Reader(store)),
		Hooks: []harness.Hook{offload.New(store, 1000)}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Output != "read: "+strings.Repeat("x", 10) {
		t.Fatalf("%+v %v", res, err)
	}
	if got := res.Messages[2].Content; len(got) > 600 || !strings.Contains(got, `"offloaded"`) {
		t.Fatalf("inline result = %d bytes", len(got))
	}
}

func TestASmallResultStaysInline(t *testing.T) {
	store := offload.NewMemStore()
	a := &harness.Agent{Provider: fake.Script(fake.Call("dump", nil), fake.Text("ok")), Tools: tool.NewRegistry(big("short")),
		Hooks: []harness.Hook{offload.New(store, 1000)}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Messages[2].Content != "short" {
		t.Fatalf("%v %+v", err, res)
	}
}

// durable stands in for a store outside the process, and counts its keys.
type durable struct{ offload.Store }

// A handle in the journal must still read after the process that wrote it is gone, and
// a repeated call must find the same id.
func TestAHandleSurvivesARestartAndARepeatStoresOverItself(t *testing.T) {
	store := durable{offload.NewMemStore()}
	text := strings.Repeat("x", 10000)
	j := harness.NewMemJournal()
	build := func() *harness.Agent {
		return &harness.Agent{Name: "a", Journal: j, Provider: reader(t, store, text), Tools: tool.NewRegistry(big(text), offload.Reader(store)),
			Hooks: []harness.Hook{offload.New(store, 1000)}}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	ctx, cancel := context.WithCancel(context.Background())
	stop := func(e harness.Event) {
		if e.Kind == harness.KindToolDone {
			cancel()
		}
	}
	if _, err := build().Run(harness.WithEvents(ctx, stop), in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	res, err := build().Run(context.Background(), harness.Resume("r"))
	if err != nil || res.Output != "read: "+strings.Repeat("x", 10) {
		t.Fatalf("%v %+v", err, res)
	}
}

type failing struct{ offload.Store }

func (failing) Put(context.Context, string, string) error { return errors.New("store down") }

func TestAStoreThatFailsLeavesTheResultInline(t *testing.T) {
	text := strings.Repeat("x", 2000)
	a := &harness.Agent{Provider: fake.Script(fake.Call("dump", nil), fake.Text("ok")), Tools: tool.NewRegistry(big(text)),
		Hooks: []harness.Hook{offload.New(failing{offload.NewMemStore()}, 1000)}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Messages[2].Content != text {
		t.Fatalf("%v", err)
	}
}

func TestReaderBounds(t *testing.T) {
	store := offload.NewMemStore()
	if err := store.Put(context.Background(), "id", "0123456789"); err != nil {
		t.Fatal(err)
	}
	r := offload.Reader(store)
	for _, c := range []struct{ args, want string }{
		{`{"id":"id"}`, "0123456789"},
		{`{"id":"id","offset":8}`, "89"},
		{`{"id":"id","offset":3,"length":2}`, "34"},
		{`{"id":"id","offset":99}`, ""},
		{`{"id":"id","offset":-5,"length":1}`, "0"},
	} {
		if got, err := r.Call(context.Background(), json.RawMessage(c.args)); err != nil || got != c.want {
			t.Errorf("%s: %q %v", c.args, got, err)
		}
	}
	if _, err := r.Call(context.Background(), json.RawMessage(`{"id":"nope"}`)); err == nil {
		t.Fatal("want an error for an unknown id")
	}
}

type scrub struct{}

func (scrub) Name() string { return "scrub" }
func (scrub) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, "secret", "[gone]"), nil
}

// What reaches a store has passed the ring, so a store is one more place that never
// holds the value.
func TestAStoreNeverSeesWhatARedactorRemoved(t *testing.T) {
	store := offload.NewMemStore()
	text := "secret " + strings.Repeat("x", 2000)
	var id string
	p := fake.NewFunc(func(req model.Request) (model.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleTool {
			var h struct {
				Offloaded string `json:"offloaded"`
			}
			_ = json.Unmarshal([]byte(last.Content), &h)
			id = h.Offloaded
			return fake.Text("ok"), nil
		}
		return fake.Call("dump", nil), nil
	})
	a := &harness.Agent{Provider: p, Tools: tool.NewRegistry(big(text)), Hooks: []harness.Hook{offload.New(store, 1000), scrub{}}}
	if _, err := a.Run(context.Background(), harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), id)
	if err != nil || !ok || strings.Contains(got, "secret") {
		t.Fatalf("%v %v %.40q", err, ok, got)
	}
}

// Offloading what the reader returns would hand the model a handle for every page.
func TestAReaderResultIsNotOffloadedAgain(t *testing.T) {
	store := offload.NewMemStore()
	if err := store.Put(context.Background(), "id", strings.Repeat("y", 5000)); err != nil {
		t.Fatal(err)
	}
	a := &harness.Agent{Provider: fake.Script(fake.Call(offload.ReaderName, map[string]any{"id": "id", "length": 5000}), fake.Text("ok")),
		Tools: tool.NewRegistry(offload.Reader(store)), Hooks: []harness.Hook{offload.New(store, 1000)}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Messages[2].Content != strings.Repeat("y", 5000) {
		t.Fatalf("%v", err)
	}
}
