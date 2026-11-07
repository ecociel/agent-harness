package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ecociel/agent-harness"
)

// Result is one scenario's outcome. A scenario that broke instead of being graded
// carries Err, and counts as neither a pass nor a failure.
type Result struct {
	Scenario string        `json:"scenario"`
	Category string        `json:"category,omitempty"`
	Channel  string        `json:"channel,omitempty"`
	Pass     bool          `json:"pass"`
	Failed   []Failure     `json:"failed,omitempty"`
	Err      string        `json:"error,omitempty"`
	Answer   string        `json:"answer"`
	Tools    []string      `json:"tools"`
	Spend    harness.Spend `json:"spend"`
	Latency  time.Duration `json:"latency_ns"`
}

// Tally counts passes out of graded scenarios.
type Tally struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

// ScenarioReport aggregates a run over scenarios. Its JSON is report.json.
type ScenarioReport struct {
	Title    string        `json:"title,omitempty"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration_ns"`
	// LatencyP50 is the median scenario latency, each the sum of its turns.
	LatencyP50 time.Duration `json:"latency_p50_ns"`
	// Errors counts the scenarios that broke instead of being graded.
	Errors     int              `json:"errors"`
	Categories map[string]Tally `json:"categories"`
	Channels   map[string]Tally `json:"channels,omitempty"`
	Spend      harness.Spend    `json:"spend"`
	Results    []Result         `json:"results"`
}

func (rep *ScenarioReport) add(r Result) {
	rep.Results = append(rep.Results, r)
	rep.Spend = rep.Spend.Add(r.Spend)
	if r.Err != "" {
		rep.Errors++
		return
	}
	if rep.Categories == nil {
		rep.Categories = map[string]Tally{}
	}
	rep.Categories[r.Category] = rep.Categories[r.Category].add(r.Pass)
	if r.Channel != "" {
		if rep.Channels == nil {
			rep.Channels = map[string]Tally{}
		}
		rep.Channels[r.Channel] = rep.Channels[r.Channel].add(r.Pass)
	}
}

func (rep *ScenarioReport) finish() {
	rep.Duration = time.Since(rep.Started)
	lat := make([]time.Duration, len(rep.Results))
	for i, r := range rep.Results {
		lat[i] = r.Latency
	}
	slices.Sort(lat)
	if len(lat) > 0 {
		rep.LatencyP50 = lat[len(lat)/2]
	}
}

func (t Tally) add(pass bool) Tally {
	t.Total++
	if pass {
		t.Passed++
	}
	return t
}

// Redacted returns a copy whose answers, errors and failure notes went through
// redact, and how many of those texts it changed. The rules are the consumer's: a
// report holds what a model wrote, which can carry a credential.
func (rep ScenarioReport) Redacted(redact func(string) string) (ScenarioReport, int) {
	masked := 0
	mask := func(s string) string {
		out := redact(s)
		if out != s {
			masked++
		}
		return out
	}
	clean := rep
	clean.Results = make([]Result, len(rep.Results))
	for i, r := range rep.Results {
		r.Answer, r.Err = mask(r.Answer), mask(r.Err)
		r.Failed = slices.Clone(r.Failed)
		for j := range r.Failed {
			r.Failed[j].Why = mask(r.Failed[j].Why)
		}
		clean.Results[i] = r
	}
	return clean, masked
}

// Write stores the report as report.json and summary.md in dir, through redact
// when it is not nil, and returns how many texts redact changed.
func (rep ScenarioReport) Write(dir string, redact func(string) string) (int, error) {
	masked := 0
	if redact != nil {
		rep, masked = rep.Redacted(redact)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return masked, err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return masked, err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(raw, '\n'), 0o600); err != nil {
		return masked, err
	}
	return masked, os.WriteFile(filepath.Join(dir, "summary.md"), []byte(rep.Markdown()), 0o600)
}

// Markdown renders the report as summary.md.
func (rep ScenarioReport) Markdown() string {
	var b strings.Builder
	b.WriteString("# Eval")
	if rep.Title != "" {
		b.WriteString(": " + rep.Title)
	}
	fmt.Fprintf(&b, "\n\nRun %s, %.0f s, median scenario latency %.1f s.\n\n",
		rep.Started.UTC().Format(time.RFC3339), rep.Duration.Seconds(), rep.LatencyP50.Seconds())
	fmt.Fprintf(&b, "Scenarios that broke instead of being graded (left out of the counts): %d.\n\n", rep.Errors)
	tally(&b, "Category", rep.Categories)
	tally(&b, "Channel", rep.Channels)
	u := rep.Spend.Usage
	fmt.Fprintf(&b, "Model calls %d, tool calls %d; tokens: input %d, cached %d, output %d.\n\n",
		rep.Spend.ModelCalls, rep.Spend.ToolCalls, u.Input, u.Cached, u.Output)
	b.WriteString("| Scenario | Result | Latency | Tokens in / out | Why |\n|---|---|---|---|---|\n")
	for _, r := range rep.Results {
		verdict, why := "pass", ""
		switch {
		case r.Err != "":
			verdict, why = "error", r.Err
		case !r.Pass:
			notes := make([]string, len(r.Failed))
			for i, f := range r.Failed {
				notes[i] = f.String()
			}
			verdict, why = "FAIL", strings.Join(notes, "; ")
		}
		why = strings.ReplaceAll(strings.ReplaceAll(why, "|", `\|`), "\n", " ")
		fmt.Fprintf(&b, "| %s | %s | %.1f s | %d / %d | %s |\n", r.Scenario, verdict, r.Latency.Seconds(),
			r.Spend.Usage.Input, r.Spend.Usage.Output, why)
	}
	return b.String()
}

func tally(b *strings.Builder, title string, ts map[string]Tally) {
	if len(ts) == 0 {
		return
	}
	fmt.Fprintf(b, "| %s | Passed |\n|---|---|\n", title)
	names := make([]string, 0, len(ts))
	for name := range ts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		label := name
		if label == "" {
			label = "(none)"
		}
		fmt.Fprintf(b, "| %s | %d / %d |\n", label, ts[name].Passed, ts[name].Total)
	}
	b.WriteString("\n")
}
