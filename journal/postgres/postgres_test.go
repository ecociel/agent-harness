package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/journal/postgres"
	"github.com/ecociel/agent-harness/journaltest"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/tool"
)

func open(t *testing.T) *postgres.Journal {
	t.Helper()
	j, _ := openDB(t)
	return j
}

func openDB(t *testing.T) (*postgres.Journal, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("JOURNAL_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("JOURNAL_POSTGRES_DSN is not set, so nothing here would run")
		}
		t.Skip("set JOURNAL_POSTGRES_DSN to run against a real database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	j := postgres.New(db)
	if err := j.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return j, db
}

func TestConformance(t *testing.T) {
	journaltest.Run(t, func(t *testing.T) harness.Journal { return open(t) })
}

func TestMigrateCanRunTwice(t *testing.T) {
	if err := open(t).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

var errDead = errors.New("process died")

// dies stops accepting writes after n appends across all runs, like a process that
// was killed.
type dies struct {
	harness.Journal
	left int
}

func (d *dies) Append(ctx context.Context, id string, evs []harness.Event) error {
	if d.left <= 0 {
		return errDead
	}
	d.left--
	return d.Journal.Append(ctx, id, evs)
}

func script(toolName string) model.Provider {
	return model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		if last := req.Messages[len(req.Messages)-1]; last.Role == model.RoleTool {
			return model.Stream(model.Response{Text: "done", Usage: model.Usage{Input: 1, Output: 1}})
		}
		return model.Stream(model.Response{Usage: model.Usage{Input: 1, Output: 1},
			ToolCalls: []model.ToolCall{{ID: "c1", Name: toolName, Arguments: json.RawMessage(`{}`)}}})
	})
}

func agent(j harness.Journal, lease time.Duration, name string) *harness.Agent {
	read := tool.Func(tool.Spec{Name: "look", Effect: tool.EffectRead},
		func(context.Context, json.RawMessage) (string, error) { return "seen", nil })
	return &harness.Agent{Name: name, Journal: j, Lease: lease, Provider: script("look"), Tools: tool.NewRegistry(read)}
}

func due(t *testing.T, j *postgres.Journal, at time.Time) []string {
	t.Helper()
	ids, err := j.Due(context.Background(), at)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	return ids
}

func TestDueFindsACrashedWorkerAndLetsAFinishedRunGo(t *testing.T) {
	j := open(t)
	id := newID("crashed")
	in := harness.Prompt("go")
	in.RunID = id
	if _, err := agent(&dies{Journal: j, left: 2}, time.Minute, "a").Run(context.Background(), in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a drained worker, got %v", err)
	}
	now := time.Now()
	if slices.Contains(due(t, j, now), id) {
		t.Fatal("a run inside its lease is not due")
	}
	if !slices.Contains(due(t, j, now.Add(2*time.Minute)), id) {
		t.Fatal("a crashed worker's run must be due once its lease passes")
	}
	res, err := agent(j, time.Minute, "a").Run(context.Background(), harness.Resume(id))
	if err != nil || !res.OK() {
		t.Fatalf("the sweep's resume: %v %+v", err, res)
	}
	if slices.Contains(due(t, j, now.Add(24*time.Hour)), id) {
		t.Fatal("a finished run is never due")
	}
}

func TestDueIgnoresARunOnlyASignalCanMove(t *testing.T) {
	j := open(t)
	id := newID("parked")
	gate := gateFunc(func(harness.Call) harness.Decision { return harness.Await(harness.Wait{Kind: "approval"}) })
	a := agent(j, time.Minute, "a")
	a.Hooks = []harness.Hook{gate}
	in := harness.Prompt("go")
	in.RunID = id
	res, err := a.Run(context.Background(), in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("want a suspended run: %v %+v", err, res)
	}
	if slices.Contains(due(t, j, time.Now().Add(1000*time.Hour)), id) {
		t.Fatal("a run with no expiry waits for its signal, so no sweep may touch it")
	}
}

type gateFunc func(harness.Call) harness.Decision

func (gateFunc) Name() string { return "gate" }
func (g gateFunc) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	return g(c)
}

// ordered parks branch 0 before branch 1 writes anything, then kills branch 1 after
// its first two appends, so the tree is always in the state under test.
type ordered struct {
	harness.Journal
	root   string
	parked chan struct{}
	once   sync.Once

	mu     sync.Mutex
	writes int
}

func (o *ordered) Append(ctx context.Context, id string, evs []harness.Event) error {
	if id == o.root+"/1" {
		select {
		case <-o.parked:
		case <-time.After(10 * time.Second):
			return errors.New("branch 0 never parked")
		}
		o.mu.Lock()
		o.writes++
		dead := o.writes > 2
		o.mu.Unlock()
		if dead {
			return errDead
		}
	}
	err := o.Journal.Append(ctx, id, evs)
	if err == nil && id == o.root+"/0" && evs[len(evs)-1].To == harness.StateSuspended {
		o.once.Do(func() { close(o.parked) })
	}
	return err
}

