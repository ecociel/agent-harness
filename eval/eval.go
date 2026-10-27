// Package eval runs a Runner against a set of tasks and scores the results —
// the "private evals define what good looks like" half of the harness talk.
//
// A Task is shaped like a Harbor task: an instruction (the prompt) and a
// verifier (Check) that scores the result, which can be code, a string
// match, or another agent acting as judge. Report tracks accuracy together
// with tokens and latency, because a harness change that is more accurate
// but three times as expensive is not obviously better.
package eval

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Check scores one result between 0 and 1 and explains why.
type Check func(ctx context.Context, res *harness.Result) (score float64, note string)

// Task is one eval case.
type Task struct {
	Name        string
	Instruction string
	Check       Check
}

// TaskResult is the outcome of one Task.
type TaskResult struct {
	Task     string        `json:"task"`
	Score    float64       `json:"score"`
	Note     string        `json:"note"`
	Stop     string        `json:"stop"`
	Output   string        `json:"output"`
	Usage    model.Usage   `json:"usage"`
	Duration time.Duration `json:"duration_ns"`
	Err      string        `json:"error,omitempty"`
}

// Report aggregates a run over all tasks.
type Report struct {
	Results  []TaskResult  `json:"results"`
	Mean     float64       `json:"mean"`
	Usage    model.Usage   `json:"usage"`
	Duration time.Duration `json:"duration_ns"`
}

// Run evaluates r on tasks with up to parallel tasks at once (default 4).
func Run(ctx context.Context, r harness.Runner, tasks []Task, parallel int) Report {
	if parallel <= 0 {
		parallel = 4
	}
	start := time.Now()
	out := make([]TaskResult, len(tasks))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			res, err := r.Run(ctx, harness.Prompt(t.Instruction))
			tr := TaskResult{Task: t.Name, Duration: time.Since(t0)}
			if res != nil {
				tr.Stop, tr.Output, tr.Usage = string(res.Stop), res.Output, res.Usage
			}
			if err != nil {
				tr.Err, tr.Note = err.Error(), "run failed"
			} else if t.Check != nil {
				tr.Score, tr.Note = t.Check(ctx, res)
			}
			out[i] = tr
		}()
	}
	wg.Wait()
	rep := Report{Results: out, Duration: time.Since(start)}
	for _, tr := range out {
		rep.Mean += tr.Score
		rep.Usage = rep.Usage.Add(tr.Usage)
	}
	if len(out) > 0 {
		rep.Mean /= float64(len(out))
	}
	return rep
}

// Contains scores 1 when the output contains want (case-insensitive) and the
// run completed.
func Contains(want string) Check {
	return func(_ context.Context, res *harness.Result) (float64, string) {
		if !res.OK() {
			return 0, "run did not complete: " + string(res.Stop)
		}
		if strings.Contains(strings.ToLower(res.Output), strings.ToLower(want)) {
			return 1, "contains " + strconv.Quote(want)
		}
		return 0, "missing " + strconv.Quote(want)
	}
}

// StoppedWith scores 1 when the run stopped for reason (e.g. a guardrail
// eval: "this prompt must be refused").
func StoppedWith(reason harness.StopReason) Check {
	return func(_ context.Context, res *harness.Result) (float64, string) {
		if res.Stop == reason {
			return 1, "stopped with " + string(reason)
		}
		return 0, fmt.Sprintf("stopped with %s, want %s", res.Stop, reason)
	}
}

var scoreRE = regexp.MustCompile(`(?i)score\s*[:=]\s*([01](?:\.\d+)?)`)

// Judge asks another Runner to grade the output against rubric; it must
// answer "SCORE: <0..1>".
func Judge(judge harness.Runner, rubric string) Check {
	return func(ctx context.Context, res *harness.Result) (float64, string) {
		v, err := judge.Run(ctx, harness.Prompt(fmt.Sprintf("Rubric:\n%s\n\nAnswer to grade:\n%s\n\n"+
			"Reply 'SCORE: <number between 0 and 1>' and one sentence why.", rubric, res.Output)))
		if err != nil {
			return 0, "judge failed: " + err.Error()
		}
		m := scoreRE.FindStringSubmatch(v.Output)
		if m == nil {
			return 0, "judge gave no score: " + v.Output
		}
		s, _ := strconv.ParseFloat(m[1], 64)
		return s, v.Output
	}
}
