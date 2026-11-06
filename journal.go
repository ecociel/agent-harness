package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/ecociel/agent-harness/model"
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
//
// So a root's Wake is a floor, not a claim that it is alive. A tree is due at the
// latest of its root's Wake and the Wakes of whatever below it is working. A pattern
// only waits for its children, so its Lease need not outlast them: it says how soon
// to look, and a working child holds the sweep off by itself. A nested pattern does no
// work of its own, so it has no Wake at all and cannot hide a crash below it.
type Journal interface {
	// Append stores evs for runID, all or nothing. Seq in evs is consecutive and
	// starts one past the last stored event. If any position is already taken,
	// Append stores nothing and returns ErrConflict.
	Append(ctx context.Context, runID string, evs []Event) error
	// Load returns the run's events in Seq order, and none for an unknown run.
	Load(ctx context.Context, runID string) ([]Event, error)
}

// Lineage is what a run needs from a run above it, before its next step.
type Lineage struct {
	Parent    string // the run above this one, empty at the root
	Cancelled bool   // an event of this run says Event.IsCancel
	Reason    string // the Text of the first such event
}

// LineageReader is a Journal that can answer for one run without reading its events.
// It is optional. A nested run asks about each ancestor before every model call and
// tool start, and a plain Journal answers that with Load, so without this each step
// costs the length of every ancestor's history. A long-lived parent wants it. Project
// Lineage in the same transaction as Append: a stale answer is a cancel that goes
// unheard. A wrapper around a journal hides this method unless it forwards it, and
// runs then fall back to Load. An unknown run has the zero Lineage.
type LineageReader interface {
	Lineage(ctx context.Context, runID string) (Lineage, error)
}

// HistoryReader is a Journal that can answer for a finished run's final history
// without its caller walking the run's After chain one Load at a time. It is
// optional: without it, Prior folds each predecessor in turn. History returns what
// Prior would, and ErrNoSuchRun for a run that is unknown or not finished.
type HistoryReader interface {
	History(ctx context.Context, runID string) ([]model.Message, error)
}

// Prior returns the final history of a finished run: what a run started with After
// set to runID inherits. It never holds that run's Pinned context. A run that is
// unknown or not finished is ErrNoSuchRun.
func Prior(ctx context.Context, j Journal, runID string) ([]model.Message, error) {
	if h, ok := j.(HistoryReader); ok {
		return h.History(ctx, runID)
	}
	return prior(ctx, j, runID, map[string]bool{})
}

// prior walks the chain through Load. A chain that returns to a run it passed can
// only come from a damaged journal.
func prior(ctx context.Context, j Journal, runID string, seen map[string]bool) ([]model.Message, error) {
	if seen[runID] {
		return nil, fmt.Errorf("%w: the history of %s leads back to it", ErrIllegalTransition, runID)
	}
	seen[runID] = true
	evs, err := j.Load(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchRun, runID)
	}
	var base []model.Message
	if after := evs[0].After; after != "" {
		if base, err = prior(ctx, j, after, seen); err != nil {
			return nil, err
		}
	}
	r, err := FoldOn(base, evs)
	switch {
	case err != nil:
		return nil, err
	case !r.State.Terminal():
		return nil, fmt.Errorf("%w: %s has not finished", ErrNoSuchRun, runID)
	}
	return append([]model.Message{}, r.Messages...), nil
}

// lineage asks j for one run, through Load if j has no faster way.
func lineage(ctx context.Context, j Journal, runID string) (Lineage, error) {
	if r, ok := j.(LineageReader); ok {
		return r.Lineage(ctx, runID)
	}
	evs, err := j.Load(ctx, runID)
	if err != nil || len(evs) == 0 {
		return Lineage{}, err
	}
	l := Lineage{Parent: evs[0].ParentRun}
	if i := slices.IndexFunc(evs, Event.IsCancel); i >= 0 {
		l.Cancelled, l.Reason = true, evs[i].Text
	}
	return l, nil
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
	// run that has no journal and was given no Messages to start from, or to start
	// one whose After names no finished run.
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
