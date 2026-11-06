// Package fake provides a deterministic, scriptable model.Provider for tests,
// examples and the zero-key web demo.
//
// A Provider answers either from a Script (responses consumed in order) or
// from a Func that computes a response from the request. Every request is
// recorded, message parts with their resolved bytes, so tests can assert what
// the harness actually sent. When a response carries no Usage, plausible token
// counts are estimated from the request and the reply so budget and metrics
// hooks have real numbers to work with.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/ecociel/agent-harness/model"
)

// ErrExhausted is returned when a scripted Provider runs out of responses.
var ErrExhausted = errors.New("fake: script exhausted")

// Provider is a fake model. Use Script or NewFunc to build one. Safe for
// concurrent use.
type Provider struct {
	// Delay is slept before each reply (and between streamed chunks, divided
	// by the chunk count) so streaming UIs have something to animate.
	Delay time.Duration

	mu       sync.Mutex
	script   []model.Response
	fn       func(req model.Request) (model.Response, error)
	requests []model.Request
}

// Script returns a Provider that replies with responses in order.
func Script(responses ...model.Response) *Provider {
	return &Provider{script: responses}
}

// NewFunc returns a Provider whose replies are computed by fn.
func NewFunc(fn func(req model.Request) (model.Response, error)) *Provider {
	return &Provider{fn: fn}
}

// Text is a reply with only text.
func Text(text string) model.Response { return model.Response{Text: text} }

// Call is a reply that asks for one tool call with the given arguments
// (marshalled to JSON).
func Call(name string, args any) model.Response {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return model.Response{ToolCalls: []model.ToolCall{{ID: "call_" + name, Name: name, Arguments: raw}}}
}

// Requests returns a copy of every request received so far.
func (p *Provider) Requests() []model.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.Request(nil), p.requests...)
}

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
	p.mu.Lock()
	p.requests = append(p.requests, req.Clone())
	var (
		resp model.Response
		err  error
	)
	switch {
	case p.fn != nil:
		p.mu.Unlock()
		resp, err = p.fn(req)
	case len(p.script) > 0:
		resp = p.script[0]
		p.script = p.script[1:]
		p.mu.Unlock()
	default:
		p.mu.Unlock()
		err = fmt.Errorf("%w after %d requests", ErrExhausted, len(p.Requests())-1)
	}
	if err != nil {
		return model.Fail(err)
	}
	if resp.Usage == (model.Usage{}) {
		resp.Usage = model.Usage{
			Input:  model.EstimateTokens(req.Messages) + 10*len(req.Tools),
			Output: max(1, (len(resp.Text)+len(fmt.Sprint(resp.ToolCalls)))/4),
		}
	}
	return p.stream(ctx, resp)
}

func (p *Provider) stream(ctx context.Context, resp model.Response) iter.Seq2[model.Event, error] {
	inner := model.Stream(resp)
	if p.Delay <= 0 {
		return inner
	}
	return func(yield func(model.Event, error) bool) {
		step := p.Delay / 8
		for ev, err := range inner {
			select {
			case <-ctx.Done():
				yield(nil, context.Cause(ctx))
				return
			case <-time.After(step):
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}
