package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/hooks/budget"
	"github.com/ecociel/agent-harness/hooks/compact"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/hooks/logging"
	"github.com/ecociel/agent-harness/hooks/metrics"
	"github.com/ecociel/agent-harness/hooks/offload"
	"github.com/ecociel/agent-harness/hooks/profile"
	"github.com/ecociel/agent-harness/hooks/retry"
	"github.com/ecociel/agent-harness/hooks/trace"
	"github.com/ecociel/agent-harness/hooks/trajectory"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

//go:embed static
var staticFS embed.FS

// SessionLimits is the per-browser-session budget shown as the token bar.
var SessionLimits = budget.Limits{Tokens: 60_000, ModelCalls: 300, ToolCalls: 150, CostUSD: 0.50, RepeatCalls: 3}

// Server is the web chat: a static UI plus a small JSON/SSE API.
//
//	GET  /                 the UI (embedded, no build step)
//	GET  /api/meta         states, transitions, hook points, hooks, patterns
//	POST /api/chat         one turn; the response is an SSE stream of events
//	POST /api/approve      answer a pending approval request
//	POST /api/reset        forget a session (history, budget, owner)
type Server struct {
	Provider model.Provider
	Model    string
	// ProviderLabel is shown in the UI ("scripted demo", "openai · gpt-…").
	ProviderLabel string
	Log           *slog.Logger
	// ApprovalTimeout bounds how long a tool waits for a human (default 2m).
	ApprovalTimeout time.Duration

	metrics  *metrics.Registry
	mu       sync.Mutex
	sessions map[string]*session
	pending  map[string]chan bool
	mux      *http.ServeMux
}

type session struct {
	turn        sync.Mutex // one turn at a time per session
	budget      *budget.Budget
	traj        *trajectory.Recorder
	store       *offload.Store
	history     map[string][]model.Message
	runners     map[string]harness.Runner
	autoApprove atomic.Bool
}

// NewServer wires the routes.
func NewServer(p model.Provider, modelName, label string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{Provider: p, Model: modelName, ProviderLabel: label, Log: log,
		metrics: metrics.NewRegistry(), sessions: map[string]*session{}, pending: map[string]chan bool{}}
	sub, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /api/meta", s.meta)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/approve", s.approve)
	mux.HandleFunc("POST /api/reset", s.reset)
	s.mux = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// hookStack is every hook the demo attaches, in order. The order matters:
// guard and approval veto before anything is spent, retry sits outside the
// trace so each attempt is its own span, offload runs inside the tool span.
func (s *Server) hookStack(sess *session) []harness.Hook {
	return []harness.Hook{
		&guard.Guard{
			InputDeny: guard.Secrets(),
			ToolDeny:  []string{"delete_records"},
			Redact:    guard.PII(),
		},
		sess.budget,
		approval.New(tool.RiskDangerous, approval.ApproverFunc(s.approver(sess))),
		profile.Profiles{
			{Match: "demo", Instructions: "Model profile 'demo': the scripted model ignores style hints."},
			{Match: "gpt", Instructions: "Model profile 'gpt': prefer one tool call per turn."},
			{Match: "claude", Instructions: "Model profile 'claude': keep answers under 120 words."},
		},
		&compact.Compactor{MaxTokens: 1500, KeepLast: 6},
		&retry.Retry{Attempts: 3, Base: 150 * time.Millisecond},
		trace.New(nil),
		metrics.New(s.metrics),
		&streamMeter{},
		offload.New(sess.store, 1200),
		sess.traj,
		logging.New(s.Log),
	}
}

func (s *Server) session(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess
	}
	sess := &session{
		budget: budget.New(SessionLimits).WithPrice(budget.Price{InputPerM: 0.40, OutputPerM: 1.60}),
		traj:   trajectory.New(), store: offload.NewStore(),
		history: map[string][]model.Message{}, runners: map[string]harness.Runner{},
	}
	s.sessions[id] = sess
	return sess
}

