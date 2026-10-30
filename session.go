package harness

import (
	"context"
	"fmt"
	"sync"

	"github.com/ecociel/agent-harness/model"
)

// Session is a conversation across runs. A run is workflow state and lives in the
// Journal; a session is only the history that carries from one run to the next.
// Its fields are exported so a consumer can save them anywhere and rebuild the
// struct: there is no store to implement.
//
// Each Send runs under the ID "<Session.ID>/<Session.Turn>". Turn only moves when
// a run finishes, so a Send repeated after a crash resumes that run instead of
// paying for it twice, and a Session survives a restart with no extra machinery.
type Session struct {
	ID      string
	Runner  Runner
	Turn    int
	History []model.Message

	mu sync.Mutex
}

// NewSession returns an empty session over r.
func NewSession(r Runner) *Session { return &Session{ID: newRunID("session"), Runner: r} }

// Send starts or continues this turn's run. History moves on when the run reaches
// a terminal state with a usable transcript. A suspended or drained run leaves the
// session where it is: call Resume, or Send again.
func (s *Session) Send(ctx context.Context, text string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.advance(ctx, Input{Messages: append(append([]model.Message(nil), s.History...), model.User(text))})
}

// Resume delivers signals to this turn's suspended run.
func (s *Session) Resume(ctx context.Context, signals ...Signal) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.advance(ctx, Input{Signals: signals})
}

func (s *Session) advance(ctx context.Context, in Input) (*Run, error) {
	in.RunID = fmt.Sprintf("%s/%d", s.ID, s.Turn)
	res, err := s.Runner.Run(ctx, in)
	if err != nil || !res.State.Terminal() {
		return res, err
	}
	s.History = res.Messages
	s.Turn++
	return res, nil
}
