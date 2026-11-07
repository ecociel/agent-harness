package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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

// Price is what the demo charges for the cost meter.
var Price = budget.Price{InputPerM: 0.40, OutputPerM: 1.60}

// Server is the web chat: a static UI plus a small JSON/SSE API.
//
//	GET  /                 the UI (embedded, no build step)
//	GET  /api/meta         states, transitions, hook points, hooks, patterns
//	POST /api/chat         one turn; the response is an SSE stream of events
//	POST /api/approve      answer a wait; the turn goes on in an SSE stream
//	POST /api/reset        forget a session (history, budget, owner)
//
// An approval is a durable wait: the run suspends and the turn's stream ends with
// the wait in it. Nothing is held open for the person, and the answer is a signal.
// Each conversation is a harness.Session, so every message is its own run, chained
// to the one before, and the journal holds each message once. The journal lives in
// memory, so a restart forgets the runs.
type Server struct {
	Provider model.Provider
	Model    string
	// ProviderLabel is shown in the UI ("scripted demo", "openai · gpt-…").
	ProviderLabel string
	Log           *slog.Logger

	journal  harness.Journal
	metrics  *metrics.Registry
	mu       sync.Mutex
	sessions map[string]*session
	mux      *http.ServeMux
}

type session struct {
	turn   sync.Mutex // one turn at a time per session
	budget *budget.Budget
	traj   *trajectory.Recorder
	store  offload.Store
	chats  map[string]*harness.Session

	mu    sync.Mutex
	spent budget.Spent
}

func newSession() *session {
	sess := &session{traj: trajectory.New(), store: offload.NewMemStore(), chats: map[string]*harness.Session{}}
	sess.budget = budget.New(SessionLimits).WithPrice(Price).WithPool(func(context.Context, harness.Run) (budget.Spent, error) {
		return sess.total(), nil
	})
	return sess
}

func (sess *session) total() budget.Spent {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.spent
}

