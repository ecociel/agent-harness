package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ecociel/agent-harness/model"
)

// brain is the zero-key demo model. It is a deterministic function of the
// request: it reads the role from the system instructions ("You are the
// planner …") and the conversation, and answers the way a real model in that
// role would, in the formats the multi-agent patterns parse (WINNER:, TASK:,
// APPROVED, REVISE:, transfer_to_*). It exists so every state, hook and
// pattern can be exercised without an API key.
func brain(req model.Request) (model.Response, error) {
	sys := ""
	if len(req.Messages) > 0 && req.Messages[0].Role == model.RoleSystem {
		sys = strings.ToLower(req.Messages[0].Content)
	}
	msgs := req.Messages
	last := msgs[len(msgs)-1]
	user := model.LastUser(msgs)
	lu := strings.ToLower(user)

	switch {
	case strings.Contains(lu, "'winner: <"):
		return model.Response{Text: judgeWinner(user)}, nil
	case strings.Contains(lu, "answer done or continue"):
		return model.Response{Text: "DONE — every task has a handoff report and no concerns are open."}, nil
	case strings.Contains(lu, "reply 'approved'"):
		if strings.Contains(lu, "(revised)") {
			return model.Response{Text: "APPROVED"}, nil
		}
		return model.Response{Text: "REVISE: add one concrete example and say which hook point it uses."}, nil
	case strings.Contains(lu, "classify the request into exactly one of"):
		return model.Response{Text: classify(lu)}, nil
	case strings.Contains(lu, "you are the planner. do no work yourself"):
		if strings.Contains(lu, "handoff reports from workers so far") {
			return model.Response{Text: "DONE"}, nil
		}
		g := goal(user, "Goal:")
		return model.Response{Text: "TASK: Research what is known about: " + g + "\nTASK: Draft a short answer to: " + g}, nil
	case strings.Contains(lu, "worker handoff reports"):
		return model.Response{Text: "Summary of the workers' reports: " + firstSentences(after(user, "Worker handoff reports:"), 2)}, nil
	case strings.Contains(lu, "end with a handoff report"):
		t := goal(user, "Your task:")
		return model.Response{Text: "Did: " + t + "\nFindings: the harness exposes this through explicit states and hook points.\nHandoff: no concerns."}, nil
	case strings.Contains(lu, "team goal:"):
		t := goal(user, "):")
		out := "Finished " + strings.TrimSpace(t) + "."
		if strings.Contains(lu, "your task (#1)") {
			out += "\nNEW TASK: Double-check the result of task 1"
		}
		return model.Response{Text: out}, nil
	case strings.Contains(lu, "other agents answered"):
		return model.Response{Text: "Having read the others, I keep my core point and add theirs: " + firstSentences(after(user, "Other agents answered:"), 1)}, nil
	}

	switch {
	case strings.Contains(sys, "you are the lead"):
		return lead(msgs, sys, user)
	case strings.Contains(sys, "you are triage"):
		if last.Role == model.RoleUser {
			switch {
			case regexp.MustCompile(`invoice|bill|refund|price|pay`).MatchString(lu):
				return call("transfer_to_billing", map[string]string{"note": "billing question"}), nil
			case regexp.MustCompile(`bug|error|crash|broken|fails`).MatchString(lu):
				return call("transfer_to_tech", map[string]string{"note": "technical problem"}), nil
			}
		}
		return model.Response{Text: "Triage here. Ask me about an invoice (billing) or a bug (tech) to see a handoff."}, nil
	case strings.Contains(sys, "you are billing"):
		if regexp.MustCompile(`bug|error|crash`).MatchString(lu) && last.Role == model.RoleUser && !strings.Contains(lu, "handed this conversation") {
			return call("transfer_to_tech", map[string]string{"note": "this is a bug, not billing"}), nil
		}
		return model.Response{Text: "Billing here: your last invoice was CHF 49.00, paid on the 1st. Anything else about billing?"}, nil
	case strings.Contains(sys, "you are tech support"):
		return model.Response{Text: "Tech support here: clear the cache and retry; if it still fails, send me the error id."}, nil
	case strings.Contains(sys, "you are the pipeline planner"):
		return model.Response{Text: "Plan: 1) restate the question, 2) answer it in two sentences, 3) list one caveat."}, nil
	case strings.Contains(sys, "you are the writer"):
		if strings.Contains(lu, "reviewer feedback") {
			return model.Response{Text: draftFor(msgs) + " (revised) Example: a guardrail at BeforeTool denied delete_records."}, nil
		}
		return model.Response{Text: draftFor(msgs)}, nil
	case strings.Contains(sys, "you are the reviewer"):
		return model.Response{Text: "Reviewed. " + firstSentences(after(user, "):\n"), 2) + " Caveat: this is the scripted demo model."}, nil
	case strings.Contains(sys, "specialist"):
		role := between(sys, "you are the ", " specialist")
		return model.Response{Text: fmt.Sprintf("From the %s angle: %s looks fine; one thing to watch is %s.", role, short(user), concern(role))}, nil
	case strings.Contains(sys, "you are candidate"):
		style := between(sys, "you are candidate ", ".")
		return model.Response{Text: candidate(style, user)}, nil
	case strings.Contains(sys, "you are debater"):
		who := between(sys, "you are debater ", ".")
		return model.Response{Text: fmt.Sprintf("Debater %s: %s — %s.", who, short(user), concern(who))}, nil
	case strings.Contains(sys, "you are the researcher"):
		return model.Response{Text: "Research notes on " + short(user) + ": the harness models this as states plus hooks; see docs/patterns."}, nil
	case strings.Contains(sys, "you are smalltalk"):
		return model.Response{Text: "Hi! I'm the small-talk route. Try asking for the time, a calculation, or an invoice."}, nil
	}
	return assistant(msgs, user)
}

