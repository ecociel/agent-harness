package multi_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/provider/fake"
)

// bot is a text-only agent whose fake model answers with reply(prompt).
func bot(name string, reply func(prompt string) string) *harness.Agent {
	return &harness.Agent{Name: name, Provider: fake.NewFunc(func(req model.Request) (model.Response, error) {
		return fake.Text(reply(model.LastUser(req.Messages))), nil
	})}
}

func say(text string) func(string) string { return func(string) string { return text } }

func ExampleSequential() {
	pipeline := &multi.Sequential{Name: "pipeline", Steps: []harness.Runner{
		bot("planner", say("plan: add a greeting")),
		bot("implementer", func(p string) string {
			if strings.Contains(p, "plan: add a greeting") {
				return "func hello() string { return \"hi\" }"
			}
			return "no plan"
		}),
		bot("verifier", func(p string) string {
			if strings.Contains(p, "func hello") {
				return "verified: hello() returns hi"
			}
			return "nothing to verify"
		}),
	}}
	res, _ := pipeline.Run(context.Background(), harness.Prompt("greet the user"))
	fmt.Println(res.Output)
	// Output: verified: hello() returns hi
}

func ExampleParallel() {
	review := &multi.Parallel{Name: "review", Branches: []harness.Runner{
		bot("security", say("no secrets found")),
		bot("style", say("naming is consistent")),
		bot("tests", say("coverage 82%")),
	}}
	res, _ := review.Run(context.Background(), harness.Prompt("review this diff"))
	fmt.Println(res.Output)
	// Output:
	// ## security
	// no secrets found
	//
	// ## style
	// naming is consistent
	//
	// ## tests
	// coverage 82%
}

func ExampleBestOfN() {
	best := &multi.BestOfN{Name: "best-of-3",
		Candidates: []harness.Runner{bot("a", say("2+2=5")), bot("b", say("2+2=4")), bot("c", say("four-ish"))},
		Judge:      bot("judge", say("WINNER: 2 — the only exact answer")),
	}
	res, _ := best.Run(context.Background(), harness.Prompt("what is 2+2?"))
	fmt.Println(res.Output)
	// Output: 2+2=4
}

func ExampleRouter() {
	router := &multi.Router{Name: "support",
		Classifier: bot("classifier", func(p string) string {
			if strings.Contains(p, "invoice") {
				return "billing"
			}
			return "unsure"
		}),
		Routes: map[string]harness.Runner{
			"billing":   bot("billing", say("Your invoice is attached.")),
			"technical": bot("technical", say("Try restarting.")),
		},
		Fallback: bot("general", say("A human will get back to you.")),
	}
	a, _ := router.Run(context.Background(), harness.Prompt("where is my invoice?"))
	b, _ := router.Run(context.Background(), harness.Prompt("tell me a joke"))
	fmt.Println(a.Output)
	fmt.Println(b.Output)
	// Output:
	// Your invoice is attached.
	// A human will get back to you.
}

func ExampleSubagent() {
	researcher := bot("researcher", say("Zurich has 4 public holidays in December."))
	// The lead's model: first delegate, then answer from the tool result.
	lead := &harness.Agent{Name: "lead"}
	lead.Provider = fake.NewFunc(func(req model.Request) (model.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleTool {
			return fake.Text("According to research: " + last.Content), nil
		}
		return fake.Call("research", map[string]string{"task": "How many public holidays does Zurich have in December?"}), nil
	})
	lead, _ = multi.WithSubagents(lead, multi.Subagent{Name: "research", Description: "Look facts up.", Agent: researcher})
	res, _ := lead.Run(context.Background(), harness.Prompt("plan December"))
	fmt.Println(res.Output)
	// Output: According to research: Zurich has 4 public holidays in December.
}

func ExampleBackground() {
	pool := multi.NewBackground(multi.Subagent{Name: "crawler", Description: "Crawl a site.",
		Agent: bot("crawler", say("12 pages crawled"))})
	step := 0
	lead := &harness.Agent{Name: "lead", Tools: nil, Provider: fake.NewFunc(func(req model.Request) (model.Response, error) {
		step++
		switch step {
		case 1:
			return fake.Call("start_crawler", map[string]string{"task": "crawl ecociel.ch"}), nil
		case 2:
			return fake.Call("collect", map[string]any{"id": "job-1", "wait": true}), nil
		}
		return fake.Text("crawler says: " + req.Messages[len(req.Messages)-1].Content), nil
	})}
	reg, _ := lead.Tools.With(pool.Tools()...)
	lead.Tools = reg
	res, _ := lead.Run(context.Background(), harness.Prompt("crawl in the background"))
	pool.Wait()
	fmt.Println(res.Output)
	// Output: crawler says: 12 pages crawled
}