func (s *Server) runner(sess *session, pattern string) (harness.Runner, error) {
	if r, ok := sess.runners[pattern]; ok {
		return r, nil
	}
	hooks := s.hookStack(sess)
	tools := demoTools(sess.store)
	mk := func(name, instr string, reg *tool.Registry) *harness.Agent {
		return &harness.Agent{Name: name, Instructions: instr, Provider: s.Provider, Model: s.Model,
			Tools: reg, Hooks: hooks, MaxTurns: 8}
	}
	r, err := build(pattern, mk, tools)
	if err != nil {
		return nil, err
	}
	sess.runners[pattern] = r
	return r, nil
}

// --- /api/meta -------------------------------------------------------------

type hookInfo struct {
	Name   string          `json:"name"`
	Points []harness.Point `json:"points"`
	Role   string          `json:"role"`
}

var hookRoles = map[string]string{
	"guard":      "guardrails: input deny, tool veto, output redaction",
	"budget":     "tokens, cost, call caps, loop detection",
	"approval":   "SPA: security · policy · approval (human in the loop)",
	"profile":    "per-model prompt and tool adjustments",
	"compact":    "context compaction before the model call",
	"retry":      "retries and fallback around the model call",
	"trace":      "OTel-style spans (gen_ai conventions)",
	"metrics":    "counters and latency histograms",
	"stream":     "OnDelta: live streaming meter",
	"offload":    "moves large tool results out of the context",
	"trajectory": "records the run for evals and replay",
	"logging":    "structured logs (slog) to the server terminal",
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	sess := &session{budget: budget.New(SessionLimits), traj: trajectory.New(), store: offload.NewStore()}
	var hooks []hookInfo
	for _, h := range s.hookStack(sess) {
		hooks = append(hooks, hookInfo{Name: h.Name(), Points: harness.HookPoints(h), Role: hookRoles[h.Name()]})
	}
	type edge struct {
		From harness.State `json:"from"`
		To   harness.State `json:"to"`
	}
	var edges []edge
	for from, tos := range harness.Transitions() {
		for _, to := range tos {
			edges = append(edges, edge{from, to})
		}
	}
	writeJSON(w, map[string]any{
		"provider": s.ProviderLabel, "model": s.Model,
		"states": harness.States(), "transitions": edges, "points": harness.Points(),
		"hooks": hooks, "patterns": patterns, "limits": SessionLimits,
	})
}

// --- /api/chat ---------------------------------------------------------------

