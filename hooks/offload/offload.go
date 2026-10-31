// Package offload keeps oversized tool results out of the context window.
//
// A result longer than Limit bytes is stored and replaced by a small JSON handle with
// a preview. The model can page through the full content with the read_offloaded tool
// (Reader), so nothing is lost; it just stops costing tokens on every later turn.
//
// The journal holds the handle and not the result, so a durable run needs a durable
// Store: NewMemStore's results die with the process and leave a resumed run with
// handles nothing can read. A handle is the call's idempotency key, so a call repeated
// after a crash stores over itself. What is stored has passed the redactors, because
// this hook sits outside the ring.
package offload

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/tool"
)

// Store holds offloaded results. Implementations must be safe for concurrent use, and
// a repeated Put with the same id replaces what is there.
type Store interface {
	Put(ctx context.Context, id, content string) error
	// Get returns the content for id, and false if there is none.
	Get(ctx context.Context, id string) (string, bool, error)
}

type memStore struct {
	mu   sync.Mutex
	data map[string]string
}

// NewMemStore returns a Store in memory, for tests and runs that never resume.
func NewMemStore() Store { return &memStore{data: map[string]string{}} }

func (s *memStore) Put(_ context.Context, id, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = content
	return nil
}

func (s *memStore) Get(_ context.Context, id string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok, nil
}

// Offloader is the hook.
type Offloader struct {
	Store Store
	// Limit in bytes above which results are offloaded (default 4000).
	Limit int
	// Preview bytes kept inline (default 300).
	Preview int
}

// New returns an offloader over store.
func New(store Store, limit int) *Offloader { return &Offloader{Store: store, Limit: limit} }

// Name implements harness.Hook.
func (o *Offloader) Name() string { return "offload" }

// WrapTool replaces large results with a handle. A store that fails leaves the result
// inline: it costs tokens, and losing it would cost the work.
func (o *Offloader) WrapTool(_ harness.Run, next harness.ToolFunc) harness.ToolFunc {
	limit, preview := o.Limit, o.Preview
	if limit <= 0 {
		limit = 4000
	}
	if preview <= 0 {
		preview = 300
	}
	return func(ctx context.Context, c harness.Call) (string, error) {
		out, err := next(ctx, c)
		if err != nil || len(out) <= limit || c.Name == ReaderName {
			return out, err
		}
		if err := o.Store.Put(ctx, c.Key, out); err != nil {
			return out, nil
		}
		handle, err := json.Marshal(map[string]any{
			"offloaded": c.Key, "bytes": len(out), "preview": strings.ToValidUTF8(out[:min(preview, len(out))], ""),
			"hint": "call " + ReaderName + " with this id and an offset to read more",
		})
		if err != nil {
			return out, nil
		}
		return string(handle), nil
	}
}

// ReaderName is the name of the tool returned by Reader.
const ReaderName = "read_offloaded"

// Reader returns the tool that pages through offloaded content.
func Reader(store Store) tool.Tool {
	spec := tool.Spec{
		Name:        ReaderName,
		Description: "Read part of a tool result that was too large to show inline.",
		Effect:      tool.EffectRead,
		Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},` +
			`"offset":{"type":"integer"},"length":{"type":"integer"}},"required":["id"]}`),
	}
	return tool.Func(spec, func(ctx context.Context, raw json.RawMessage) (string, error) {
		var a struct {
			ID     string `json:"id"`
			Offset int    `json:"offset"`
			Length int    `json:"length"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		v, ok, err := store.Get(ctx, a.ID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no offloaded result %q", a.ID)
		}
		if a.Length <= 0 {
			a.Length = 2000
		}
		start := min(max(a.Offset, 0), len(v))
		end := min(start+a.Length, len(v))
		return strings.ToValidUTF8(v[start:end], ""), nil
	})
}
