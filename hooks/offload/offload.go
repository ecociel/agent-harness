// Package offload keeps oversized tool results out of the context window —
// the "context offloading wraps the tool call" middleware from the harness
// talk (and ecociel/agents cmd/patterns/offloaded).
//
// A result longer than Limit bytes is stored in a Store and replaced by a
// small JSON handle with a preview. The model can page through the full
// content with the read_offloaded tool (Reader), so nothing is lost; it just
// stops costing tokens on every subsequent turn.
package offload

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Store holds offloaded results. Safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	data map[string]string
	n    int
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{data: map[string]string{}} }

// Put stores s and returns its handle.
func (s *Store) Put(v string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	id := fmt.Sprintf("off-%d", s.n)
	s.data[id] = v
	return id
}

// Get returns the content for id.
func (s *Store) Get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok
}

// Offloader is the hook.
type Offloader struct {
	Store *Store
	// Limit in bytes above which results are offloaded (default 4000).
	Limit int
	// Preview bytes kept inline (default 300).
	Preview int
}

// New returns an offloader over store.
func New(store *Store, limit int) *Offloader { return &Offloader{Store: store, Limit: limit} }

// Name implements harness.Hook.
func (o *Offloader) Name() string { return "offload" }

// WrapTool replaces large results with a handle.
func (o *Offloader) WrapTool(r *harness.Run, next harness.ToolCallFunc) harness.ToolCallFunc {
	limit, preview := o.Limit, o.Preview
	if limit <= 0 {
		limit = 4000
	}
	if preview <= 0 {
		preview = 300
	}
	return func(ctx context.Context, call model.ToolCall) (string, error) {
		out, err := next(ctx, call)
		if err != nil || len(out) <= limit || call.Name == ReaderName {
			return out, err
		}
		id := o.Store.Put(out)
		handle, _ := json.Marshal(map[string]any{
			"offloaded": id, "bytes": len(out), "preview": out[:min(preview, len(out))],
			"hint": "call " + ReaderName + " with this id and an offset to read more",
		})
		r.Note(o.Name(), "offloaded %d bytes as %s", len(out), id)
		return string(handle), nil
	}
}

// ReaderName is the name of the tool returned by Reader.
const ReaderName = "read_offloaded"

// Reader returns the tool that pages through offloaded content.
func Reader(store *Store) tool.Tool {
	spec := tool.Spec{
		Name:        ReaderName,
		Description: "Read part of a tool result that was too large to show inline.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},` +
			`"offset":{"type":"integer"},"length":{"type":"integer"}},"required":["id"]}`),
	}
	return tool.Func(spec, func(_ context.Context, raw json.RawMessage) (string, error) {
		var a struct {
			ID     string `json:"id"`
			Offset int    `json:"offset"`
			Length int    `json:"length"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		v, ok := store.Get(a.ID)
		if !ok {
			return "", fmt.Errorf("no offloaded result %q", a.ID)
		}
		if a.Length <= 0 {
			a.Length = 2000
		}
		start := min(max(a.Offset, 0), len(v))
		end := min(start+a.Length, len(v))
		return v[start:end], nil
	})
}
