package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness/provider/fake"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewServer(&flaky{next: fake.NewFunc(brain)}, "demo-scripted", "test", nil))
	t.Cleanup(ts.Close)
	return ts
}

type sseEvent struct {
	Name string
	Data map[string]any
}

// chat posts one turn and parses the whole SSE response. onEvent may react
// (e.g. answer an approval) while the stream is still open.
func chat(t *testing.T, ts *httptest.Server, body map[string]any, onEvent func(sseEvent)) []sseEvent {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, msg)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var evs []sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data); err != nil {
				t.Fatalf("bad data %q: %v", line, err)
			}
		case line == "":
			evs = append(evs, cur)
			if onEvent != nil {
				onEvent(cur)
			}
			cur = sseEvent{}
		}
	}
	return evs
}

func final(t *testing.T, evs []sseEvent) map[string]any {
	t.Helper()
	if len(evs) == 0 || evs[len(evs)-1].Name != "done" {
		t.Fatalf("stream did not end with done: %d events", len(evs))
	}
	res, _ := evs[len(evs)-1].Data["result"].(map[string]any)
	if res == nil {
		t.Fatalf("no result in done: %v", evs[len(evs)-1].Data)
	}
	return res
}

func TestIndexAndAssets(t *testing.T) {
	ts := newTestServer(t)
	for path, want := range map[string]string{
		"/":                            "Every hook, visible.",
		"/app.js":                      "readSSE",
		"/app.css":                     "--mint:#6fd6b0",
		"/fonts/instrument-sans.woff2": "",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("GET %s: status %d, contains %q = false", path, resp.StatusCode, want)
		}
	}
}

func TestMeta(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var meta struct {
		States   []string `json:"states"`
		Points   []string `json:"points"`
		Patterns []Pattern
		Hooks    []hookInfo
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Patterns) != 12 || len(meta.Points) != 12 || len(meta.States) < 10 {
		t.Fatalf("meta: %d patterns, %d points, %d states", len(meta.Patterns), len(meta.Points), len(meta.States))
	}
	// Every hook point must be covered by at least one demo hook, so the
	// diagnostic view can show all of them firing.
	covered := map[string]bool{}
	for _, h := range meta.Hooks {
		for _, p := range h.Points {
			covered[string(p)] = true
		}
	}
	for _, p := range meta.Points {
		if !covered[p] {
			t.Errorf("no demo hook implements %s", p)
		}
	}
}

func TestChatStreamsEveryHookPoint(t *testing.T) {
	ts := newTestServer(t)
	evs := chat(t, ts, map[string]any{"session": "s1", "message": "What time is it?"}, nil)
	res := final(t, evs)
	if res["stop"] != "completed" || !strings.HasPrefix(res["output"].(string), "It is ") {
		t.Fatalf("result %v", res)
	}
	points, kinds, spans := map[string]bool{}, map[string]bool{}, 0
	for _, e := range evs {
		switch e.Name {
		case "harness":
			kinds[e.Data["kind"].(string)] = true
			if p, ok := e.Data["point"].(string); ok {
				points[p] = true
			}
		case "span":
			spans++
		}
	}
	for _, p := range []string{"BeforeRun", "Context", "BeforeModel", "AfterModel", "BeforeTool", "AfterTool", "CheckOutput", "AfterRun"} {
		if !points[p] {
			t.Errorf("hook point %s never fired", p)
		}
	}
	for _, k := range []string{"run.started", "model.started", "model.done", "tool.gated", "tool.started", "tool.done",
		"output.checked", "run.ended", "hook", "delta"} {
		if !kinds[k] {
			t.Errorf("no %s event", k)
		}
	}
	if spans < 3 {
		t.Errorf("only %d spans", spans)
	}
}

func TestChatGuardrailsAndRedaction(t *testing.T) {
	ts := newTestServer(t)
	res := final(t, chat(t, ts, map[string]any{"session": "g", "message": "my password is hunter2"}, nil))
	if res["stop"] != "guardrail" {
		t.Errorf("secret input: stop %v", res["stop"])
	}
	res = final(t, chat(t, ts, map[string]any{"session": "g", "message": "delete all records"}, nil))
	if !strings.Contains(res["output"].(string), "not allowed") {
		t.Errorf("tool veto: %v", res["output"])
	}
	res = final(t, chat(t, ts, map[string]any{"session": "g", "message": "show my AHV number"}, nil))
	if out := res["output"].(string); strings.Contains(out, "756.1234") || !strings.Contains(out, "[redacted:") {
		t.Errorf("redaction: %q", out)
	}
}

// post sends a JSON body to path and parses the SSE response.
func stream(t *testing.T, ts *httptest.Server, path string, body map[string]any) []sseEvent {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, msg)
	}
	var evs []sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data); err != nil {
				t.Fatalf("bad data %q: %v", line, err)
			}
		case line == "":
			evs = append(evs, cur)
			cur = sseEvent{}
		}
	}
	return evs
}

