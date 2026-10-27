// Command webchat is a small web chat for trying agent-harness, with a
// diagnostic view that shows every state transition and hook invocation of
// each turn as it happens.
//
//	go run ./examples/webchat            # scripted demo model, no keys
//	HARNESS_PROVIDER=openai OPENAI_API_KEY=… go run ./examples/webchat
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/webchat
//
// Then open http://localhost:8080. Everything (HTML, CSS, JS, fonts) is
// embedded in the binary; the page works offline.
//
// Environment:
//
//	HARNESS_PROVIDER   fake (default), openai or anthropic
//	OPENAI_API_KEY     key for OpenAI or any OpenAI-compatible endpoint
//	OPENAI_BASE_URL    default https://api.openai.com/v1 (e.g. Ollama: http://localhost:11434/v1)
//	OPENAI_MODEL       default gpt-4.1-mini
//	ANTHROPIC_API_KEY  key for the Anthropic Messages API
//	ANTHROPIC_MODEL    default claude-sonnet-4-5
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/anthropic"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/provider/openai"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	verbose := flag.Bool("v", false, "log every hook invocation to stderr")
	flag.Parse()

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	p, modelName, label, err := providerFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webchat:", err)
		os.Exit(2)
	}
	srv := &http.Server{Addr: *addr, Handler: NewServer(p, modelName, label, log), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Printf("agent-harness webchat · model: %s\nopen http://%s\n", label, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "webchat:", err)
		os.Exit(1)
	}
}

// providerFromEnv picks the model provider. The default is the scripted demo
// brain, wrapped so that messages containing "flaky" fail once (to show the
// retry hook).
func providerFromEnv(getenv func(string) string) (model.Provider, string, string, error) {
	switch strings.ToLower(getenv("HARNESS_PROVIDER")) {
	case "", "fake", "demo":
		f := fake.NewFunc(brain)
		f.Delay = 250 * time.Millisecond
		return &flaky{next: f}, "demo-scripted", "scripted demo model (no key)", nil
	case "openai":
		p := &openai.Provider{BaseURL: getenv("OPENAI_BASE_URL"), APIKey: getenv("OPENAI_API_KEY")}
		if p.BaseURL == "" {
			p.BaseURL = "https://api.openai.com/v1"
		}
		if p.APIKey == "" && !strings.Contains(p.BaseURL, "localhost") && !strings.Contains(p.BaseURL, "127.0.0.1") {
			return nil, "", "", errors.New("HARNESS_PROVIDER=openai needs OPENAI_API_KEY (or a local OPENAI_BASE_URL)")
		}
		m := getenv("OPENAI_MODEL")
		if m == "" {
			m = "gpt-4.1-mini"
		}
		return p, m, "openai-compatible · " + m, nil
	case "anthropic":
		p := &anthropic.Provider{BaseURL: getenv("ANTHROPIC_BASE_URL"), APIKey: getenv("ANTHROPIC_API_KEY")}
		if p.APIKey == "" {
			return nil, "", "", errors.New("HARNESS_PROVIDER=anthropic needs ANTHROPIC_API_KEY")
		}
		m := getenv("ANTHROPIC_MODEL")
		if m == "" {
			m = "claude-sonnet-4-5"
		}
		return p, m, "anthropic · " + m, nil
	}
	return nil, "", "", fmt.Errorf("unknown HARNESS_PROVIDER %q (want fake, openai or anthropic)", getenv("HARNESS_PROVIDER"))
}

// flaky fails the first model call of every distinct request whose last
// user message mentions "flaky", so the retry hook has work to do.
type flaky struct {
	next model.Provider
	mu   sync.Mutex
	seen map[string]bool
}

var errFlaky = errors.New("demo: transient upstream error (503), the retry hook will try again")

func (f *flaky) Complete(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
	u := model.LastUser(req.Messages)
	if strings.Contains(strings.ToLower(u), "flaky") {
		key := fmt.Sprintf("%d|%s", len(req.Messages), u)
		f.mu.Lock()
		if f.seen == nil {
			f.seen = map[string]bool{}
		}
		first := !f.seen[key]
		f.seen[key] = true
		f.mu.Unlock()
		if first {
			return model.Fail(errFlaky)
		}
	}
	return f.next.Complete(ctx, req)
}