// assistant is the tool-using single agent.
func assistant(msgs []model.Message, user string) (model.Response, error) {
	last := msgs[len(msgs)-1]
	lu := strings.ToLower(user)
	if last.Role == model.RoleTool {
		if strings.Contains(lu, "loop") {
			return call("clock", map[string]string{}), nil
		}
		return model.Response{Text: answerFromTool(last)}, nil
	}
	switch {
	case strings.Contains(lu, "loop"):
		return call("clock", map[string]string{}), nil
	case regexp.MustCompile(`\btime\b|\bclock\b|what day|date`).MatchString(lu):
		return call("clock", map[string]string{}), nil
	case regexp.MustCompile(`[0-9][0-9\s().]*[-+*/][\s(]*[0-9]`).MatchString(lu):
		expr := regexp.MustCompile(`[0-9().+\-*/\s]+`).FindAllString(user, -1)
		best := ""
		for _, e := range expr {
			if len(strings.TrimSpace(e)) > len(best) {
				best = strings.TrimSpace(e)
			}
		}
		return call("calculator", map[string]string{"expression": best}), nil
	case strings.Contains(lu, "search") || strings.Contains(lu, "docs"):
		return call("search_docs", map[string]string{"query": user}), nil
	case strings.Contains(lu, "email") || strings.Contains(lu, "mail"):
		return call("send_email", map[string]string{"to": "jan@example.com", "subject": "Hello from the harness demo", "body": user}), nil
	case strings.Contains(lu, "delete"):
		return call("delete_records", map[string]string{"filter": "all"}), nil
	case strings.Contains(lu, "ahv") || strings.Contains(lu, "iban") || strings.Contains(lu, "pii"):
		return model.Response{Text: "Your AHV number on file is 756.1234.5678.97 and your IBAN is CH93 0076 2011 6238 5295 7."}, nil
	case strings.Contains(lu, "long"):
		return model.Response{Text: strings.Repeat("The harness is a state machine with hook points. ", 40)}, nil
	}
	return model.Response{Text: "I'm the scripted demo model (no API key needed). Try: what time is it · compute (17+4)*3 · search the docs for budget · send an email to Jan · delete all records · show my AHV number · loop · or switch the pattern above. You said: " + short(user)}, nil
}