// A parked branch must not hide a sibling that crashed.
func TestDueFindsACrashedBranchBesideAParkedOne(t *testing.T) {
	j := open(t)
	id := newID("tree")
	tree := &ordered{Journal: j, root: id, parked: make(chan struct{})}
	parked := agent(tree, time.Minute, "parked")
	parked.Hooks = []harness.Hook{gateFunc(func(harness.Call) harness.Decision {
		return harness.Await(harness.Wait{Kind: "approval", TTL: 24 * time.Hour})
	})}
	in := harness.Prompt("go")
	in.RunID = id
	p := &multi.Parallel{Name: "tree", Journal: tree, Lease: time.Minute,
		Branches: []harness.Runner{parked, agent(tree, time.Minute, "crashes")}}
	if _, err := p.Run(context.Background(), in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a drained worker, got %v", err)
	}
	if tree.writes <= 2 {
		t.Fatalf("branch 1 was not killed: %d writes", tree.writes)
	}
	// The consumer's Lease on the pattern decides, well inside DefaultLease.
	now := time.Now()
	if slices.Contains(due(t, j, now), id) {
		t.Fatal("a tree is not due inside its root's lease")
	}
	found := due(t, j, now.Add(2*time.Minute))
	if !slices.Contains(found, id) {
		t.Fatal("the crashed branch was not found on the pattern's lease")
	}
	for _, run := range found {
		if strings.HasPrefix(run, id+"/") {
			t.Fatalf("a sweep must not resume child %s: its parent does that", run)
		}
	}
}

// A pattern between the root and the crash does no work, so its own lease must not
// read as a sign of life. Its default would otherwise hide the crash for ten minutes.
func TestDueSeesThroughANestedPattern(t *testing.T) {
	j := open(t)
	id := newID("nested")
	in := harness.Prompt("go")
	in.RunID = id
	crashing := &dies{Journal: j, left: 4} // both patterns start, then the agent writes twice
	p := &multi.Sequential{Name: "root", Journal: crashing, Lease: time.Minute, Steps: []harness.Runner{
		&multi.Sequential{Name: "inner", Steps: []harness.Runner{agent(nil, time.Minute, "crashes")}},
	}}
	if _, err := p.Run(context.Background(), in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a drained worker, got %v", err)
	}
	if !slices.Contains(due(t, j, time.Now().Add(2*time.Minute)), id) {
		t.Fatal("a nested pattern's lease hid the crash below it")
	}
}

// A root's lease is a floor. A branch still inside its own lease is working, so a
// short lease on the pattern cannot fire over it.
func TestDueLeavesAWorkingBranchAloneOnAShortPatternLease(t *testing.T) {
	j := open(t)
	id := newID("working")
	in := harness.Prompt("go")
	in.RunID = id
	crashing := &dies{Journal: j, left: 3}
	p := &multi.Parallel{Name: "root", Journal: crashing, Lease: time.Minute,
		Branches: []harness.Runner{agent(nil, time.Hour, "slow")}}
	if _, err := p.Run(context.Background(), in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a drained worker, got %v", err)
	}
	now := time.Now()
	if slices.Contains(due(t, j, now.Add(30*time.Minute)), id) {
		t.Fatal("a branch inside its own lease is working, so its tree is not due")
	}
	if !slices.Contains(due(t, j, now.Add(2*time.Hour)), id) {
		t.Fatal("a tree whose branch went quiet must be found")
	}
}

func TestDuplicateAppendAcrossConnectionsIsAConflict(t *testing.T) {
	a, b := open(t), open(t) // two pools, as two processes would have
	id := newID("two-processes")
	ev := harness.Event{Seq: 1, Time: time.Now(), Kind: harness.KindHistoryEdited}
	if err := a.Append(context.Background(), id, []harness.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(context.Background(), id, []harness.Event{ev}); !errors.Is(err, harness.ErrConflict) {
		t.Fatalf("want ErrConflict from the other process, got %v", err)
	}
}

// effects is a table an atomic tool writes in the journal's transaction.
func effects(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS journal_test_effects (key text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
}

func effectCount(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM journal_test_effects WHERE key = $1`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func write(ctx context.Context, key string) error {
	tx, ok := postgres.TxFrom(ctx)
	if !ok {
		return errors.New("no unit on the context")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO journal_test_effects (key) VALUES ($1)`, key)
	return err
}

func TestAtomicallyCommitsTheEffectWithItsEvents(t *testing.T) {
	j, db := openDB(t)
	effects(t, db)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		outcome string
		taken   bool
		effect  int
		events  int
		err     error
	}{
		{"ok", "ok", false, 1, 1, nil},
		{"a tool error keeps its record and loses its writes", "error", false, 0, 1, nil},
		{"a fenced unit loses both", "ok", true, 0, 1, harness.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, key := newID("unit"), newID("key")
			if tc.taken {
				if err := j.Append(ctx, id, []harness.Event{{Seq: 1, Time: time.Now(), Kind: harness.KindRunClaimed}}); err != nil {
					t.Fatal(err)
				}
			}
			err := j.Atomically(ctx, id, func(ctx context.Context) ([]harness.Event, error) {
				if err := write(ctx, key); err != nil {
					return nil, err
				}
				return []harness.Event{{Seq: 1, Time: time.Now(), Kind: harness.KindToolDone, Outcome: tc.outcome}}, nil
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("want %v, got %v", tc.err, err)
			}
			if n := effectCount(t, db, key); n != tc.effect {
				t.Fatalf("%d effects", n)
			}
			evs, err := j.Load(ctx, id)
			if err != nil || len(evs) != tc.events {
				t.Fatalf("%v: %d events", err, len(evs))
			}
		})
	}
}
