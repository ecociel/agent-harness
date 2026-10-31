package hooks_test

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
)

var binaries string

func TestMain(m *testing.M) {
	code, err := buildAndRun(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func buildAndRun(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "hook-examples")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	if out, err := exec.Command("go", "build", "-o", dir+string(filepath.Separator), "./...").CombinedOutput(); err != nil {
		return 0, fmt.Errorf("go build: %v\n%s", err, out)
	}
	binaries = dir
	return m.Run(), nil
}

// examples lists every directory here that holds a main.go.
func examples(t *testing.T) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "main.go" {
			dirs = append(dirs, filepath.ToSlash(filepath.Dir(path)))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// run starts the example with HARNESS_PROVIDER cleared and extra added.
func run(dir string, extra ...string) (string, error) {
	cmd := exec.Command(filepath.Join(binaries, filepath.Base(dir)))
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "HARNESS_PROVIDER=")
	}), extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestOutputMatchesGolden(t *testing.T) {
	for _, dir := range examples(t) {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			got, err := run(dir)
			if err != nil {
				t.Fatalf("%v\n%s", err, got)
			}
			path := filepath.Join(dir, "output.txt")
			if os.Getenv("UPDATE_DOCS") != "" {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("%s is out of date: run UPDATE_DOCS=1 go test ./examples/hooks\ngot:\n%s", path, got)
			}
		})
	}
}

func TestEveryPointHasAnExample(t *testing.T) {
	have := examples(t)
	for _, p := range harness.Points() {
		if dir := "points/" + strings.ToLower(string(p)); !slices.Contains(have, dir) {
			t.Errorf("no example at examples/hooks/%s", dir)
		}
	}
}

// A model that ignores the tools and says "hi" takes every example off its script.
func TestEveryExampleRunsOnARealProvider(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(stub.Close)
	for _, dir := range examples(t) {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			got, err := run(dir, "HARNESS_PROVIDER=openai", "OPENAI_BASE_URL="+stub.URL, "OPENAI_MODEL=stub-model")
			if err != nil {
				t.Fatalf("%v\n%s", err, got)
			}
			if !strings.Contains(got, "model: openai-compatible · stub-model\n") {
				t.Fatalf("the banner does not name stub-model:\n%s", got)
			}
		})
	}
}