func ExamplePlannerWorker() {
	round := 0
	planner := bot("planner", func(p string) string {
		round++
		if round == 1 {
			return "TASK: write the parser\nTASK: write the tests"
		}
		return "DONE"
	})
	worker := bot("worker", func(p string) string {
		switch {
		case strings.Contains(p, "write the parser"):
			return "parser written; handles CSV"
		case strings.Contains(p, "write the tests"):
			return "tests written; 12 pass"
		}
		return "?"
	})
	judge := bot("judge", say("CONTINUE: check once more"))
	pw := &multi.PlannerWorker{Name: "build", Planner: planner, Worker: worker, Judge: judge, MaxRounds: 3}
	res, _ := pw.Run(context.Background(), harness.Prompt("build a CSV importer"))
	fmt.Println(res.Output)
	// Output:
	// ### Handoff 1
	// parser written; handles CSV
	//
	// ### Handoff 2
	// tests written; 12 pass
}

func ExampleEvaluatorOptimizer() {
	gen := bot("writer", func(p string) string {
		if strings.Contains(p, "Reviewer feedback") {
			return "Grüezi! Welcome to écociel."
		}
		return "Hello."
	})
	critic := bot("critic", func(p string) string {
		if strings.Contains(p, "écociel") {
			return "APPROVED"
		}
		return "REVISE: mention écociel and greet in Swiss German"
	})
	eo := &multi.EvaluatorOptimizer{Name: "copy", Generator: gen, Evaluator: critic}
	res, _ := eo.Run(context.Background(), harness.Prompt("write a welcome line"))
	fmt.Println(res.Output)
	// Output: Grüezi! Welcome to écociel.
}

func ExampleHandoff() {
	triage := &harness.Agent{Name: "triage", Instructions: "Route the user.",
		Provider: fake.Script(fake.Call("transfer_to_billing", map[string]string{"reason": "invoice question"}))}
	billing := &harness.Agent{Name: "billing", Instructions: "Answer billing questions.",
		Provider: fake.Script(fake.Text("Invoice 2026-114 was paid on 3 October."))}
	h := &multi.Handoff{Name: "desk", Agents: []*harness.Agent{triage, billing}}
	res, _ := h.Run(context.Background(), harness.Prompt("was my invoice paid?"))
	fmt.Println(res.Agent+":", res.Output)
	// Output: billing: Invoice 2026-114 was paid on 3 October.
}

func ExampleTeam() {
	member := bot("member", func(p string) string {
		if strings.Contains(p, "Your task (#1)") {
			return "inventory done\nNEW TASK: reorder low stock"
		}
		return "reorder placed"
	})
	team := &multi.Team{Name: "ops", Members: []harness.Runner{member, member}, MaxTasks: 5}
	res, _ := team.Run(context.Background(), harness.Prompt("count the inventory"))
	for _, line := range strings.Split(res.Output, "\n") {
		if strings.HasPrefix(line, "## Task") {
			fmt.Println(line[:strings.Index(line, "(")] + strings.SplitN(line, ": ", 2)[1])
		}
	}
	// Output:
	// ## Task 1 count the inventory
	// ## Task 2 reorder low stock
}

func ExampleDebate() {
	d := &multi.Debate{Name: "debate",
		Debaters: []harness.Runner{
			bot("optimist", func(p string) string {
				if strings.Contains(p, "Other agents answered") {
					return "17 — I was wrong, 9+8 is 17"
				}
				return "18"
			}),
			bot("skeptic", say("17")),
		},
		Judge: bot("judge", say("WINNER: 1 — corrected and explained")),
	}
	res, _ := d.Run(context.Background(), harness.Prompt("what is 9+8?"))
	fmt.Println(res.Output)
	// Output: 17 — I was wrong, 9+8 is 17
}
