package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/tool"
)

// unitJournal is an AtomicJournal whose unit buffers the tool's writes and
// applies them only when the events land, like a transaction. die makes the next
// unit fail before its commit, as a process killed inside it would.
type unitJournal struct {
	harness.Journal
	w     *world
	units int
	die   bool
}

type unitKey struct{}

func (u *unitJournal) Atomically(ctx context.Context, id string, fn func(context.Context) ([]harness.Event, error)) error {
	u.units++
	var writes []string
	evs, err := fn(context.WithValue(ctx, unitKey{}, &writes))
	if err != nil {
		return err
	}
	if u.die {
		u.die = false
		return errCrash
	}
	if err := u.Journal.Append(ctx, id, evs); err != nil {
		return err
	}
	for _, key := range writes {
		u.w.ran("book", key)
	}
	return nil
}

func booking(fail bool) tool.Tool {
	return tool.Func(tool.Spec{Name: "book", Effect: tool.EffectAtomic}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		writes := ctx.Value(unitKey{}).(*[]string)
		*writes = append(*writes, tool.IdempotencyKey(ctx))
		if fail {
			return "", errors.New("no seats left")
		}
		return "booked", nil
	})
}

func TestAnAtomicToolNeedsAnAtomicJournal(t *testing.T) {
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: struct{ harness.Journal }{j}, Provider: newWorld().provider("book"), Tools: tool.NewRegistry(booking(false))}
	in := harness.Prompt("book it")
	in.RunID = "plain"
	if _, err := a.Run(context.Background(), in); !errors.Is(err, harness.ErrNotAtomic) {
		t.Fatalf("want ErrNotAtomic, got %v", err)
	}
	if evs := mustLoad(t, j, "plain"); len(evs) != 0 {
		t.Fatalf("a refused start wrote %d events", len(evs))
	}
}

func TestAnAtomicCallCommitsItsEffectWithItsResult(t *testing.T) {
	w := newWorld()
	j := &unitJournal{Journal: harness.NewMemJournal(), w: w}
	a := &harness.Agent{Journal: j, Provider: w.provider("book"), Tools: tool.NewRegistry(booking(false))}
	in := harness.Prompt("book it")
	in.RunID = "unit"
	res, err := a.Run(context.Background(), in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if j.units != 1 || len(w.effect["book"]) != 1 {
		t.Fatalf("units %d, effects %v", j.units, w.effect["book"])
	}
	var kinds []harness.Kind
	for _, e := range mustLoad(t, j, "unit") {
		if e.Tool == "book" {
			kinds = append(kinds, e.Kind)
		}
	}
	if want := []harness.Kind{harness.KindToolGated, harness.KindToolStarted, harness.KindToolDone}; !equalKinds(kinds, want) {
		t.Fatalf("book recorded %v", kinds)
	}
}

func equalKinds(a, b []harness.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A crash inside the unit undoes the effect with the record, so the call runs again
// under the same key and asks nobody.
func TestALostAtomicCallRunsAgainUnderTheSameKey(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	j := &unitJournal{Journal: harness.NewMemJournal(), w: w, die: true}
	a := &harness.Agent{Journal: j, Provider: w.provider("book"), Tools: tool.NewRegistry(booking(false))}
	in := harness.Prompt("book it")
	in.RunID = "lost"
	if _, err := a.Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a drained worker, got %v", err)
	}
	if n := len(w.effect["book"]); n != 0 {
		t.Fatalf("an effect outlived its unit: %d", n)
	}
	res, err := a.Run(ctx, harness.Resume("lost"))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	var attempts []int
	lost := false
	for _, e := range mustLoad(t, j, "lost") {
		switch e.Kind {
		case harness.KindToolStarted:
			attempts = append(attempts, e.Attempt)
		case harness.KindToolLost:
			lost = true
		}
	}
	if !lost || len(attempts) != 2 || attempts[1] != 2 {
		t.Fatalf("tool.lost %v, attempts %v", lost, attempts)
	}
	if len(w.effect["book"]) != 1 {
		t.Fatalf("effects %v", w.effect["book"])
	}
}

// A tool error is a result, not a failed unit: the error text is recorded, and the
// unit is the store's to undo.
func TestAnAtomicToolErrorIsRecordedAsItsResult(t *testing.T) {
	w := newWorld()
	j := &unitJournal{Journal: harness.NewMemJournal(), w: w}
	a := &harness.Agent{Journal: j, Provider: w.provider("book"), Tools: tool.NewRegistry(booking(true))}
	in := harness.Prompt("book it")
	in.RunID = "error"
	res, err := a.Run(context.Background(), in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	for _, e := range mustLoad(t, j, "error") {
		if e.Kind == harness.KindToolDone && (e.Outcome != "error" || e.Text != "error: no seats left") {
			t.Fatalf("tool.done %q %q", e.Outcome, e.Text)
		}
	}
}

// fencedUnit refuses the unit as a store does when another worker took the position.
type fencedUnit struct{ harness.Journal }

func (f fencedUnit) Atomically(ctx context.Context, _ string, fn func(context.Context) ([]harness.Event, error)) error {
	if _, err := fn(context.WithValue(ctx, unitKey{}, new([]string))); err != nil {
		return err
	}
	return harness.ErrConflict
}

func TestAFencedUnitStopsTheWorker(t *testing.T) {
	w := newWorld()
	a := &harness.Agent{Journal: fencedUnit{harness.NewMemJournal()}, Provider: w.provider("book"), Tools: tool.NewRegistry(booking(false))}
	if _, err := a.Run(context.Background(), harness.Prompt("book it")); !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("want ErrFenced, got %v", err)
	}
}
