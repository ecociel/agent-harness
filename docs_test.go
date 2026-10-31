package harness_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
)

// transitionTable renders the machine's edges, one row per state and event.
func transitionTable() string {
	type key struct {
		from harness.State
		on   harness.Kind
	}
	var rows []key
	to := map[key][]string{}
	for _, e := range harness.Transitions() {
		if e.On == harness.KindRunClaimed || e.On == harness.KindRunStopping {
			continue // the prose below the table says every live state takes these
		}
		k := key{e.From, e.On}
		if !slices.Contains(rows, k) {
			rows = append(rows, k)
		}
		if name := "`" + string(e.To) + "`"; !slices.Contains(to[k], name) {
			to[k] = append(to[k], name)
		}
	}
	var b strings.Builder
	b.WriteString("| From | Event | To |\n|---|---|---|\n")
	for _, k := range rows {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", k.from, k.on, strings.Join(to[k], ", "))
	}
	return b.String()
}

func TestTheDocsMatchTheTable(t *testing.T) {
	const path = "docs/state-machine.md"
	const open, end = "<!-- transitions -->\n", "<!-- /transitions -->"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	before, rest, ok := strings.Cut(doc, open)
	if !ok {
		t.Fatal("no table marker")
	}
	got, after, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatal("no end marker")
	}
	want := transitionTable()
	if os.Getenv("UPDATE_DOCS") != "" {
		if err := os.WriteFile(path, []byte(before+open+want+end+after), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if got != want {
		t.Fatalf("%s is out of date: run UPDATE_DOCS=1 go test -run TestTheDocsMatchTheTable .", path)
	}
}

// Every state the code knows is explained in the page, and every plugin point.
func TestTheDocsNameEveryStateAndPoint(t *testing.T) {
	for path, names := range map[string][]string{
		"docs/state-machine.md": stateNames(),
		"docs/hooks.md":         pointNames(),
		"README.md":             pointNames(),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if !strings.Contains(string(raw), "`"+n+"`") {
				t.Errorf("%s does not mention %s", path, n)
			}
		}
	}
}

func stateNames() []string {
	var out []string
	for _, s := range harness.States() {
		out = append(out, string(s))
	}
	return out
}

func pointNames() []string {
	var out []string
	for _, p := range harness.Points() {
		out = append(out, string(p))
	}
	return out
}