// The stream ends at a wait, nothing is held open, and the answer resumes the run.
func TestChatApprovalIsADurableWait(t *testing.T) {
	ts := newTestServer(t)
	for session, approve := range map[string]bool{"yes": true, "no": false} {
		evs := chat(t, ts, map[string]any{"session": session, "message": "send an email to Jan"}, nil)
		last := evs[len(evs)-1].Data
		if res := last["result"].(map[string]any); res["state"] != "suspended" {
			t.Fatalf("result %v", res)
		}
		waits, _ := last["waits"].([]any)
		if len(waits) != 1 {
			t.Fatalf("waits %v", last["waits"])
		}
		id := waits[0].(map[string]any)["id"]
		out := final(t, stream(t, ts, "/api/approve", map[string]any{"session": session, "wait": id, "approve": approve}))["output"].(string)
		if approve != strings.Contains(out, "sent") {
			t.Errorf("approve=%v: %q", approve, out)
		}
	}
}

func TestChatAutoApproveAnswersEveryWait(t *testing.T) {
	ts := newTestServer(t)
	evs := chat(t, ts, map[string]any{"session": "auto", "message": "send an email to Jan", "auto_approve": true}, nil)
	if out := final(t, evs)["output"].(string); !strings.Contains(out, "sent") {
		t.Fatalf("%q", out)
	}
}

func TestChatApproveWithNoConversationIsRefused(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/approve", "application/json", strings.NewReader(`{"session":"none","wait":"w"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestChatRetriesAFlakyModel(t *testing.T) {
	ts := newTestServer(t)
	evs := chat(t, ts, map[string]any{"session": "f", "message": "this one is flaky"}, nil)
	failed := 0
	for _, e := range evs {
		if e.Name == "harness" && e.Data["kind"] == "model.failed" {
			failed++
		}
	}
	if res := final(t, evs); res["stop"] != "completed" || failed != 1 {
		t.Fatalf("%v, %d failures", res, failed)
	}
}

// The budget spans a session's runs, so it comes from a projection and not one Run.
func TestChatSpendAddsUpAcrossTurns(t *testing.T) {
	ts := newTestServer(t)
	used := func(evs []sseEvent) float64 {
		return evs[len(evs)-1].Data["spent"].(map[string]any)["model_calls"].(float64)
	}
	first := used(chat(t, ts, map[string]any{"session": "b", "message": "hello"}, nil))
	second := used(chat(t, ts, map[string]any{"session": "b", "message": "hello again"}, nil))
	if first != 1 || second != 2 {
		t.Fatalf("%v then %v", first, second)
	}
}

func TestChatBudgetLoopDetection(t *testing.T) {
	ts := newTestServer(t)
	res := final(t, chat(t, ts, map[string]any{"session": "l", "message": "loop"}, nil))
	if res["stop"] != "loop_detected" {
		t.Errorf("stop %v (%v)", res["stop"], res["detail"])
	}
}

func TestChatEveryPattern(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range patterns {
		t.Run(p.ID, func(t *testing.T) {
			evs := chat(t, ts, map[string]any{"session": "p-" + p.ID, "pattern": p.ID, "message": p.Try, "auto_approve": true}, nil)
			res := final(t, evs)
			if res["stop"] != "completed" || res["output"] == "" {
				t.Fatalf("%s: stop %v detail %v output %q", p.ID, res["stop"], res["detail"], res["output"])
			}
			if p.ID != "single" {
				sawPattern := false
				for _, e := range evs {
					if e.Name == "harness" && e.Data["kind"] == "pattern" {
						sawPattern = true
						break
					}
				}
				if !sawPattern && p.ID != "subagent" && p.ID != "background" {
					t.Error("no pattern events")
				}
			}
		})
	}
}

func TestChatRejectsBadInput(t *testing.T) {
	ts := newTestServer(t)
	for _, body := range []string{`{`, `{"session":"x"}`, `{"session":"x","message":"hi","pattern":"nope"}`} {
		resp, err := http.Post(ts.URL+"/api/chat", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, resp.StatusCode)
		}
	}
}

func TestProviderFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, m, _, err := providerFromEnv(env(nil)); err != nil || m != "demo-scripted" {
		t.Errorf("default: %v %v", m, err)
	}
	if _, _, _, err := providerFromEnv(env(map[string]string{"HARNESS_PROVIDER": "openai"})); err == nil {
		t.Error("openai without key should fail")
	}
	if _, m, _, err := providerFromEnv(env(map[string]string{"HARNESS_PROVIDER": "anthropic", "ANTHROPIC_API_KEY": "k"})); err != nil || m != "claude-opus-5-5" {
		t.Errorf("anthropic: %v %v", m, err)
	}
	if _, _, _, err := providerFromEnv(env(map[string]string{"HARNESS_PROVIDER": "bogus"})); err == nil {
		t.Error("bogus provider accepted")
	}
}

func TestChatCompactionAndOffload(t *testing.T) {
	ts := newTestServer(t)
	saw := func(evs []sseEvent, kind, text string) bool {
		for _, e := range evs {
			if e.Name == "harness" && e.Data["kind"] == kind {
				if s, _ := e.Data["text"].(string); strings.Contains(s, text) {
					return true
				}
			}
		}
		return false
	}
	if evs := chat(t, ts, map[string]any{"session": "c", "message": "search the docs for budget"}, nil); !saw(evs, "tool.done", `"offloaded"`) {
		t.Error("the large result was not offloaded")
	}
	compacted := false
	for i := 0; i < 6 && !compacted; i++ {
		compacted = saw(chat(t, ts, map[string]any{"session": "c", "message": "tell me something long"}, nil), "hook", "messages summarised")
	}
	if !compacted {
		t.Error("compaction never happened")
	}
}