// count keeps the session's totals. A run knows only its own spend, so a limit across
// runs needs a projection of the events: this is that projection, in memory.
func (sess *session) count(e harness.Event) {
	var u model.Usage
	calls, tools := 0, 0
	switch e.Kind {
	case harness.KindModelDone:
		u, calls = e.Reply.Usage, 1
	case harness.KindModelFailed:
		u, calls = e.Failure.Usage, 1
	case harness.KindModelLost:
		calls = 1
	case harness.KindToolDone:
		tools = 1
	default:
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.spent.Usage = sess.spent.Usage.Add(u)
	sess.spent.ModelCalls += calls
	sess.spent.ToolCalls += tools
	sess.spent.CostUSD += float64(u.Input)*Price.InputPerM/1e6 + float64(u.Output)*Price.OutputPerM/1e6
}

// NewServer wires the routes.
func NewServer(p model.Provider, modelName, label string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{Provider: p, Model: modelName, ProviderLabel: label, Log: log, journal: harness.NewMemJournal(),
		metrics: metrics.NewRegistry(), sessions: map[string]*session{}}
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

// hookStack is every hook the demo attaches, in order: guard and approval veto before
// anything is spent, and the redactor and the stream meter show the rest of the points.
// Tracing, metrics and logging are sinks, attached once per turn in stream.
func (s *Server) hookStack(sess *session) []harness.Hook {
	return []harness.Hook{
		guard.Redactor(guard.PII()),
		&guard.Guard{InputDeny: guard.Secrets(), ToolDeny: []string{"delete_records"}},
		sess.budget,
		&approval.Policy{Threshold: tool.RiskDangerous, Ask: true},
		profile.Profiles{
			{Match: "demo", Instructions: "Model profile 'demo': the scripted model ignores style hints."},
			{Match: "gpt", Instructions: "Model profile 'gpt': prefer one tool call per turn."},
			{Match: "claude", Instructions: "Model profile 'claude': keep answers under 120 words."},
		},
		&compact.Compactor{MaxTokens: 1500, KeepLast: 6},
		&retry.Retry{Attempts: 3, Base: 150 * time.Millisecond},
		&streamMeter{},
		offload.New(sess.store, 1200),
		sess.traj,
	}
}

func (s *Server) session(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess
	}
	sess := newSession()
	s.sessions[id] = sess
	return sess
}

// conversation is the session's chat with one pattern.
func (s *Server) conversation(sess *session, pattern string) (*harness.Session, error) {
	if c, ok := sess.chats[pattern]; ok {
		return c, nil
	}
	hooks := s.hookStack(sess)
	tools := demoTools(sess.store)
	mk := func(name, instr string, reg *tool.Registry) *harness.Agent {
		return &harness.Agent{Name: name, Instructions: instr, Provider: s.Provider, Model: s.Model,
			Tools: reg, Hooks: hooks, MaxTurns: 8}
	}
	r, err := build(pattern, mk, tools, s.journal)
	if err != nil {
		return nil, err
	}
	c := harness.NewSession(r)
	sess.chats[pattern] = c
	return c, nil
}

// --- /api/meta -------------------------------------------------------------

type hookInfo struct {
	Name   string          `json:"name"`
	Points []harness.Point `json:"points"`
	Role   string          `json:"role"`
}

var hookRoles = map[string]string{
	"guard.redactor": "keeps personal data out of the model, the tools and the journal",
	"guard":          "guardrails: input deny, tool veto",
	"budget":         "tokens, cost, call caps, loop detection",
	"approval":       "scopes and human approval, as a durable wait",
	"profile":        "per-model prompt and tool adjustments",
	"compact":        "summarises old context, as a recorded edit",
	"retry":          "retries as recorded decisions",
	"stream":         "meters the stream and each result",
	"offload":        "moves large tool results out of the context",
	"trajectory":     "records the run for evals and replay",
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	var hooks []hookInfo
	for _, h := range s.hookStack(newSession()) {
		hooks = append(hooks, hookInfo{Name: h.Name(), Points: harness.HookPoints(h), Role: hookRoles[h.Name()]})
	}
	type edge struct {
		From harness.State `json:"from"`
		To   harness.State `json:"to"`
	}
	var edges []edge
	for _, e := range harness.Transitions() {
		edges = append(edges, edge{e.From, e.To})
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
	sess.turn.Lock()
	defer sess.turn.Unlock()
	c, err := s.conversation(sess, req.Pattern)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.stream(w, r, sess, func(ctx context.Context) (*harness.Run, error) {
		res, err := c.Send(ctx, req.Message)
		for req.AutoApprove && err == nil && res.State == harness.StateSuspended {
			var yes []harness.Signal
			for _, wait := range res.Waits {
				yes = append(yes, harness.Signal{Wait: wait.ID, Value: approval.Approved, By: "auto-approve"})
			}
			res, err = c.Resume(ctx, yes...)
		}
		return res, err
	})
}

// stream runs one step of a turn and sends its events as SSE.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, sess *session, step func(context.Context) (*harness.Run, error)) {
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
	ctx = harness.WithEvents(ctx, sess.count)
	ctx = harness.WithEvents(ctx, metrics.Sink(s.metrics))
	ctx = harness.WithEvents(ctx, logging.Sink(s.Log, slog.LevelDebug))
	ctx = harness.WithEvents(ctx, trace.Sink(trace.ExporterFunc(func(sd trace.SpanData) { send(sse{"span", sd}) })))
	ctx = approval.WithPrincipal(ctx, approval.Principal{ID: "jan", Scopes: []string{"mail"}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer closeOut()
		res, err := step(ctx)
		final := map[string]any{"spent": sess.total(), "limits": sess.budget.Limits(), "metrics": s.metrics.Snapshot()}
		if err != nil {
			final["error"] = err.Error()
		}
		if res != nil {
			final["result"] = summary(res)
			if res.State == harness.StateSuspended {
				final["waits"] = res.Waits
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

func summary(res *harness.Run) map[string]any {
	return map[string]any{
		"run_id": res.ID, "agent": res.Agent, "output": res.Output, "state": res.State,
		"stop": res.Stop, "detail": res.Detail, "usage": res.Usage, "turns": res.Turn,
		"repairs": res.Repairs, "model_calls": res.ModelCalls, "tool_calls": res.ToolCalls,
		"duration_ms": res.Duration().Milliseconds(), "path": res.Path, "handoff": res.Handoff,
	}
}

// --- approvals: a wait, a person, a signal --------------------------------------

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var a struct {
		Session string `json:"session"`
		Pattern string `json:"pattern"`
		Wait    string `json:"wait"`
		Approve bool   `json:"approve"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if a.Pattern == "" {
		a.Pattern = "single"
	}
	sess := s.session(a.Session)
	sess.turn.Lock()
	defer sess.turn.Unlock()
	c, ok := sess.chats[a.Pattern]
	if !ok {
		http.Error(w, "no such conversation", http.StatusNotFound)
		return
	}
	value := "no"
	if a.Approve {
		value = approval.Approved
	}
	s.stream(w, r, sess, func(ctx context.Context) (*harness.Run, error) {
		return c.Resume(ctx, harness.Signal{Wait: a.Wait, Value: value, By: "jan"})
	})
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

// streamMeter shows the points nothing else in the demo uses: it passes the stream
// through and notes how much text each reply and result carried.
type streamMeter struct{}

func (streamMeter) Name() string { return "stream" }

func (streamMeter) WrapModel(_ harness.Run, next model.Provider) model.Provider { return next }

func (streamMeter) AfterModel(ctx context.Context, _ harness.Run, resp model.Response) (model.Response, error) {
	harness.Note(ctx, "%d chars", len(resp.Text))
	return resp, nil
}

func (streamMeter) AfterTool(ctx context.Context, _ harness.Run, c harness.Call, content string) (string, error) {
	harness.Note(ctx, "%s: %s", c.Name, fmt.Sprint(len(content), " chars"))
	return content, nil
}