func answerFromTool(m model.Message) string {
	c := m.Content
	switch {
	case strings.Contains(c, `"denied":true`):
		var d struct{ Reason string }
		_ = json.Unmarshal([]byte(c), &d)
		return "I was not allowed to run " + m.Name + ": " + d.Reason
	case strings.Contains(c, `"offloaded"`):
		var d struct {
			Offloaded string `json:"offloaded"`
			Bytes     int    `json:"bytes"`
			Preview   string `json:"preview"`
		}
		_ = json.Unmarshal([]byte(c), &d)
		return fmt.Sprintf("The search returned %d bytes, so the offload hook kept it out of my context (%s). From the preview: %s", d.Bytes, d.Offloaded, firstSentences(d.Preview, 2))
	case strings.HasPrefix(c, "error:"):
		return "The tool failed: " + strings.TrimPrefix(c, "error: ")
	case m.Name == "clock":
		return "It is " + c + "."
	case m.Name == "calculator":
		var d struct {
			Expression string
			Value      float64
		}
		_ = json.Unmarshal([]byte(c), &d)
		return fmt.Sprintf("%s = %g", d.Expression, d.Value)
	case m.Name == "send_email":
		return "Done — the (pretend) e-mail was sent. Nothing actually left the machine."
	}
	return m.Name + " says: " + short(c)
}

// lead drives the orchestrator (foreground sub-agents) and background demos.
func lead(msgs []model.Message, sys, user string) (model.Response, error) {
	last := msgs[len(msgs)-1]
	background := strings.Contains(sys, "background")
	if last.Role == model.RoleUser {
		if background {
			return call("start_researcher", map[string]string{"task": "Research: " + user}), nil
		}
		return call("researcher", map[string]string{"task": "Research this and report in two sentences: " + user}), nil
	}
	if background && last.Role == model.RoleTool && strings.Contains(last.Content, `"status":"running"`) {
		var d struct{ Job string }
		_ = json.Unmarshal([]byte(last.Content), &d)
		return call("collect", map[string]any{"id": d.Job, "wait": true}), nil
	}
	return model.Response{Text: "The researcher sub-agent reported back: " + short(strings.TrimSpace(last.Content))}, nil
}

func call(name string, args any) model.Response {
	b, _ := json.Marshal(args)
	return model.Response{ToolCalls: []model.ToolCall{{ID: "call_" + name, Name: name, Arguments: b}}}
}

func classify(lu string) string {
	req := after(lu, "request:")
	switch {
	case regexp.MustCompile(`invoice|bill|refund|price|pay`).MatchString(req):
		return "billing"
	case regexp.MustCompile(`time|clock|date|[0-9]|search|docs|mail|delete|ahv|loop`).MatchString(req):
		return "assistant"
	}
	return "smalltalk"
}

func judgeWinner(prompt string) string {
	// Pick the longest candidate: a crude but deterministic "most complete".
	best, n, i := 0, 0, 0
	for _, part := range regexp.MustCompile(`(?m)^(Candidate|Answer) \d+:`).Split(prompt, -1)[1:] {
		i++
		if l := len(part); l > n {
			best, n = i, l
		}
	}
	if best == 0 {
		best = 1
	}
	return fmt.Sprintf("WINNER: %d — the most complete answer.", best)
}

func candidate(style, user string) string {
	switch style {
	case "a":
		return "Short answer: " + short(user) + "."
	case "b":
		return "Thorough answer: " + short(user) + ". It covers the inner loop, the outer loop and the hook points, with one example per point."
	}
	return "Playful answer: " + short(user) + " — think of hooks as plug sockets on a state machine."
}

func concern(role string) string {
	switch role {
	case "security":
		return "tool calls that need approval"
	case "cost":
		return "token budgets per run"
	case "a":
		return "explicit states make failures debuggable"
	case "b":
		return "hooks keep cross-cutting concerns out of the loop"
	}
	return "keeping the answer verifiable"
}

func draftFor(msgs []model.Message) string {
	u := firstUser(msgs)
	if strings.Contains(u, "Original request:") {
		u = goal(u, "Original request:")
	}
	return "Draft: " + short(u) + " — the harness answers this with explicit states and hooks."
}

func firstUser(msgs []model.Message) string {
	for _, m := range msgs {
		if m.Role == model.RoleUser {
			return m.Content
		}
	}
	return ""
}

func goal(s, marker string) string {
	return strings.TrimSpace(firstLine(after(s, marker)))
}

func after(s, marker string) string {
	if i := strings.Index(strings.ToLower(s), strings.ToLower(marker)); i >= 0 {
		return strings.TrimSpace(s[i+len(marker):])
	}
	return s
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstSentences(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	out := ""
	for range n {
		i := strings.IndexAny(s, ".!?")
		if i < 0 {
			return strings.TrimSpace(out + s)
		}
		out += s[:i+1] + " "
		s = s[i+1:]
	}
	return strings.TrimSpace(out)
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 140 {
		return string(r[:140]) + "…"
	}
	return s
}
