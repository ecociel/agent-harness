package main

import (
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/tool"
)

// Pattern describes one choice in the UI's pattern selector.
type Pattern struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Summary string `json:"summary"`
	Source  string `json:"source"`
	Try     string `json:"try"`
}

var patterns = []Pattern{
	{"single", "Single agent", "One agent, the full inner/outer loop, all tools and every hook.", "Harrison Chase, “When to build your own agent harness”", "What time is it?"},
	{"sequential", "Sequential", "planner → writer → reviewer pipeline; each step's output feeds the next.", "ecociel/agents sequential; Cursor planner→implementer→verifier subagents", "Explain the inner loop"},
	{"parallel", "Parallel", "Security, cost and facts specialists review at once; outputs are joined.", "Cursor “parallel agents”; ecociel/agents parallel", "Review my plan to add a delete tool"},
	{"best_of_n", "Best-of-N + judge", "Three candidates answer; a judge picks the winner.", "Cursor “parallel agents, judge” / multiple models; tessmir critic", "Describe a hook in one line"},
	{"router", "Router", "A classifier sends the request to assistant, billing or small talk.", "ecociel/agents routing; tessmir routing", "Where is my invoice?"},
	{"subagent", "Subagent (orchestrator)", "A lead delegates to a researcher sub-agent with a fresh context, via a tool call.", "Cursor subagents (foreground); Dani Zhu “hierarchical delegation”", "What does the budget hook do?"},
	{"background", "Background subagent", "The lead starts a background job and collects it later.", "Cursor background subagents", "Research compaction"},
	{"planner_worker", "Planner / worker / judge", "A planner writes TASK lines, isolated workers do them in parallel, a judge decides whether to continue.", "Cursor “Scaling agents” and “Self-driving codebases”", "Write release notes for the harness"},
	{"evaluator_optimizer", "Evaluator–optimizer", "A writer drafts, a critic answers APPROVED or REVISE:, repeat.", "ecociel/agents critic; tessmir verified; Anthropic “building effective agents”", "Explain guardrails"},
	{"handoff", "Handoff", "triage transfers the conversation to billing or tech; control does not come back.", "ecociel/agents handoff; Dani Zhu “programmed workflows”", "My invoice page crashes with an error"},
	{"team", "Shared-state team", "Two peers work through a task list in rounds and add follow-ups.", "Dani Zhu “shared-state teams”; Cursor “Scaling agents”", "Audit the hook docs"},
	{"debate", "Debate", "Two debaters answer, read each other, revise; a judge picks.", "ecociel/agents ensemble; multi-agent debate literature", "Are explicit state machines worth it?"},
}

