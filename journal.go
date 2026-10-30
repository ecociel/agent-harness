package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Journal is the one durable thing: an append-only log of events per run. It is
// all a consumer implements to make runs durable.
//
// A unique index on (runID, Seq) is the whole contract. It is the ownership
// fence, because two workers cannot record the same position. It is also the
// outbox, because a store can project usage or status in the same transaction as
// Append.
//
// One projection replaces a third method. The last event of every append carries
// Wake: when this run next needs someone to call Run, or zero if nobody needs to,
// because it ended or only a signal can move it. A running run's Wake is its Lease
// past that append, so a crashed worker and a drained one look the same. A parked
// run's Wake is its earliest expiry. Project Wake and the event's To into a row per
// run and one query finds everything that needs a worker:
//
//	-- a root is due when its own Wake passed and nothing below it is working
//	SELECT r.run FROM runs r WHERE r.parent = '' AND r.wake <= $now
//	  AND NOT EXISTS (SELECT 1 FROM runs c WHERE c.root = r.run
//	                  AND c.wake > $now AND c.state <> 'suspended')
//
// Children are never swept: their parent resumes them. The NOT EXISTS is what lets
// a root sit quiet for hours while a child works, and its state test is what stops
// a child parked until next week from hiding a sibling that crashed.
type Journal interface {
	// Append stores evs for runID, all or nothing. Seq in evs is consecutive and
	// starts one past the last stored event. If any position is already taken,
	// Append stores nothing and returns ErrConflict.
	Append(ctx context.Context, runID string, evs []Event) error
	// Load returns the run's events in Seq order, and none for an unknown run.
	Load(ctx context.Context, runID string) ([]Event, error)
}

var (
	// ErrConflict is what a Journal returns when a position is already taken.
	ErrConflict = errors.New("harness: journal position taken")
	// ErrDrained is what Run returns when this worker stopped before the run did:
	// a drain, a cancelled context or a journal failure. Any process may resume the
	// run. A tool that starts a run passes it up unchanged, so a stopped child stops
	// its parent instead of reading as a tool error.
	ErrDrained = errors.New("harness: worker stopped, run is resumable")
	// ErrFenced is what Run returns when another worker took a run this worker was
	// advancing. The fenced worker has recorded nothing there since. In a tree each
	// run has its own fence, so two live workers can each hold part of it and both
	// stop. The tree is then owned by nobody until the next Run, so a sweep is not
	// optional for trees.
	ErrFenced = errors.New("harness: run is owned by another worker")
	// ErrStaleSignal is what Run returns, writing nothing, for a signal that
	// names no wait of this run or arrives after the wait expired.
	ErrStaleSignal = errors.New("harness: signal does not match an open wait")
	// ErrNoSuchRun is what Run returns, writing nothing, when asked to continue a
	// run that has no journal and was given no Messages to start from.
	ErrNoSuchRun = errors.New("harness: no such run")
	// ErrNotDurable ends a run that must suspend but has no Journal to resume
	// from.
	ErrNotDurable = errors.New("harness: suspending needs Agent.Journal")
)

type memJournal struct {
	mu   sync.Mutex
	runs map[string][]Event
}

// NewMemJournal returns a Journal in memory, for tests and for suspending within
// one process. Safe for concurrent use.
func NewMemJournal() Journal { return &memJournal{runs: map[string][]Event{}} }

func (j *memJournal) Append(_ context.Context, runID string, evs []Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(evs) == 0 {
		return nil
	}
	if evs[0].Seq != int64(len(j.runs[runID]))+1 {
		return fmt.Errorf("%w: %s at %d", ErrConflict, runID, evs[0].Seq)
	}
	j.runs[runID] = append(j.runs[runID], evs...)
	return nil
}

func (j *memJournal) Load(_ context.Context, runID string) ([]Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Event(nil), j.runs[runID]...), nil
}