type chatRequest struct {
	Session     string `json:"session"`
	Pattern     string `json:"pattern"`
	Message     string `json:"message"`
	AutoApprove bool   `json:"auto_approve"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Session == "" || req.Message == "" {
		http.Error(w, "session and message are required", http.StatusBadRequest)
		return
	}
	if req.Pattern == "" {
		req.Pattern = "single"
	}
	sess := s.session(req.Session)
	sess.autoApprove.Store(req.AutoApprove)
	sess.turn.Lock()
	defer sess.turn.Unlock()
	runner, err := s.runner(sess, req.Pattern)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Events may arrive after the turn ended (background sub-agent jobs
	// outlive it), so sends are guarded against the closed channel.
	out := make(chan sse, 256)
	var (
		outMu  sync.RWMutex
		closed bool
	)
	send := func(ev sse) {
		outMu.RLock()
		defer outMu.RUnlock()
		if closed {
			return
		}
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}
	closeOut := func() {
		outMu.Lock()
		closed = true
		close(out)
		outMu.Unlock()
	}
	ctx = harness.WithEvents(ctx, func(e harness.Event) { send(sse{"harness", e}) })
	ctx = trace.WithTracer(ctx, trace.NewTracer(trace.ExporterFunc(func(sd trace.SpanData) { send(sse{"span", sd}) })))
	ctx = approval.WithPrincipal(ctx, approval.Principal{ID: "jan", Scopes: []string{"mail"}})

	history := append(append([]model.Message(nil), sess.history[req.Pattern]...), model.User(req.Message))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer closeOut()
		res, err := runner.Run(ctx, harness.Input{Messages: history})
		final := map[string]any{"spent": sess.budget.Spent(), "limits": sess.budget.Limits(), "metrics": s.metrics.Snapshot()}
		if err != nil {
			final["error"] = err.Error()
		}
		if res != nil {
			final["result"] = summary(res)
			if res.OK() || res.Stop != harness.StopFailed {
				sess.history[req.Pattern] = nextHistory(req.Pattern, history, res)
			}
		}
		send(sse{"done", final})
	}()
	for ev := range out {
		if err := ev.write(w); err != nil {
			cancel()
			break
		}
		flusher.Flush()
	}
	cancel()
	for range out { // drain so the run goroutine can finish
	}
	<-done
}

// nextHistory keeps the full transcript for the single agent (so compaction
// has something to do) and user/answer pairs for patterns, whose internal
// messages belong to their sub-agents.
func nextHistory(pattern string, history []model.Message, res *harness.Result) []model.Message {
	if pattern == "single" && len(res.Messages) > 0 {
		return res.Messages
	}
	out := res.Output
	if out == "" {
		out = "(" + string(res.Stop) + ": " + res.Detail + ")"
	}
	return append(history, model.Assistant(out))
}

func summary(res *harness.Result) map[string]any {
	return map[string]any{
		"run_id": res.RunID, "agent": res.Agent, "output": res.Output, "state": res.State,
		"stop": res.Stop, "detail": res.Detail, "usage": res.Usage, "turns": res.Turns,
		"repairs": res.Repairs, "model_calls": res.ModelCalls, "tool_calls": res.ToolCalls,
		"duration_ms": res.Duration.Milliseconds(), "path": res.Path, "handoff": res.Handoff,
	}
}

// --- approvals (the "SPA" hook made interactive) ------------------------------

func (s *Server) approver(sess *session) func(ctx context.Context, req approval.Request) (bool, string) {
	return func(ctx context.Context, req approval.Request) (bool, string) {
		if sess.autoApprove.Load() {
			return true, "auto-approved (toggle in the UI)"
		}
		id := newID()
		ch := make(chan bool, 1)
		s.mu.Lock()
		s.pending[id] = ch
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
		harness.Emit(ctx, harness.Event{Kind: "approval", RunID: req.RunID, Agent: req.Agent, Hook: "approval",
			Point: harness.PointBeforeTool, Outcome: "pending", Text: req.Call.Name,
			Data: map[string]any{"id": id, "tool": req.Call.Name, "arguments": json.RawMessage(req.Call.Arguments), "risk": req.Tool.Risk.String()}})
		timeout := s.ApprovalTimeout
		if timeout == 0 {
			timeout = 2 * time.Minute
		}
		select {
		case ok := <-ch:
			if ok {
				return true, "approved by jan"
			}
			return false, "denied by jan"
		case <-time.After(timeout):
			return false, "no answer within " + timeout.String()
		case <-ctx.Done():
			return false, "cancelled"
		}
	}
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var a struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	ch, ok := s.pending[a.ID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no such pending approval", http.StatusNotFound)
		return
	}
	select {
	case ch <- a.Approve:
	default:
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	var a struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&a)
	s.mu.Lock()
	delete(s.sessions, a.Session)
	s.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
}

// --- helpers -------------------------------------------------------------------

type sse struct {
	event string
	data  any
}

func (e sse) write(w http.ResponseWriter) error {
	b, err := json.Marshal(e.data)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	_, err = w.Write([]byte("event: " + e.event + "\ndata: " + string(b) + "\n\n"))
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// streamMeter is the demo's OnDelta hook: it counts streamed chunks and
// characters per run so the UI can show live streaming at that point.
type streamMeter struct{}

func (streamMeter) Name() string { return "stream" }

func (streamMeter) OnDelta(_ context.Context, r *harness.Run, ev model.Event) {
	if d, ok := ev.(model.TextDelta); ok {
		n, _ := r.Value("stream.chars").(int)
		r.Set("stream.chars", n+len(d.Text))
	}
}
