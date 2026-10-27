# Source: "When to Build Your Own Agent Harness" (Harrison Chase, LangChain)

Sequoia Capital, https://youtu.be/HI2q3ci3Iuc. About 15 minutes of talk and 15 minutes of Q&A. The summary below was made from the public transcript (youtubetotranscript.com). Quotes are verbatim; the rest is paraphrase. It was the **primary source** for this library's loop and plugin points.

## Summary

**An agent is three parts: a harness that orchestrates a model and some context.** To own your intelligence you want to own all three:

- **The model.** Being able to switch avoids lock-in and lets you use the best model as soon as it is available.
- **The context.** Memory, semantic knowledge and previous conversations.
- **The harness**, the focus of the talk. Its main job is *"to bring context to the model at the right point in time"*: assembling fixed and dynamic context into the window, showing it to the model, and acting on the response.

**The core loop is the same everywhere.** *"An LLM running in a loop calling tools"*: a request comes in, the LLM generates, the generation may include tool calls, the tools run, and the observation goes back to the LLM. Harnesses differ in what they add *at different stages* of that loop. LangChain's minimal harness and Deep Agents (a model-agnostic, general-purpose take on Claude Code with file systems, skills and sub-agents) are the same loop, customised with **middleware**:

- run code **before the agent** is invoked
- run code **before each model call**
- **wrap the model call**
- **wrap the tool calls**

Coding agents call the same idea hooks or plugins. Through middleware you can add a sandbox, a file system, sub-agents, memory and summarisation:

- **Summarisation** runs *before the model*: check whether the context is too long, and summarise it if so.
- **Context offloading** *wraps the tool call*: large tool results are dumped out of the context.

**Cognitive architectures versus a general loop.** In 2023–24 models weren't good enough to run in a loop, so people built bespoke flows, like a deep-research graph that fans out sub-questions, or a code-review bot. Much of that has moved into the harness as modifications of the core loop. Explicit architectures are still used for particular flows and for **predictability and control** (financial-services customers find a free-running agent "too scary"). Advice: *start with a general harness*, then add **gates and checks** as you narrow in on the use case. It's a spectrum: off-the-shelf harness → hooks/middleware → a full custom cognitive architecture.

**Build vs buy.** Off-the-shelf harnesses (Claude Code / Agent SDK, Codex) are tied to their labs' models. The more *in distribution* your task is, the better they work; the further out of distribution, the more you'll tune your own harness. Even then, keep the in-distribution parts close to the model. Labs RL their models on specific tools: OpenAI and Anthropic models edit files differently. Deep Agents therefore has **model profiles** that switch the edit-file implementation depending on the model.

**Evals.** Every company building a mission-critical agent builds benchmarks for it, to catch regressions and to hill-climb by changing the harness or the model. **Harbor** (from the makers of Terminal Bench 2) is becoming the standard runner. A task bundles four parts:

- an **environment** (a Dockerfile/sandbox)
- a golden **solution** (a sanity check)
- a **test** verifier: code, unit tests, an LLM as judge or an agent as judge
- an `instruction.md` prompt

Track **accuracy, latency and tokens/cost**.

**Observability** is underrated. Agents fail when an LLM call goes wrong, either because the model isn't good enough or, more often, because *the context it received isn't good enough*. You need to see what went into the context window, how it accumulated, and which steps and tools ran. Agents are increasingly represented as **trajectories** (the message list), but you also need the **full trace**.

**The data flywheel.** Build an agent, run it, collect traces, curate them with feedback, run experiments, and improve any of the three parts:

- the harness, through harness engineering
- the model, through fine-tuning
- the context, through memory

Feedback can come from UX design (implicit signals) or from **online evaluators**: cheap LLM judges, fine-tuned SLMs, or plain code over traces. LangSmith Engine automates the loop: an agent over traces that files issues and suggests prompt and harness fixes.

**Q&A.**

- Engine is benchmarked on its own Harbor-style "issue bench". Running Codex on it showed Codex aggressively writing small scripts against the traces. That lesson was brought back into Engine's harness: run many harnesses on one benchmark and keep what works.
- Will harnesses converge? "I don't know." General-purpose harnesses are now good enough to start with. Customise as you move out of distribution. Small lab-specific differences, like edit-file styles, persist, which is why evals and observability matter.

## What this library took from it

| Talk | agent-harness |
|---|---|
| agent = harness + model + context | `Agent{Provider, Tools, Hooks, Instructions}`; providers are swappable (`model.Provider`; openai, anthropic, fake) |
| the core loop: LLM → tools → observation → LLM | the inner loop `compose → model → route → tools → observe → compose` |
| middleware: before agent, before model, wrap model, wrap tool | `BeforeRun`, `BeforeModel`, `WrapModel`, `WrapTool`, plus `AfterRun` and the finer points the talk implies (`AfterModel`, `BeforeTool`, `AfterTool`, `AfterTurn`, `OnDelta`, `OnTransition`) |
| summarisation before the model | `hooks/compact` at `BeforeModel` |
| context offloading wraps the tool call | `hooks/offload` at `WrapTool` + `read_offloaded` tool |
| general loop + gates and checks; explicit architectures for control | the loop is general; gates are hooks (`BeforeTool` decisions, `CheckOutput` verdicts with the repair edge); the explicit transition table and the `multi` patterns give predictability |
| model profiles (per-model tool variants) | `hooks/profile` (instructions, hidden tools, output caps per model prefix) |
| sub-agents | `multi.Subagent`, `multi.Background`, and the other patterns |
| evals: tasks, verifier, LLM/agent as judge; accuracy, latency, tokens | package `eval` (`Task`, `Check`, `Judge`, `Report`); `OutputHook` verifiers |
| observability: context, steps, tools; trajectories and traces | the event stream (`WithEvents`), `hooks/trace` (spans), `hooks/trajectory` (message trajectories), `hooks/metrics`, `hooks/budget` (tokens/cost) |
| online evaluators over traces; the data flywheel | trajectories are JSONL; `eval.Judge` can run over them; out of scope beyond that |

### Where the design changed because of the video

- The plugin points were renamed and regrouped around the video's four middleware moments (*before agent*, *before model*, *wrap model*, *wrap tool*). Wrapping is real middleware (`next` chains), not before/after pairs, so retry, tracing and offloading compose naturally.
- Summarisation is a `BeforeModel` hook, as in the talk, not a step of its own (corporate-admin compacts inside `CallModel`). The compose state stays generic.
- Model profiles became a built-in hook.
- Trajectories became a hook of their own, separate from traces, because the talk treats them as different views.
