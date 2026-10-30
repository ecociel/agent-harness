package harness_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func ExampleAgent_Run() {
	weather := tool.Func(tool.Spec{Name: "weather", Description: "Weather for a city.", Effect: tool.EffectRead},
		func(_ context.Context, _ json.RawMessage) (string, error) {
			return `{"city":"Zurich","temp_c":11}`, nil
		})
	agent := &harness.Agent{
		Instructions: "Answer briefly.",
		Provider: fake.Script(
			fake.Call("weather", map[string]string{"city": "Zurich"}),
			fake.Text("It is 11 °C in Zurich."),
		),
		Tools: tool.NewRegistry(weather),
	}
	res, err := agent.Run(context.Background(), harness.Prompt("Weather in Zurich?"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Output)
	fmt.Println(res.Stop, res.Path)
	// Output:
	// It is 11 °C in Zurich.
	// completed [start compose model gate tools compose model verify closing done]
}

// pgJournal is a whole durable store. CREATE TABLE journal (run_id text, seq bigint,
// kind text, at timestamptz, doc jsonb, PRIMARY KEY (run_id, seq)).
type pgJournal struct{ db *sql.DB }

func (j pgJournal) Append(ctx context.Context, run string, evs []harness.Event) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range evs {
		doc, err := json.Marshal(e)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO journal VALUES ($1, $2, $3, $4, $5)`, run, e.Seq, e.Kind, e.Time, doc)
		if isUniqueViolation(err) {
			return harness.ErrConflict
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (j pgJournal) Load(ctx context.Context, run string) ([]harness.Event, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT doc FROM journal WHERE run_id = $1 ORDER BY seq`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []harness.Event
	for rows.Next() {
		var doc []byte
		var e harness.Event
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(doc, &e); err != nil {
			return nil, err
		}
		evs = append(evs, e)
	}
	return evs, rows.Err()
}

func isUniqueViolation(error) bool { return false }

// worker is what a queue consumer runs. Redelivery is safe: the same call converges.
func worker(ctx context.Context, agent *harness.Agent, orderID string) error {
	in := harness.Prompt("Refund order " + orderID)
	in.RunID = "refund-" + orderID
	res, err := agent.Run(ctx, in)
	switch {
	case errors.Is(err, harness.ErrFenced):
		return nil // another worker owns it now
	case errors.Is(err, harness.ErrDrained):
		return err // requeue: any process can resume it
	case err != nil:
		return err
	case res.State == harness.StateSuspended:
		return notify(ctx, res.ID, res.Waits)
	}
	return nil
}

func notify(context.Context, string, []harness.Wait) error { return nil }

// approve runs in a different process from the worker. It builds the same Agent.
func approve(agent *harness.Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithoutCancel(r.Context()) // the run outlives this request
		res, err := agent.Run(ctx, harness.Resume(r.PathValue("run"), harness.Signal{
			Wait: r.PathValue("wait"), Value: r.FormValue("decision"), By: r.Header.Get("X-User"),
		}))
		switch {
		case errors.Is(err, harness.ErrStaleSignal):
			http.Error(w, "this request is no longer open", http.StatusConflict)
		case err != nil:
			http.Error(w, "try again", http.StatusServiceUnavailable)
		default:
			_ = json.NewEncoder(w).Encode(res)
		}
	}
}

// redactAHV is a complete redaction hook.
type redactAHV struct{}

var ahv = regexp.MustCompile(`756\.\d{4}\.\d{4}\.\d{2}`)

func (redactAHV) Name() string { return "redact-ahv" }
func (redactAHV) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return ahv.ReplaceAllString(s, "[AHV]"), nil
}

// inspect answers "what is this run doing?" for a dashboard.
func inspect(ctx context.Context, j harness.Journal, id string) (string, error) {
	evs, err := j.Load(ctx, id)
	if err != nil {
		return "", err
	}
	r, err := harness.Fold(evs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s since %s, %d waits, %d claims", r.State, r.Updated, len(r.Waits), r.Claims), nil
}

var (
	_                  = worker
	_                  = approve
	_                  = inspect
	_ harness.Journal  = pgJournal{}
	_ harness.Redactor = redactAHV{}
)
