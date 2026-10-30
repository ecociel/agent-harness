package multi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ecociel/agent-harness"
)

// TaskStatus is the lifecycle of a board task.
type TaskStatus string

// Task statuses.
const (
	TaskOpen    TaskStatus = "open"
	TaskClaimed TaskStatus = "claimed"
	TaskDone    TaskStatus = "done"
	TaskFailed  TaskStatus = "failed"
)

// BoardTask is one unit of work on a Board.
type BoardTask struct {
	ID      int        `json:"id"`
	Text    string     `json:"text"`
	Status  TaskStatus `json:"status"`
	Owner   string     `json:"owner,omitempty"`
	Result  string     `json:"result,omitempty"`
	Version int        `json:"version"`
}

// ErrConflict is returned by Claim when the task changed since it was read
// (optimistic concurrency: re-read and try another task).
var ErrConflict = errors.New("board: version conflict")

// Board is shared, versioned task state. Agents coordinate through it
// instead of calling each other. Safe for concurrent use.
type Board struct {
	mu    sync.Mutex
	cond  *sync.Cond
	tasks []BoardTask
}

// NewBoard returns an empty board.
func NewBoard() *Board {
	b := &Board{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Add appends an open task and returns its id.
func (b *Board) Add(text string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := len(b.tasks) + 1
	b.tasks = append(b.tasks, BoardTask{ID: id, Text: text, Status: TaskOpen, Version: 1})
	b.cond.Broadcast()
	return id
}

// Snapshot returns a copy of all tasks.
func (b *Board) Snapshot() []BoardTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BoardTask(nil), b.tasks...)
}

// Claim takes task id for owner if it is still open at version.
func (b *Board) Claim(id, version int, owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := &b.tasks[id-1]
	if t.Version != version || t.Status != TaskOpen {
		return ErrConflict
	}
	t.Status, t.Owner = TaskClaimed, owner
	t.Version++
	return nil
}

// Finish records the outcome of a claimed task.
func (b *Board) Finish(id int, owner, result string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := &b.tasks[id-1]
	if t.Owner != owner {
		return
	}
	t.Status, t.Result = TaskDone, result
	if !ok {
		t.Status = TaskFailed
	}
	t.Version++
	b.cond.Broadcast()
}

// next blocks until an open task exists (returned) or no work can appear
// any more (ok=false: nothing open and nothing in progress).
func (b *Board) next(ctx context.Context) (BoardTask, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return BoardTask{}, false
		}
		busy := false
		for _, t := range b.tasks {
			switch t.Status {
			case TaskOpen:
				return t, true
			case TaskClaimed:
				busy = true
			}
		}
		if !busy {
			return BoardTask{}, false
		}
		b.cond.Wait()
	}
}

// Team is a shared-state team: long-lived members pull tasks from a
// Board, claim them with optimistic concurrency, post results, and may add
// follow-up tasks. Nobody is in charge; the run ends when the work list is
// empty and nobody is busy.
//
//	member A ─┐  claim / post / add  ┌─ member B
//	          └──────▶  Board  ◀─────┘
//
// Cursor found that flat self-coordination makes agents risk-averse at
// scale, which is why PlannerWorker exists; Team is still the right shape for
// a small pool of peers working through a queue.
type Team struct {
	Name    string
	Members []harness.Runner
	// Seed returns the initial tasks; nil uses the request as one task.
	Seed func(in harness.Input) []string
	// MaxTasks caps the total number of tasks, including added ones
	// (default 12).
	MaxTasks int
	// Board, if set, is used (and left populated) instead of a fresh one.
	Board *Board
}

// RunnerName implements harness.Named.
func (tm *Team) RunnerName() string { return tm.Name }

// Run implements harness.Runner.
func (tm *Team) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "team", tm.Name)
	board := tm.Board
	if board == nil {
		board = NewBoard()
	}
	seed := []string{in.Text()}
	if tm.Seed != nil {
		seed = tm.Seed(in)
	}
	for _, t := range seed {
		board.Add(t)
	}
	maxTasks := def(tm.MaxTasks, 12)
	// Wake members blocked on the board if the run is cancelled.
	stop := context.AfterFunc(s.ctx, func() {
		board.mu.Lock()
		board.cond.Broadcast()
		board.mu.Unlock()
	})
	defer stop()
	var wg sync.WaitGroup
	for i, m := range tm.Members {
		name := fmt.Sprintf("%s#%d", harness.NameOf(m), i+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				t, ok := board.next(s.ctx)
				if !ok {
					return
				}
				if err := board.Claim(t.ID, t.Version, name); err != nil {
					s.emit("conflict", fmt.Sprintf("%s lost task %d", name, t.ID), nil)
					continue
				}
				s.emit("claim", fmt.Sprintf("%s claimed %d: %s", name, t.ID, t.Text), map[string]any{"task": t.ID})
				res, err := m.Run(s.ctx, harness.Prompt(teamPrompt(in.Text(), t, board.Snapshot())))
				s.add(res)
				if err != nil || !res.OK() {
					why := "stopped"
					if err != nil {
						why = err.Error()
					}
					board.Finish(t.ID, name, why, false)
					continue
				}
				for _, nt := range newTasks(res.Output) {
					if len(board.Snapshot()) < maxTasks {
						id := board.Add(nt)
						s.emit("add", fmt.Sprintf("%s added %d: %s", name, id, nt), nil)
					}
				}
				board.Finish(t.ID, name, res.Output, true)
				s.emit("post", fmt.Sprintf("%s finished %d", name, t.ID), map[string]any{"task": t.ID})
			}
		}()
	}
	wg.Wait()
	var b strings.Builder
	failed := 0
	for _, t := range board.Snapshot() {
		if t.Status == TaskFailed {
			failed++
		}
		fmt.Fprintf(&b, "## Task %d (%s, %s): %s\n%s\n\n", t.ID, t.Status, t.Owner, t.Text, strings.TrimSpace(t.Result))
	}
	if failed > 0 {
		return s.done(in, strings.TrimSpace(b.String()), harness.StopFailed, fmt.Sprintf("%d tasks failed", failed), nil)
	}
	return s.ok(in, strings.TrimSpace(b.String()))
}

func teamPrompt(goal string, t BoardTask, board []BoardTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team goal:\n%s\n\nYour task (#%d):\n%s\n\nBoard:\n", goal, t.ID, t.Text)
	for _, x := range board {
		fmt.Fprintf(&b, "- #%d [%s] %s\n", x.ID, x.Status, x.Text)
	}
	b.WriteString("\nDo your task. If you find follow-up work not on the board, add lines 'NEW TASK: <task>'.")
	return b.String()
}

func newTasks(out string) []string {
	var ts []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if len(l) > 9 && strings.EqualFold(l[:9], "NEW TASK:") {
			ts = append(ts, strings.TrimSpace(l[9:]))
		}
	}
	return ts
}
