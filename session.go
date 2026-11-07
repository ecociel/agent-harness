package harness

import (
	"context"
	"fmt"
	"sync"

	"github.com/ecociel/agent-harness/model"
)

// Session is a conversation of chained runs. Each Send starts "<ID>/<Turn>" with
// After "<ID>/<Turn-1>", so the journal stores each message once and holds the
// whole conversation: there is no history to save. The fields are exported so a
// consumer can save them anywhere and rebuild the struct.
//
// Turn only moves when a run finishes, so a Send repeated after a crash resumes
// that run instead of paying for it twice, and a Session over a durable Runner
// survives a restart with no extra machinery. A Runner with no journal of its own,
// and none from its context, chains through one in memory that lives as long as
// the Session value, so a Session rebuilt from its fields cannot continue it.
type Session struct {
	ID     string
	Runner Runner
	Turn   int

	mu  sync.Mutex
	mem Journal
}

// NewSession returns an empty session over r.
func NewSession(r Runner) *Session { return &Session{ID: newRunID("session"), Runner: r} }

// Send starts or continues this turn's run. The turn moves on when the run reaches
// a terminal state. A suspended or drained run leaves the session where it is: call
// Resume, or Send again.
func (s *Session) Send(ctx context.Context, text string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := Input{Messages: []model.Message{model.User(text)}}
	if s.Turn > 0 {
		in.After = s.runID(s.Turn - 1)
	}
	return s.advance(ctx, in)
}

// Resume delivers signals to this turn's suspended run.
func (s *Session) Resume(ctx context.Context, signals ...Signal) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.advance(ctx, Input{Signals: signals})
}

func (s *Session) runID(turn int) string { return fmt.Sprintf("%s/%d", s.ID, turn) }

func (s *Session) advance(ctx context.Context, in Input) (*Run, error) {
	if _, ok := ctx.Value(keyJournal).(Journal); !ok {
		if s.mem == nil {
			s.mem = NewMemJournal()
		}
		ctx = context.WithValue(ctx, keyJournal, s.mem)
	}
	in.RunID = s.runID(s.Turn)
	res, err := s.Runner.Run(ctx, in)
	if err != nil || !res.State.Terminal() {
		return res, err
	}
	s.Turn++
	return res, nil
}
