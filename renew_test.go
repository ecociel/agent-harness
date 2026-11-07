package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// renewals records every renewal and answers with refuse.
type renewals struct {
	harness.Journal
	mu     sync.Mutex
	heads  []int64
	until  []time.Time
	refuse error
}

func (r *renewals) Renew(_ context.Context, _ string, head int64, until time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heads, r.until = append(r.heads, head), append(r.until, until)
	return r.refuse
}

func slowAgent(j harness.Journal, lease time.Duration) *harness.Agent {
	slow := tool.Func(tool.Spec{Name: "slow", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		time.Sleep(120 * time.Millisecond)
		return "slow ok", nil
	})
	w := newWorld()
	inner := w.provider("slow")
	p := model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		time.Sleep(120 * time.Millisecond)
		return inner.Complete(ctx, req)
	})
	return &harness.Agent{Journal: j, Lease: lease, Provider: p, Tools: tool.NewRegistry(slow)}
}

func TestASlowStepRenewsItsLeaseOnTheHeadItWrote(t *testing.T) {
	j := &renewals{Journal: harness.NewMemJournal()}
	in := harness.Prompt("go")
	in.RunID = "renew"
	start := time.Now()
	if res, err := slowAgent(j, 90*time.Millisecond).Run(context.Background(), in); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	intents := map[int64]harness.Kind{}
	for _, e := range mustLoad(t, j, "renew") {
		if e.Kind == harness.KindModelStarted || e.Kind == harness.KindToolStarted {
			intents[e.Seq] = e.Kind
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	seen := map[harness.Kind]bool{}
	for i, h := range j.heads {
		kind, ok := intents[h]
		if !ok {
			t.Fatalf("renewal %d named head %d, which is no intent this worker wrote", i, h)
		}
		seen[kind] = true
		if lead := j.until[i].Sub(start); lead < 90*time.Millisecond {
			t.Fatalf("renewal %d extends only %v past the start", i, lead)
		}
	}
	if !seen[harness.KindModelStarted] || !seen[harness.KindToolStarted] {
		t.Fatalf("renewed during %v, want a model call and a tool", seen)
	}
}

func TestARefusedRenewalStopsTheWorkerAtItsNextAppend(t *testing.T) {
	j := &renewals{Journal: harness.NewMemJournal(), refuse: harness.ErrFenced}
	in := harness.Prompt("go")
	in.RunID = "refused"
	if _, err := slowAgent(j, 90*time.Millisecond).Run(context.Background(), in); !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("want ErrFenced, got %v", err)
	}
	evs := mustLoad(t, j, "refused")
	if last := evs[len(evs)-1]; last.Kind != harness.KindModelStarted {
		t.Fatalf("the fenced worker wrote %s after its intent", last.Kind)
	}
}
