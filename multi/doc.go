// Package multi implements multi-agent and sub-agent architectures on top of
// the harness. Every pattern is a harness.Runner, and takes Runners as its
// parts, so patterns compose freely: a Router can route to a Parallel whose
// branches are Pipelines of Agents.
//
// The set, and where each comes from:
//
//	Pattern             Source
//	------------------  ---------------------------------------------------------
//	Sequential          Cursor docs/subagents "orchestrator pattern" (planner →
//	                    implementer → verifier); ecociel/agents sequential
//	Parallel            Cursor parallel agents and parallel subagents;
//	                    ecociel/agents parallel (code join, not a model join)
//	BestOfN (judge)     Cursor "run multiple models at once … Cursor suggests
//	                    which solution is best"; harness talk: agent-as-judge
//	Subagent /          Cursor subagents (own context window, foreground or
//	Background          background); Dani Zhu "hierarchical delegation";
//	                    ecociel/agents orchestrator (delegate is a tool, depth cap)
//	PlannerWorker       Cursor "Scaling long-running autonomous coding" and
//	                    "Towards self-driving codebases": planners, recursive
//	                    sub-planners, isolated workers, handoff reports, a judge
//	                    deciding whether to continue, fresh start per cycle
//	EvaluatorOptimizer  ecociel/agents critic and verified; tessmir critic
//	Router              ecociel/agents routing; tessmir routing
//	Handoff             ecociel/agents handoff (control moves, cycle cap)
//	Team (Blackboard)   Cursor's first coordination attempt (a shared list);
//	                    Dani Zhu "shared-state teams"; ecociel/agents blackboard
//	Debate              multi-agent debate: peers see each other's answers for
//	                    a few rounds, then a judge decides
//
// Every pattern is a composite run with its own journal. Its body keeps no state: a
// child's ID comes from its position, and each decision comes from what a child
// recorded, so a resume walks the same tree and finished children answer from their
// journals. Functions a pattern takes, like Score or Seed, must be pure for the same
// reason.
//
// Patterns report their own steps as harness.KindPattern events, so the diagnostic
// view shows fan-outs, verdicts and handoffs alongside the inner state machines.
package multi
