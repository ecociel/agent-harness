package harness

import (
	"context"
	"sync"

	"github.com/ecociel/agent-harness/model"
)

// Session is a multi-episode conversation with one Runner: each Send is one
// outer-loop episode, and the history carries over between them. It is the
// shape of a chat UI.
//
// A Session serialises Sends; it is safe for concurrent use.
type Session struct {
	Runner Runner

	mu      sync.Mutex
	history []model.Message
}

// NewSession returns an empty session over r.
func NewSession(r Runner) *Session { return &Session{Runner: r} }

// Send appends a user message, runs one episode and keeps the resulting
// history. If the run failed, the history is left unchanged so the user can
// retry.
func (s *Session) Send(ctx context.Context, text string) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := Input{Messages: append(append([]model.Message(nil), s.history...), model.User(text))}
	res, err := s.Runner.Run(ctx, in)
	if err != nil {
		return res, err
	}
	if len(res.Messages) > 0 {
		s.history = res.Messages
	} else {
		// Patterns that do not return a transcript: record the exchange.
		s.history = append(in.Messages, model.Assistant(res.Output))
	}
	return res, nil
}

// History returns a copy of the conversation so far.
func (s *Session) History() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Message(nil), s.history...)
}
