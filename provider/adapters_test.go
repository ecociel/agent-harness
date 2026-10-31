package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/anthropic"
	"github.com/ecociel/agent-harness/provider/openai"
	"github.com/ecociel/agent-harness/tool"
)

const secret = "AHV-756.1234.5678.97"

// step answers one request.
type step func(w http.ResponseWriter)

// server plays a script and keeps every request body.
type server struct {
	*httptest.Server
	mu     sync.Mutex
	script []step
	bodies [][]byte
}

func serve(t *testing.T, script ...step) *server {
	t.Helper()
	s := &server{script: script}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body.Bytes())
		if len(s.script) == 0 {
			s.mu.Unlock()
			http.Error(w, "script exhausted", http.StatusTeapot)
			return
		}
		next := s.script[0]
		s.script = s.script[1:]
		s.mu.Unlock()
		next(w)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.bodies)
}

// status answers with an error, with headers as name, value pairs.
func status(code int, body string, headers ...string) step {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = fmt.Fprint(w, body)
	}
}

// stream writes SSE data lines and then ends the response.
func stream(lines ...string) step {
	return func(w http.ResponseWriter) {
		for _, l := range lines {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", l)
		}
	}
}

// wire is what the adapters do not share: how each provider says the same things.
type wire struct {
	name     string
	provider func(url string) model.Provider
	text     func(text string) step
	call     func(id, name string) step
	cut      step   // a reply that stops partway with no end marker
	billedIn int    // what a cut reply reports as billed input
	limited  string // the body of a 429
	tooLong  string // the body of a 400 for a full context
	denied   string // the body of a 401
}

var anthropicWire = wire{
	name:     "anthropic",
	provider: func(url string) model.Provider { return &anthropic.Provider{BaseURL: url, APIKey: "k", Model: "m"} },
	text: func(text string) step {
		return stream(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text),
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`)
	},
	call: func(id, name string) step {
		return stream(`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
			fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q}}`, id, name),
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`)
	},
	cut: stream(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half a rep"}}`),
	billedIn: 10,
	limited:  `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
	tooLong:  `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 2000000 tokens > 1000000 maximum"}}`,
	denied:   `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
}

var openaiWire = wire{
	name:     "openai",
	provider: func(url string) model.Provider { return &openai.Provider{BaseURL: url, APIKey: "k", Model: "m"} },
	text: func(text string) step {
		return stream(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text),
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, `[DONE]`)
	},
	call: func(id, name string) step {
		return stream(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"function":{"name":%q,"arguments":"{}"}}]}}]}`, id, name),
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, `[DONE]`)
	},
	cut:     stream(`{"choices":[{"delta":{"content":"half a rep"}}]}`),
	limited: `{"error":{"message":"slow down","type":"requests","code":"rate_limit_exceeded"}}`,
	tooLong: `{"error":{"message":"too many tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`,
	denied:  `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`,
}

// retries is a recovery policy of the shape hooks/retry has.
type retries struct{}

func (retries) Name() string { return "retries" }
func (retries) Recover(_ context.Context, r harness.Run, f harness.Failure) harness.Recovery {
	return harness.Recovery{Retry: f.Attempt < 3 && (f.Class == model.ClassRateLimit || f.Class == model.ClassTransient)}
}

// scrubber is a redactor that replaces one value everywhere.
type scrubber struct{}

func (scrubber) Name() string { return "scrubber" }
func (scrubber) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, secret, "[ahv]"), nil
}

func look() tool.Tool {
	return tool.Func(tool.Spec{Name: "look", Effect: tool.EffectRead},
		func(context.Context, json.RawMessage) (string, error) { return "seen", nil })
}

func agent(p model.Provider, j harness.Journal, hooks ...harness.Hook) *harness.Agent {
	return &harness.Agent{Name: "a", Instructions: "be brief", Provider: p, Journal: j, Hooks: hooks, Tools: tool.NewRegistry(look())}
}

func prompt(text string) harness.Input {
	in := harness.Prompt(text)
	in.RunID = "r"
	return in
}

