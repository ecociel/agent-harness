package harness_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

// say runs one turn of a chain to its end.
func say(t *testing.T, a *harness.Agent, id, after, text string, pinned ...model.Message) *harness.Run {
	t.Helper()
	in := harness.Prompt(text)
	in.RunID, in.After, in.Pinned = id, after, pinned
	res, err := a.Run(context.Background(), in)
	if err != nil || !res.OK() {
		t.Fatalf("%s: %v %+v", id, err, res)
	}
	return res
}

func contents(msgs []model.Message) string {
	var out []string
	for _, m := range msgs {
		out = append(out, string(m.Role)+":"+m.Content)
	}
	return strings.Join(out, " | ")
}

func TestAChainedRunContinuesItsPredecessorWithoutCopyingIt(t *testing.T) {
	j := newJSONJournal()
	rq := &requests{}
	a := &harness.Agent{Journal: j, Instructions: "be brief",
		Provider: rq.wrap(fake.Script(fake.Text("first answer"), fake.Text("second answer"), fake.Text("third answer")))}
	say(t, a, "c/0", "", "one")
	res := say(t, a, "c/1", "c/0", "two", model.User("pinned for two"))
	if res.After != "c/0" || res.Inherited != 2 {
		t.Fatalf("After %q Inherited %d", res.After, res.Inherited)
	}
	if got, want := contents(res.Messages), "user:one | assistant:first answer | user:two | assistant:second answer"; got != want {
		t.Fatalf("Messages %s\nwant %s", got, want)
	}
	if want := "[system:be brief user:one assistant:first answer user:pinned for two user:two]"; fmt.Sprint(rq.calls[1]) != want {
		t.Fatalf("request %v\nwant %s", rq.calls[1], want)
	}
	started := mustLoad(t, j, "c/1")[0]
	if started.After != "c/0" || len(started.Messages) != 1 {
		t.Fatalf("run.started copied the history: after %q, %d messages", started.After, len(started.Messages))
	}
	say(t, a, "c/2", "c/1", "three")
	if want := "[system:be brief user:one assistant:first answer user:two assistant:second answer user:three]"; fmt.Sprint(rq.calls[2]) != want {
		t.Fatalf("a successor saw %v\nwant %s", rq.calls[2], want)
	}
}

func TestAChainedRunFoldsOnlyOnItsBase(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: j, Provider: fake.Script(fake.Text("a"), fake.Text("b"))}
	say(t, a, "f/0", "", "one")
	say(t, a, "f/1", "f/0", "two")
	evs := mustLoad(t, j, "f/1")
	if _, err := harness.Fold(evs); !errors.Is(err, harness.ErrIllegalTransition) {
		t.Fatalf("a chained run folded without its base: %v", err)
	}
	base, err := harness.Prior(ctx, j, "f/0")
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.FoldOn(base, evs)
	if err != nil || contents(r.Messages) != "user:one | assistant:a | user:two | assistant:b" {
		t.Fatalf("%v %s", err, contents(r.Messages))
	}
}

func TestAfterMustNameAFinishedRun(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "parked"
	if res, err := approvalAgent(w, j, 0).Run(ctx, in); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	for _, after := range []string{"parked", "nowhere"} {
		in := harness.Prompt("next")
		in.RunID, in.After = "next-"+after, after
		if _, err := (&harness.Agent{Journal: j, Provider: w.provider()}).Run(ctx, in); !errors.Is(err, harness.ErrNoSuchRun) {
			t.Fatalf("After %s: want ErrNoSuchRun, got %v", after, err)
		}
		if evs := mustLoad(t, j, in.RunID); len(evs) != 0 {
			t.Fatalf("After %s: wrote %d events", after, len(evs))
		}
	}
}

// summarizeInherited replaces the inherited history with one message.
type summarizeInherited struct{}

func (summarizeInherited) Name() string { return "summarize" }
func (summarizeInherited) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	if r.Inherited < 2 {
		return harness.Edit{}, nil
	}
	return harness.Edit{To: r.Inherited, Source: "summarize", With: []model.Message{model.User("summary of before")}}, nil
}

