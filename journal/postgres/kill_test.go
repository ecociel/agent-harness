package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/journal/postgres"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/tool"
)

// The world outside a run, in the same database so a killed process can't take it
// along: what the tools really did and what the provider really billed.
const world = `
CREATE TABLE IF NOT EXISTS kill_effects (root text NOT NULL, tool text NOT NULL, key text NOT NULL, pid int NOT NULL);
CREATE TABLE IF NOT EXISTS kill_shipped (root text NOT NULL, key text NOT NULL, PRIMARY KEY (root, key));
CREATE TABLE IF NOT EXISTS kill_billed (root text NOT NULL);
`

const (
	rootEnv = "AGENT_HARNESS_KILL_ROOT"
	once    = "pay_a"
	onceToo = "pay_c"
)

// worker is what the killed process does: it drives one run to its end. Everything it
// knows comes from the journal and the outside world, so it is the same code on a first
// start and on the tenth restart. It stands in for the operator too, and can be killed
// while doing that.
func worker(root string) int {
	ctx := context.Background()
	db, err := sql.Open("pgx", os.Getenv("JOURNAL_POSTGRES_DSN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer db.Close()
	r := tree(db, root)
	in := harness.Prompt("the task")
	in.RunID = root
	res, err := r.Run(ctx, in)
	for err == nil && !res.State.Terminal() {
		var signals []harness.Signal
		for _, w := range res.Waits {
			name := strings.TrimSuffix(strings.TrimPrefix(w.Detail, "the outcome of "), " is unknown")
			if err = operator(ctx, db, root, name); err != nil {
				break
			}
			signals = append(signals, harness.Signal{Wait: w.ID, Value: name + " ok", By: "op"})
		}
		if err == nil {
			res, err = r.Run(ctx, harness.Resume(root, signals...))
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Printf("%s\n%s\n", res.Stop, res.Output)
	return 0
}

// operator is a person answering an unknown outcome: they look at the outside world,
// and finish the job by hand if it never happened.
func operator(ctx context.Context, db *sql.DB, root, name string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO kill_effects (root, tool, key, pid)
		SELECT $1, $2, 'by hand', $3 WHERE NOT EXISTS (SELECT 1 FROM kill_effects WHERE root = $1 AND tool = $2)`,
		root, name, os.Getpid())
	return err
}

func tree(db *sql.DB, root string) harness.Runner {
	agent := func(name, toolName string, spec tool.Spec) *harness.Agent {
		spec.Name = toolName
		act := tool.Func(spec, func(ctx context.Context, _ json.RawMessage) (string, error) {
			var err error
			if spec.Effect == tool.EffectOnce {
				_, err = db.ExecContext(ctx, `INSERT INTO kill_effects (root, tool, key, pid) VALUES ($1, $2, $3, $4)`,
					root, toolName, tool.IdempotencyKey(ctx), os.Getpid())
			} else {
				_, err = db.ExecContext(ctx, `INSERT INTO kill_shipped (root, key) VALUES ($1, $2) ON CONFLICT DO NOTHING`, root, tool.IdempotencyKey(ctx))
			}
			if err != nil {
				return "", err
			}
			time.Sleep(3 * time.Millisecond) // the window between the effect and its record
			return toolName + " ok", nil
		})
		return &harness.Agent{Name: name, Tools: tool.NewRegistry(act), Provider: billing(db, root, name, toolName)}
	}
	return &multi.Parallel{Name: "fan", Journal: postgres.New(db), Lease: time.Minute, Branches: []harness.Runner{
		agent("a", once, tool.Spec{Effect: tool.EffectOnce}),
		&multi.Sequential{Name: "pipe", Steps: []harness.Runner{
			agent("b", "ship", tool.Spec{Effect: tool.EffectIdempotent}),
			agent("c", onceToo, tool.Spec{Effect: tool.EffectOnce}),
		}},
	}}
}

// billing is a model that bills before it answers, as a real one does. Its answer
// quotes what it was told, so a lost result changes the output.
func billing(db *sql.DB, root, agent, toolName string) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		if _, err := db.ExecContext(ctx, `INSERT INTO kill_billed (root) VALUES ($1)`, root); err != nil {
			return model.Fail(err)
		}
		time.Sleep(2 * time.Millisecond)
		usage := model.Usage{Input: 10, Output: 1}
		for _, m := range req.Messages {
			if m.Role == model.RoleTool {
				return model.Stream(model.Response{Usage: usage, Text: fmt.Sprintf("%s got %q", agent, m.Content)})
			}
		}
		return model.Stream(model.Response{Usage: usage,
			ToolCalls: []model.ToolCall{{ID: "c1", Name: toolName, Arguments: json.RawMessage(`{}`)}}})
	})
}

func TestMain(m *testing.M) {
	if root := os.Getenv(rootEnv); root != "" {
		os.Exit(worker(root))
	}
	os.Exit(m.Run())
}

func spawn(root string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), rootEnv+"="+root)
	return cmd
}

// A process SIGKILLed at random points, restarted from the run ID alone, again and
// again, until the run ends. Nothing here runs in the dying process's favour: no defer,
// no flush, no ctx. The crash sweeps kill the journal at every append, and this kills
// the process, at points nobody chose.
func TestKillTheWorkerAtRandomPoints(t *testing.T) {
	j := open(t)
	db, err := sql.Open("pgx", os.Getenv("JOURNAL_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, world); err != nil {
		t.Fatal(err)
	}
	rounds := 40
	if v, err := strconv.Atoi(os.Getenv("AGENT_HARNESS_KILL_ROUNDS")); err == nil {
		rounds = v
	}
	seed := uint64(time.Now().UnixNano())
	if v, err := strconv.ParseUint(os.Getenv("AGENT_HARNESS_KILL_SEED"), 10, 64); err == nil {
		seed = v
	}
	t.Logf("seed %d, %d rounds", seed, rounds)
	rng := rand.New(rand.NewPCG(seed, 1))

	clean := newID("kill-clean")
	start := time.Now()
	out, err := spawn(clean).Output()
	if err != nil {
		t.Fatalf("uncrashed run: %v\n%s", err, out)
	}
	whole := time.Since(start)
	t.Logf("an uncrashed run takes %v", whole)

	kills, midRun, unknown, lost := 0, 0, 0, 0
	for round := range rounds {
		root := newID("kill")
		var last []byte
		finished := false
		for attempt := 0; attempt < 300 && !finished; attempt++ {
			cmd := spawn(root)
			stdout := new(strings.Builder)
			cmd.Stdout = stdout
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			died := make(chan error, 1)
			go func() { died <- cmd.Wait() }()
			select {
			case err := <-died:
				if err != nil {
					t.Fatalf("round %d: a worker failed by itself: %v", round, err)
				}
				last, finished = []byte(stdout.String()), true
			case <-time.After(time.Duration(rng.Int64N(int64(whole) * 3 / 2))):
				if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Fatal(err)
				}
				if err := <-died; err == nil {
					last, finished = []byte(stdout.String()), true
				} else {
					kills++
					if parked(t, ctx, j, root) {
						midRun++
					}
				}
			}
		}
		if !finished {
			t.Fatalf("round %d: did not finish in 300 restarts", round)
		}
		if string(last) != string(out) {
			t.Errorf("round %d: output\n%s\nwant\n%s", round, last, out)
		}
		u, l := audit(t, ctx, db, j, root)
		unknown += u
		lost += l
	}
	t.Logf("%d kills, %d mid-run, %d runs reached an unknown outcome, %d lost model calls", kills, midRun, unknown, lost)
	if midRun == 0 || unknown == 0 || lost == 0 {
		t.Fatalf("the kills did not test anything: %d mid-run, %d unknown, %d lost", midRun, unknown, lost)
	}
	if t.Failed() {
		return
	}
	if err := cleanUp(ctx, db); err != nil {
		t.Log(err)
	}
}

// parked reports whether a kill left a run that has started and has not ended.
func parked(t *testing.T, ctx context.Context, j *postgres.Journal, root string) bool {
	t.Helper()
	evs, err := j.Load(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.Fold(evs)
	if err != nil {
		t.Fatalf("a kill left a journal that does not fold: %v", err)
	}
	return len(evs) > 0 && !r.State.Terminal()
}

// audit holds a finished tree to the outside world.
func audit(t *testing.T, ctx context.Context, db *sql.DB, j *postgres.Journal, root string) (unknown, lost int) {
	t.Helper()
	for _, name := range []string{once, onceToo} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM kill_effects WHERE root = $1 AND tool = $2`, root, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s: once-only tool %s took effect %d times", root, name, n)
		}
	}
	var billed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM kill_billed WHERE root = $1`, root).Scan(&billed); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT run_id FROM journal_runs WHERE root = $1`, root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 5 {
		t.Errorf("%s: %d runs in the tree: %v", root, len(ids), ids)
	}
	calls := 0
	for _, id := range ids {
		evs, err := j.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r, err := harness.Fold(evs)
		if err != nil {
			t.Fatal(err)
		}
		if !r.State.Terminal() {
			t.Errorf("%s left in %s", id, r.State)
		}
		calls += r.ModelCalls
		lost += r.LostCalls
		for _, e := range evs {
			if e.Kind == harness.KindToolLost && (e.Tool == once || e.Tool == onceToo) {
				unknown++
			}
		}
	}
	if calls > billed || billed > calls+lost {
		t.Errorf("%s: billed %d outside [%d, %d]", root, billed, calls, calls+lost)
	}
	if found := due(t, j, time.Now().Add(24*time.Hour)); slices.Contains(found, root) {
		t.Errorf("%s: a finished run is due", root)
	}
	return unknown, lost
}

func cleanUp(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `DROP TABLE kill_effects; DROP TABLE kill_shipped; DROP TABLE kill_billed`)
	return err
}