func failures(t *testing.T, j harness.Journal) []harness.Failure {
	t.Helper()
	evs, err := j.Load(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	var out []harness.Failure
	for _, e := range evs {
		if e.Kind == harness.KindModelFailed {
			out = append(out, *e.Failure)
		}
	}
	return out
}

func eachWire(t *testing.T, f func(t *testing.T, w wire)) {
	for _, w := range []wire{anthropicWire, openaiWire} {
		t.Run(w.name, func(t *testing.T) { f(t, w) })
	}
}

func TestARateLimitWaitsAsTheServerAskedThenSucceeds(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t, status(429, w.limited, "Retry-After", "0.2"), w.text("done"))
		j := harness.NewMemJournal()
		start := time.Now()
		res, err := agent(w.provider(s.URL), j, retries{}).Run(context.Background(), prompt("x"))
		if err != nil || res.Output != "done" {
			t.Fatalf("%v %+v", err, res)
		}
		if took := time.Since(start); took < 200*time.Millisecond {
			t.Fatalf("retried after %v", took)
		}
		f := failures(t, j)
		if len(f) != 1 || f[0].Class != model.ClassRateLimit || f[0].RetryAfter != 200*time.Millisecond {
			t.Fatalf("%+v", f)
		}
	})
}

// A wait this long must not hold a worker, so the run parks and a sweep wakes it.
func TestALongRetryAfterParksTheRun(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t, status(429, w.limited, "Retry-After", "120"), w.text("done"))
		res, err := agent(w.provider(s.URL), harness.NewMemJournal(), retries{}).Run(context.Background(), prompt("x"))
		if err != nil || res.State != harness.StateSuspended || len(res.Waits) != 1 || res.Waits[0].Kind != harness.WaitTimer {
			t.Fatalf("%v %+v", err, res)
		}
		if n := len(s.requests()); n != 1 {
			t.Fatalf("%d requests while parked", n)
		}
	})
}

func TestAServerErrorRetriesAndAnAuthErrorDoesNot(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t, status(503, "upstream down"), w.text("done"))
		if res, err := agent(w.provider(s.URL), harness.NewMemJournal(), retries{}).Run(context.Background(), prompt("x")); err != nil || res.Output != "done" {
			t.Fatalf("%v %+v", err, res)
		}
		s = serve(t, status(401, w.denied), w.text("never"))
		j := harness.NewMemJournal()
		res, err := agent(w.provider(s.URL), j, retries{}).Run(context.Background(), prompt("x"))
		if err == nil || res.State != harness.StateFailed || len(s.requests()) != 1 {
			t.Fatalf("%v %+v, %d requests", err, res, len(s.requests()))
		}
		if f := failures(t, j); len(f) != 1 || f[0].Class != model.ClassAuth {
			t.Fatalf("%+v", f)
		}
	})
}

func TestAFullContextIsItsOwnClass(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t, status(400, w.tooLong))
		j := harness.NewMemJournal()
		if _, err := agent(w.provider(s.URL), j, retries{}).Run(context.Background(), prompt("x")); err == nil {
			t.Fatal("want a failure")
		}
		if f := failures(t, j); len(f) != 1 || f[0].Class != model.ClassContextLength {
			t.Fatalf("%+v", f)
		}
	})
}

// A stream that stops with no end marker is not a reply, however clean the close. Half
// an answer must not be recorded as the answer, and what was billed still counts.
func TestACutStreamIsARetryNotAnAnswer(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t, w.cut, w.text("whole"))
		j := harness.NewMemJournal()
		res, err := agent(w.provider(s.URL), j, retries{}).Run(context.Background(), prompt("x"))
		if err != nil || res.Output != "whole" {
			t.Fatalf("%v %+v", err, res)
		}
		f := failures(t, j)
		if len(f) != 1 || f[0].Class != model.ClassTransient || f[0].Usage.Input != w.billedIn {
			t.Fatalf("%+v", f)
		}
		if res.Usage.Input != w.billedIn+10 {
			t.Fatalf("usage %+v lost what the cut call billed", res.Usage)
		}
	})
}