// build assembles the runner for a pattern. mk creates an agent with the
// shared provider and the full hook stack.
func build(id string, mk func(name, instructions string, tools *tool.Registry) *harness.Agent, tools *tool.Registry, j harness.Journal) (harness.Runner, error) {
	plain := func(name, instr string) *harness.Agent { return mk(name, instr, nil) }
	durable := func(a *harness.Agent) *harness.Agent {
		a.Journal = j
		return a
	}
	switch id {
	case "single", "":
		return durable(mk("assistant", assistantPrompt, tools)), nil
	case "sequential":
		return &multi.Sequential{Name: "pipeline", Journal: j, Steps: []harness.Runner{
			plain("planner", "You are the pipeline planner. Write a three-step plan for answering the request. Do not answer it."),
			plain("writer", "You are the writer. Follow the plan and answer the original request in two or three sentences."),
			plain("reviewer", "You are the reviewer. Tighten the draft, keep it correct, and add one caveat."),
		}}, nil
	case "parallel":
		var bs []harness.Runner
		for _, r := range []string{"security", "cost", "facts"} {
			bs = append(bs, plain(r, fmt.Sprintf("You are the %s specialist. Review the request from that angle only, in one or two sentences.", r)))
		}
		return &multi.Parallel{Name: "review-panel", Journal: j, Branches: bs}, nil
	case "best_of_n":
		var cs []harness.Runner
		for _, s := range []string{"a", "b", "c"} {
			cs = append(cs, plain("candidate-"+s, "You are candidate "+s+". Answer the request well; "+map[string]string{"a": "be terse.", "b": "be thorough.", "c": "be playful."}[s]))
		}
		return &multi.BestOfN{Name: "best-of-3", Journal: j, Candidates: cs, Judge: plain("judge", "You are the judge. Compare answers strictly and follow the reply format you are given."), Criteria: "correctness and completeness"}, nil
	case "router":
		return &multi.Router{Name: "router", Journal: j,
			Classifier: plain("classifier", "You are the classifier. Reply with one label only."),
			Routes: map[string]harness.Runner{
				"assistant": mk("assistant", assistantPrompt, tools),
				"billing":   plain("billing", billingPrompt),
				"smalltalk": plain("smalltalk", "You are smalltalk: a friendly greeter. Keep it to one sentence."),
			},
			Fallback: plain("smalltalk", "You are smalltalk: a friendly greeter. Keep it to one sentence."),
		}, nil
	case "subagent":
		lead := mk("lead", "You are the lead. Delegate research to the researcher tool with a complete, self-contained task, then answer from its report.", nil)
		return multi.WithSubagents(durable(lead), multi.Subagent{Name: "researcher", Description: "Researches a question and reports in two sentences.",
			Agent: plain("researcher", researcherPrompt)})
	case "background":
		bg := multi.NewBackground(multi.Subagent{Name: "researcher", Description: "Researches a question and reports in two sentences.",
			Agent: plain("researcher", researcherPrompt)})
		return durable(mk("lead", "You are the lead with background helpers. Start research in the background with start_researcher, then collect the job (wait=true) and answer.",
			tool.NewRegistry(bg.Tools()...))), nil
	case "planner_worker":
		return &multi.PlannerWorker{Name: "planner-worker", Journal: j,
			Planner:     plain("planner", "You plan work for others and never do it yourself."),
			Worker:      plain("worker", "You are a worker. Do exactly your task and write a handoff report."),
			Judge:       plain("judge", "You are the judge. Decide whether the goal is achieved."),
			Synthesizer: plain("synthesizer", "You turn worker reports into the final answer."),
			MaxRounds:   2,
		}, nil
	case "evaluator_optimizer":
		return &multi.EvaluatorOptimizer{Name: "writer-critic", Journal: j,
			Generator: plain("writer", "You are the writer. Answer the request in two or three sentences; when given reviewer feedback, revise."),
			Evaluator: plain("critic", "You are the critic. Be strict and follow the reply format you are given."),
		}, nil
	case "handoff":
		return &multi.Handoff{Name: "support-desk", Journal: j, Sticky: true, Agents: []*harness.Agent{
			plain("triage", "You are triage: you find out what the user needs and transfer them to billing or tech."),
			plain("billing", billingPrompt),
			plain("tech", "You are tech support: you fix technical problems and bugs."),
		}}, nil
	case "team":
		return &multi.Team{Name: "team", Journal: j, Members: []harness.Runner{
			plain("member-a", "You are a team member. Do your task from the board and add follow-up tasks when needed."),
			plain("member-b", "You are a team member. Do your task from the board and add follow-up tasks when needed."),
		}, MaxTasks: 4}, nil
	case "debate":
		return &multi.Debate{Name: "debate", Journal: j, Rounds: 2, Debaters: []harness.Runner{
			plain("debater-a", "You are debater a. Argue for the strongest answer you can."),
			plain("debater-b", "You are debater b. Argue for the strongest answer you can."),
		}, Judge: plain("judge", "You are the judge. Pick the best final answer.")}, nil
	}
	return nil, fmt.Errorf("unknown pattern %q", id)
}

const (
	assistantPrompt  = "You are the assistant in the agent-harness demo. Use tools when they help: clock, calculator, search_docs, send_email, delete_records. Be brief."
	billingPrompt    = "You are billing: you answer questions about invoices, refunds and payments. Transfer technical problems to tech."
	researcherPrompt = "You are the researcher. Answer the task in two factual sentences."
)
