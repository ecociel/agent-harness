// Package postgres is a harness.Journal on PostgreSQL. It uses database/sql and
// imports no driver, so the application chooses one.
//
// The event document is stored as bytea, not jsonb or text: a tool's output can
// hold a NUL byte, which those types refuse, and the journal must not lose a run to
// what a tool printed.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ecociel/agent-harness"
)

const schema = `
CREATE TABLE IF NOT EXISTS journal (
	run_id text        NOT NULL,
	seq    bigint      NOT NULL,
	kind   text        NOT NULL,
	at     timestamptz NOT NULL,
	doc    bytea       NOT NULL,
	PRIMARY KEY (run_id, seq)
);
CREATE TABLE IF NOT EXISTS journal_runs (
	run_id text PRIMARY KEY,
	parent text NOT NULL,
	root   text NOT NULL,
	state  text NOT NULL,
	wake   timestamptz
);
CREATE INDEX IF NOT EXISTS journal_runs_due ON journal_runs (wake) WHERE parent = '';
CREATE INDEX IF NOT EXISTS journal_runs_root ON journal_runs (root);
`

// Journal stores runs in the database. Safe for concurrent use.
type Journal struct {
	db *sql.DB
}

// New returns a journal on db. Call Migrate once before using it.
func New(db *sql.DB) *Journal { return &Journal{db: db} }

// Migrate creates the tables if they do not exist.
func (j *Journal) Migrate(ctx context.Context) error {
	_, err := j.db.ExecContext(ctx, schema)
	return err
}

// Append implements harness.Journal. The primary key is the fence. The projection
// row commits with the events, so it can never disagree with them.
func (j *Journal) Append(ctx context.Context, runID string, evs []harness.Event) error {
	if len(evs) == 0 {
		return nil
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range evs {
		doc, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("postgres: encode event %d: %w", e.Seq, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO journal (run_id, seq, kind, at, doc) VALUES ($1, $2, $3, $4, $5)`,
			runID, e.Seq, string(e.Kind), e.Time, doc)
		if isUniqueViolation(err) {
			return harness.ErrConflict
		}
		if err != nil {
			return err
		}
	}
	last := evs[len(evs)-1]
	var wake sql.NullTime
	if !last.Wake.IsZero() {
		wake = sql.NullTime{Time: last.Wake, Valid: true}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO journal_runs (run_id, parent, root, state, wake) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (run_id) DO UPDATE SET state = COALESCE(NULLIF(EXCLUDED.state, ''), journal_runs.state), wake = EXCLUDED.wake`,
		runID, last.ParentRun, last.RootRun, string(last.To), wake); err != nil {
		return err
	}
	return tx.Commit()
}

// Load implements harness.Journal.
func (j *Journal) Load(ctx context.Context, runID string) ([]harness.Event, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT doc FROM journal WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []harness.Event
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var e harness.Event
		if err := json.Unmarshal(doc, &e); err != nil {
			return nil, fmt.Errorf("postgres: decode %s event %d: %w", runID, len(out)+1, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Due returns the root runs that need a worker at now: crashed, drained, or with a
// timer or expiry that has passed. Call Run with harness.Resume for each. Children
// are never returned, because their parent resumes them.
//
// A root that is quiet while a child works is not due. A child parked until next
// week does not count as working, so it cannot hide a sibling that crashed.
func (j *Journal) Due(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT r.run_id FROM journal_runs r
		WHERE r.parent = '' AND r.wake <= $1
		AND NOT EXISTS (SELECT 1 FROM journal_runs c WHERE c.root = r.run_id AND c.run_id <> r.run_id
			AND c.wake > $1 AND c.state <> $2)
		ORDER BY r.wake`, now, string(harness.StateSuspended))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// isUniqueViolation reads SQLSTATE 23505. Both common drivers expose SQLState on
// their error type, so this needs neither of them.
func isUniqueViolation(err error) bool {
	var e interface{ SQLState() string }
	return errors.As(err, &e) && e.SQLState() == "23505"
}