func journalOf(t *testing.T, j harness.Journal) []byte {
	t.Helper()
	evs, err := j.Load(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(evs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestASecretInAReplyOrAnErrorStaysOut(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t, status(500, "failed for "+secret), w.call("c1", "look"), w.text("your number is "+secret))
		j := harness.NewMemJournal()
		res, err := agent(w.provider(s.URL), j, retries{}, scrubber{}).Run(context.Background(), prompt("x"))
		if err != nil || res.Output != "your number is [ahv]" {
			t.Fatalf("%v %+v", err, res)
		}
		if bytes.Contains(journalOf(t, j), []byte(secret)) {
			t.Fatal("the journal holds the secret")
		}
		for i, b := range s.requests() {
			if bytes.Contains(b, []byte(secret)) {
				t.Fatalf("request %d sent the secret back", i)
			}
		}
	})
}

// thinkingThenCall is a reply with interleaved thinking, with deltas split like a real
// stream's.
var thinkingThenCall = stream(
	`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":30,"cache_creation_input_tokens":5,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first "}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"note "}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"redacted_thinking","data":"opaque-data"}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"more"}}`,
	`{"type":"content_block_stop","index":3}`,
	`{"type":"content_block_start","index":4,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	`{"type":"content_block_delta","index":4,"delta":{"type":"signature_delta","signature":"sig-2"}}`,
	`{"type":"content_block_stop","index":4}`,
	`{"type":"content_block_start","index":5,"content_block":{"type":"tool_use","id":"c1","name":"look"}}`,
	`{"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
	`{"type":"content_block_stop","index":5}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
	`{"type":"message_stop"}`)

const wantReplay = `[{"type":"thinking","thinking":"first thought","signature":"sig-1"},` +
	`{"type":"text","text":"note "},{"type":"redacted_thinking","data":"opaque-data"},{"type":"text","text":"more"},` +
	`{"type":"thinking","thinking":"","signature":"sig-2"},{"type":"tool_use","id":"c1","name":"look","input":{}}]`

func messages(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var w struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	return w.Messages
}

// content is the content array of an assistant message.
func content(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return string(m.Content)
}

// jsonJournal stores bytes, as a database does, and can die after a reply is recorded.
type jsonJournal struct {
	mu    sync.Mutex
	runs  map[string][][]byte
	dying bool
	dead  bool
}

func (j *jsonJournal) Append(_ context.Context, id string, evs []harness.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.dead {
		return errors.New("process died")
	}
	if evs[0].Seq != int64(len(j.runs[id]))+1 {
		return harness.ErrConflict
	}
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		j.runs[id] = append(j.runs[id], b)
		j.dead = j.dead || j.dying && e.Kind == harness.KindModelDone
	}
	return nil
}

func (j *jsonJournal) Load(_ context.Context, id string) ([]harness.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]harness.Event, len(j.runs[id]))
	for i, b := range j.runs[id] {
		if err := json.Unmarshal(b, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// The API's own test: each request's history is a byte-identical prefix of the next.
// This one crosses a crash, so the reply comes back through the journal's bytes.
func TestThinkingGoesBackInPlaceAndTheHistoryOnlyGrows(t *testing.T) {
	s := serve(t, thinkingThenCall, anthropicWire.call("c2", "look"), anthropicWire.text("done"))
	p := anthropicWire.provider(s.URL)
	j := &jsonJournal{runs: map[string][][]byte{}, dying: true}
	if _, err := agent(p, j).Run(context.Background(), prompt("x")); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a crash after the reply, got %v", err)
	}
	j.dying, j.dead = false, false
	res, err := agent(anthropicWire.provider(s.URL), j).Run(context.Background(), harness.Resume("r"))
	if err != nil || res.Output != "done" {
		t.Fatalf("%v %+v", err, res)
	}
	reqs := s.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		before, after := messages(t, reqs[i-1]), messages(t, reqs[i])
		if len(after) <= len(before) {
			t.Fatalf("request %d did not grow", i)
		}
		for n := range before {
			if !bytes.Equal(before[n], after[n]) {
				t.Fatalf("request %d changed message %d:\n%s\n%s", i, n, before[n], after[n])
			}
		}
	}
	if got := content(t, messages(t, reqs[1])[1]); got != wantReplay {
		t.Fatalf("got  %s\nwant %s", got, wantReplay)
	}
	if res.Usage.Input != 65 || res.Usage.Cached != 30 {
		t.Fatalf("cache reads and writes are input too: %+v", res.Usage)
	}
}

type rewrite struct{}

func (rewrite) Name() string { return "rewrite" }
func (rewrite) AfterModel(_ context.Context, _ harness.Run, r model.Response) (model.Response, error) {
	r.Text = "clean"
	return r, nil
}

// A hook that changed the reply decided what the history says, and thinking cannot
// bring the old words back.
func TestARewrittenReplyGoesBackAsRewrittenWithoutThinking(t *testing.T) {
	s := serve(t, thinkingThenCall, anthropicWire.text("done"))
	if _, err := agent(anthropicWire.provider(s.URL), harness.NewMemJournal(), rewrite{}).Run(context.Background(), prompt("x")); err != nil {
		t.Fatal(err)
	}
	got := content(t, messages(t, s.requests()[1])[1])
	if strings.Contains(got, "thinking") || strings.Contains(got, "note") || !strings.Contains(got, `"text":"clean"`) {
		t.Fatalf("%s", got)
	}
}

type lengthen struct{}

func (lengthen) Name() string { return "lengthen" }
func (lengthen) AfterModel(_ context.Context, _ harness.Run, r model.Response) (model.Response, error) {
	r.Text += " (reviewed)"
	return r, nil
}

// The saved order still fits as far as it goes, and the words a hook added would be
// lost if a replay stopped there.
func TestALengthenedReplyKeepsItsNewWords(t *testing.T) {
	s := serve(t, thinkingThenCall, anthropicWire.text("done"))
	if _, err := agent(anthropicWire.provider(s.URL), harness.NewMemJournal(), lengthen{}).Run(context.Background(), prompt("x")); err != nil {
		t.Fatal(err)
	}
	got := content(t, messages(t, s.requests()[1])[1])
	if strings.Contains(got, "thinking") || !strings.Contains(got, "(reviewed)") {
		t.Fatalf("%s", got)
	}
}

const refused = "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":" +
	"\"messages.1.content.0: Invalid `signature` in `thinking` block. The block is bound to a different conversation.\"}}"

// A history edit leaves signatures invalid and retrying the same body fails forever.
// The one recovery that needs no beta is to send the turn again without thinking.
func TestARefusedSignatureIsSentAgainWithoutThinking(t *testing.T) {
	s := serve(t, thinkingThenCall, status(400, refused), anthropicWire.text("done"))
	j := harness.NewMemJournal()
	res, err := agent(anthropicWire.provider(s.URL), j).Run(context.Background(), prompt("x"))
	if err != nil || res.Output != "done" || res.ModelCalls != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	if f := failures(t, j); len(f) != 0 {
		t.Fatalf("the run saw a failure: %+v", f)
	}
	reqs := s.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	if got := content(t, messages(t, reqs[1])[1]); got != wantReplay {
		t.Fatalf("the first try must send thinking: %s", got)
	}
	want := `[{"type":"text","text":"note "},{"type":"text","text":"more"},{"type":"tool_use","id":"c1","name":"look","input":{}}]`
	if got := content(t, messages(t, reqs[2])[1]); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestARefusalWithoutThinkingIsTriedOnceAndThenFails(t *testing.T) {
	s := serve(t, thinkingThenCall, status(400, refused), status(400, refused), anthropicWire.text("never"))
	j := harness.NewMemJournal()
	if _, err := agent(anthropicWire.provider(s.URL), j, retries{}).Run(context.Background(), prompt("x")); err == nil {
		t.Fatal("want a failure")
	}
	if n := len(s.requests()); n != 3 {
		t.Fatalf("%d requests", n)
	}
	if f := failures(t, j); len(f) != 1 || f[0].Class != model.ClassInvalid {
		t.Fatalf("%+v", f)
	}
}

func TestAnotherProvidersStateIsLeftAlone(t *testing.T) {
	foreign := []model.Message{model.User("x"), {Role: model.RoleAssistant, Content: "said", Opaque: json.RawMessage(`{"responses":"r-1"}`)}, model.User("y")}
	s := serve(t, anthropicWire.text("ok"), openaiWire.text("ok"))
	if _, err := model.Collect(anthropicWire.provider(s.URL).Complete(context.Background(), model.Request{Messages: foreign}), nil); err != nil {
		t.Fatal(err)
	}
	if got := content(t, messages(t, s.requests()[0])[1]); got != `[{"type":"text","text":"said"}]` {
		t.Fatal(got)
	}
	foreign[1].Opaque = json.RawMessage(`{"anthropic":[{"type":"thinking","thinking":"t","signature":"s"}]}`)
	if _, err := model.Collect(openaiWire.provider(s.URL+"/v1").Complete(context.Background(), model.Request{Messages: foreign}), nil); err != nil {
		t.Fatal(err)
	}
	if b := s.requests()[1]; bytes.Contains(b, []byte("signature")) || bytes.Contains(b, []byte("opaque")) {
		t.Fatalf("%s", b)
	}
}

func TestASecretInThinkingLeavesTheJournal(t *testing.T) {
	s := serve(t, stream(
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"remember `+secret+`"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"s"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c1","name":"look"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`), status(400, refused), anthropicWire.text("done"))
	j := harness.NewMemJournal()
	if res, err := agent(anthropicWire.provider(s.URL), j, scrubber{}).Run(context.Background(), prompt("x")); err != nil || res.Output != "done" {
		t.Fatalf("%v %+v", err, res)
	}
	if bytes.Contains(journalOf(t, j), []byte(secret)) {
		t.Fatal("the journal holds the secret")
	}
	reqs := s.requests()
	if bytes.Contains(reqs[1], []byte(secret)) || !bytes.Contains(reqs[1], []byte("remember [ahv]")) {
		t.Fatalf("the replay must carry the redacted text: %s", reqs[1])
	}
}