func TestAnEditOfTheInheritedHistoryCarriesForward(t *testing.T) {
	j := harness.NewMemJournal()
	rq := &requests{}
	plain := &harness.Agent{Journal: j, Provider: rq.wrap(fake.Script(fake.Text("a"), fake.Text("b"), fake.Text("c")))}
	say(t, plain, "e/0", "", "one")
	compacting := &harness.Agent{Journal: j, Provider: plain.Provider, Hooks: []harness.Hook{summarizeInherited{}}}
	res := say(t, compacting, "e/1", "e/0", "two", model.User("pinned"))
	if res.Inherited != 1 || contents(res.Messages) != "user:summary of before | user:two | assistant:b" {
		t.Fatalf("Inherited %d, %s", res.Inherited, contents(res.Messages))
	}
	if want := "[user:summary of before user:pinned user:two]"; fmt.Sprint(rq.calls[1]) != want {
		t.Fatalf("request %v\nwant %s", rq.calls[1], want)
	}
	say(t, plain, "e/2", "e/1", "three")
	if want := "[user:summary of before user:two assistant:b user:three]"; fmt.Sprint(rq.calls[2]) != want {
		t.Fatalf("the successor inherited %v\nwant %s", rq.calls[2], want)
	}
}

// crossPinned summarises one message past the inherited history.
type crossPinned struct{}

func (crossPinned) Name() string { return "cross" }
func (crossPinned) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	return harness.Edit{To: r.Inherited + 1, With: []model.Message{model.User("summary")}}, nil
}

func TestAnEditAcrossThePinnedContextFailsTheRun(t *testing.T) {
	j := harness.NewMemJournal()
	plain := &harness.Agent{Journal: j, Provider: fake.Script(fake.Text("a"))}
	say(t, plain, "x/0", "", "one")
	a := &harness.Agent{Journal: j, Provider: fake.Script(fake.Text("b")), Hooks: []harness.Hook{crossPinned{}}}
	in := harness.Prompt("two")
	in.RunID, in.After, in.Pinned = "x/1", "x/0", []model.Message{model.User("pinned")}
	res, err := a.Run(context.Background(), in)
	if err == nil || res.State != harness.StateFailed || !strings.Contains(res.Detail, "pinned") {
		t.Fatalf("%v %+v", err, res)
	}
}

// historyJournal answers Prior itself and counts the predecessor loads it saves.
type historyJournal struct {
	harness.Journal
	histories atomic.Int64
	loads     atomic.Int64
}

func (h *historyJournal) Load(ctx context.Context, id string) ([]harness.Event, error) {
	h.loads.Add(1)
	return h.Journal.Load(ctx, id)
}

func (h *historyJournal) History(ctx context.Context, id string) ([]model.Message, error) {
	h.histories.Add(1)
	return harness.Prior(ctx, h.Journal, id)
}

func TestAHistoryReaderAnswersForThePredecessor(t *testing.T) {
	j := &historyJournal{Journal: harness.NewMemJournal()}
	a := &harness.Agent{Journal: j, Provider: fake.Script(fake.Text("a"), fake.Text("b"))}
	say(t, a, "h/0", "", "one")
	j.loads.Store(0)
	res := say(t, a, "h/1", "h/0", "two")
	if j.histories.Load() != 1 || j.loads.Load() != 1 {
		t.Fatalf("histories %d, loads %d: the predecessor was read through Load", j.histories.Load(), j.loads.Load())
	}
	if contents(res.Messages) != "user:one | assistant:a | user:two | assistant:b" {
		t.Fatal(contents(res.Messages))
	}
}

func TestASessionStoresEachMessageOnce(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	s := harness.NewSession(&harness.Agent{Journal: j, Provider: fake.Script(fake.Text("a"), fake.Text("b"), fake.Text("c"))})
	var res *harness.Run
	for _, text := range []string{"one", "two", "three"} {
		var err error
		if res, err = s.Send(ctx, text); err != nil {
			t.Fatal(err)
		}
	}
	if contents(res.Messages) != "user:one | assistant:a | user:two | assistant:b | user:three | assistant:c" {
		t.Fatal(contents(res.Messages))
	}
	for turn := range 3 {
		if n := len(mustLoad(t, j, fmt.Sprintf("%s/%d", s.ID, turn))[0].Messages); n != 1 {
			t.Fatalf("turn %d started with %d messages", turn, n)
		}
	}
}

func TestASessionWithoutAJournalStillChains(t *testing.T) {
	s := harness.NewSession(&harness.Agent{Provider: fake.Script(fake.Text("a"), fake.Text("b"))})
	if _, err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(context.Background(), "two")
	if err != nil || contents(res.Messages) != "user:one | assistant:a | user:two | assistant:b" {
		t.Fatalf("%v %s", err, contents(res.Messages))
	}
}