type quoter struct{}

func (quoter) Name() string { return "quoter" }
func (quoter) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, "first thought", `"`), nil
}

// A journal cannot store bytes that are not JSON, so this must end the run, not crash
// the worker on every retry.
func TestARedactorThatBreaksProviderStateEndsTheRun(t *testing.T) {
	s := serve(t, thinkingThenCall)
	j := &jsonJournal{runs: map[string][][]byte{}}
	res, err := agent(anthropicWire.provider(s.URL), j, quoter{}).Run(context.Background(), prompt("x"))
	if res == nil || res.State != harness.StateFailed || errors.Is(err, harness.ErrDrained) {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestAnthropicStreamEndings(t *testing.T) {
	billed := `{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":1}}}`
	for name, c := range map[string]struct {
		step  step
		class model.ErrorClass
	}{
		"overloaded": {stream(billed, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), model.ClassTransient},
		"limited":    {stream(billed, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`), model.ClassRateLimit},
		"billing":    {stream(billed, `{"type":"error","error":{"type":"billing_error","message":"no credit"}}`), model.ClassAuth},
		"no detail":  {stream(billed, `{"type":"error"}`), model.ClassTransient},
		"bad event":  {stream(billed, `{not json`), model.ClassTransient},
		"window full": {stream(billed, `{"type":"message_delta","delta":{"stop_reason":"model_context_window_exceeded"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`), model.ClassContextLength},
	} {
		t.Run(name, func(t *testing.T) {
			s := serve(t, c.step)
			_, err := model.Collect(anthropicWire.provider(s.URL).Complete(context.Background(), model.Request{}), nil)
			e := model.AsError(err)
			if err == nil || e.Class != c.class || e.Usage.Input != 7 {
				t.Fatalf("%v %+v", err, e)
			}
		})
	}
	s := serve(t, stream(billed, `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":1}}`, `{"type":"message_stop"}`))
	resp, err := model.Collect(anthropicWire.provider(s.URL).Complete(context.Background(), model.Request{}), nil)
	if err != nil || resp.Finish != model.FinishRefusal {
		t.Fatalf("%v %+v", err, resp)
	}
}

func TestAnthropicErrorTypes(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		class  model.ErrorClass
	}{
		{402, `{"error":{"type":"billing_error","message":"no credit"}}`, model.ClassAuth},
		{403, `{"error":{"type":"permission_error","message":"no"}}`, model.ClassAuth},
		{404, `{"error":{"type":"not_found_error","message":"model: x"}}`, model.ClassInvalid},
		{413, `{"error":{"type":"request_too_large","message":"too big"}}`, model.ClassContextLength},
		{400, `{"error":{"type":"invalid_request_error","message":"roles must alternate"}}`, model.ClassInvalid},
		{529, `{"error":{"type":"overloaded_error","message":"busy"}}`, model.ClassTransient},
		{500, `{"error":{"type":"api_error","message":"oops"}}`, model.ClassTransient},
		{502, `<html>bad gateway</html>`, model.ClassTransient},
	} {
		s := serve(t, status(c.status, c.body))
		_, err := model.Collect(anthropicWire.provider(s.URL).Complete(context.Background(), model.Request{}), nil)
		var se *anthropic.StatusError
		if e := model.AsError(err); e.Class != c.class || !errors.As(err, &se) || se.Code != c.status {
			t.Errorf("%d %s: %v", c.status, c.body, err)
		}
	}
}

func TestOpenAIErrorShapes(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		class  model.ErrorClass
	}{
		{"an empty account is no rate limit", 429, `{"error":{"message":"quota","type":"insufficient_quota","code":"insufficient_quota"}}`, model.ClassAuth},
		{"a numeric code", 429, `{"error":{"message":"slow","code":429}}`, model.ClassRateLimit},
		{"error as a string", 500, `{"error":"boom"}`, model.ClassTransient},
		{"a compatible server's wording", 400, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, model.ClassContextLength},
		{"a numeric code on a full context", 400, `{"error":{"message":"maximum context length exceeded","code":400}}`, model.ClassContextLength},
		{"plain invalid", 400, `{"error":{"message":"bad","type":"invalid_request_error","code":null}}`, model.ClassInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := serve(t, status(c.status, c.body))
			_, err := model.Collect(openaiWire.provider(s.URL).Complete(context.Background(), model.Request{}), nil)
			if e := model.AsError(err); e.Class != c.class {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestOpenAIStreamEndings(t *testing.T) {
	collect := func(t *testing.T, st step) (model.Response, error) {
		t.Helper()
		s := serve(t, st)
		return model.Collect(openaiWire.provider(s.URL).Complete(context.Background(), model.Request{}), nil)
	}
	t.Run("a reason with no DONE is complete", func(t *testing.T) {
		resp, err := collect(t, stream(`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`))
		if err != nil || resp.Text != "hi" {
			t.Fatalf("%v %+v", err, resp)
		}
	})
	t.Run("DONE with no reason is cut", func(t *testing.T) {
		_, err := collect(t, stream(`{"choices":[{"delta":{"content":"hi"}}]}`, `[DONE]`))
		if e := model.AsError(err); err == nil || e.Class != model.ClassTransient {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an unknown reason is worked out from the reply", func(t *testing.T) {
		resp, err := collect(t, stream(`{"choices":[{"delta":{"content":"hi"},"finish_reason":"eos"}]}`))
		if err != nil || resp.Finish != model.FinishStop {
			t.Fatalf("%v %+v", err, resp)
		}
	})
	t.Run("an error in the stream keeps what was billed", func(t *testing.T) {
		_, err := collect(t, stream(`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1}}`, `{"error":{"message":"boom","type":"server_error","code":"server_error"}}`))
		if e := model.AsError(err); err == nil || e.Class != model.ClassTransient || e.Usage.Input != 9 {
			t.Fatalf("%v %+v", err, e)
		}
	})
	t.Run("a chunk that is not JSON", func(t *testing.T) {
		_, err := collect(t, stream(`{not json`))
		if e := model.AsError(err); err == nil || e.Class != model.ClassTransient {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an error as a string in the stream", func(t *testing.T) {
		_, err := collect(t, stream(`{"error":"boom"}`))
		if e := model.AsError(err); err == nil || e.Class != model.ClassTransient || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("%v", err)
		}
	})
}

// A connection that never answers is transient on both.
func TestAConnectionFailureIsTransient(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := httptest.NewServer(http.NotFoundHandler())
		url := s.URL
		s.Close()
		_, err := model.Collect(w.provider(url).Complete(context.Background(), model.Request{}), nil)
		if e := model.AsError(err); err == nil || e.Class != model.ClassTransient {
			t.Fatalf("%v", err)
		}
	})
}

// A worker stopped under a live stream says nothing about the run, so no failure is
// recorded and the next worker makes the call again.
func TestADrainDuringAStreamIsNotAFailure(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		release := make(chan struct{})
		defer close(release)
		reached := make(chan struct{})
		hang := func(rw http.ResponseWriter) {
			rw.WriteHeader(http.StatusOK)
			rw.(http.Flusher).Flush()
			close(reached)
			<-release
		}
		s := serve(t, hang, w.text("done"))
		j := harness.NewMemJournal()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-reached
			cancel()
		}()
		if _, err := agent(w.provider(s.URL), j, retries{}).Run(ctx, prompt("x")); !errors.Is(err, harness.ErrDrained) {
			t.Fatalf("got %v", err)
		}
		if f := failures(t, j); len(f) != 0 {
			t.Fatalf("%+v", f)
		}
		res, err := agent(w.provider(s.URL), j, retries{}).Run(context.Background(), harness.Resume("r"))
		if err != nil || res.Output != "done" {
			t.Fatalf("%v %+v", err, res)
		}
	})
}

// What a saved order does with a message that changed, or that has less than usual.
func TestAnAssistantMessageGoesBackAsItsOrderAllows(t *testing.T) {
	thinking := `{"type":"thinking","signature":"s"}`
	for name, c := range map[string]struct {
		msg  model.Message
		want string
	}{
		"a block with no text still sends the field": {
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a", Name: "look", Arguments: json.RawMessage(`{}`)}},
				Opaque: json.RawMessage(`{"anthropic":[` + thinking + `,{"type":"tool_use"}]}`)},
			`[{"type":"thinking","thinking":"","signature":"s"},{"type":"tool_use","id":"a","name":"look","input":{}}]`,
		},
		"a call a hook added is not lost": {
			model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a", Name: "look", Arguments: json.RawMessage(`{}`)}, {ID: "b", Name: "look", Arguments: json.RawMessage(`{}`)}},
				Opaque: json.RawMessage(`{"anthropic":[` + thinking + `,{"type":"tool_use"}]}`)},
			`[{"type":"tool_use","id":"a","name":"look","input":{}},{"type":"tool_use","id":"b","name":"look","input":{}}]`,
		},
		"a call a hook removed is not invented": {
			model.Message{Role: model.RoleAssistant, Content: "only words",
				Opaque: json.RawMessage(`{"anthropic":[` + thinking + `,{"type":"tool_use"}]}`)},
			`[{"type":"text","text":"only words"}]`,
		},
		"state that is not an order": {
			model.Message{Role: model.RoleAssistant, Content: "said", Opaque: json.RawMessage(`[1,2]`)},
			`[{"type":"text","text":"said"}]`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := serve(t, anthropicWire.text("ok"))
			req := model.Request{Messages: []model.Message{model.User("x"), c.msg, model.User("y")}}
			if _, err := model.Collect(anthropicWire.provider(s.URL).Complete(context.Background(), req), nil); err != nil {
				t.Fatal(err)
			}
			if got := content(t, messages(t, s.requests()[0])[1]); got != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}
