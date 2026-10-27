// Package model defines the provider-neutral vocabulary the harness speaks to
// a language model: messages, tool calls, streaming events and token usage.
//
// The shape deliberately mirrors corporate-admin's gateways/llm package
// (Provider.Complete returning an iter.Seq2 of streaming events, with
// TextDelta, ToolCallDelta and Finish), so an adapter between the two is a
// thin mapping rather than a redesign.
//
// A Provider only has to stream. Collect turns a stream into a Response and is
// what the harness uses internally; it is exported so providers and tests can
// reuse the exact same accumulation rules.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sort"
	"strings"
)

// Role says who authored a Message.
type Role string

// The four roles every chat-style provider understands.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry of the conversation sent to the model.
//
// For RoleAssistant, ToolCalls holds the calls the model asked for. For
// RoleTool, ToolCallID names the call this message answers and Name the tool.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// System returns a system message.
func System(text string) Message { return Message{Role: RoleSystem, Content: text} }

// User returns a user message.
func User(text string) Message { return Message{Role: RoleUser, Content: text} }

// Assistant returns an assistant message without tool calls.
func Assistant(text string) Message { return Message{Role: RoleAssistant, Content: text} }

// ToolResult returns the message that answers the tool call with id callID.
func ToolResult(callID, name, content string) Message {
	return Message{Role: RoleTool, ToolCallID: callID, Name: name, Content: content}
}

// ToolCall is a model's request to invoke one tool. Arguments is the raw JSON
// object the model produced; it is not validated here.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolSpec advertises one tool to the model. Parameters is a JSON Schema
// object describing the arguments.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Request is everything a provider needs for one model call.
type Request struct {
	Model       string     `json:"model"`
	Messages    []Message  `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
}

// Clone returns a deep-enough copy of r: slices are copied so a hook can edit
// the copy without mutating the caller's history.
func (r Request) Clone() Request {
	out := r
	out.Messages = append([]Message(nil), r.Messages...)
	out.Tools = append([]ToolSpec(nil), r.Tools...)
	return out
}

// Usage counts tokens for one or more model calls.
type Usage struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Cached    int `json:"cached,omitempty"`
	Reasoning int `json:"reasoning,omitempty"`
}

// Total is input plus output tokens. Cached and reasoning tokens are subsets
// reported for pricing and are not added again.
func (u Usage) Total() int { return u.Input + u.Output }

// Add returns the field-wise sum of u and o.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input:     u.Input + o.Input,
		Output:    u.Output + o.Output,
		Cached:    u.Cached + o.Cached,
		Reasoning: u.Reasoning + o.Reasoning,
	}
}

// FinishReason says why the model stopped generating.
type FinishReason string

// Finish reasons, aligned with the OpenAI vocabulary.
const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
)

// Event is one item of a streamed model reply: TextDelta, ToolCallDelta or
// Finish.
type Event interface{ isEvent() }

// TextDelta is a chunk of assistant text.
type TextDelta struct{ Text string }

// ToolCallDelta is a chunk of one tool call. Index identifies the call within
// the reply; ID and Name usually arrive on the first chunk and Arguments is
// appended across chunks.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Finish ends the stream with the stop reason and token usage.
type Finish struct {
	Reason FinishReason
	Usage  Usage
}

func (TextDelta) isEvent()     {}
func (ToolCallDelta) isEvent() {}
func (Finish) isEvent()        {}

// Provider is a language-model backend. Complete streams one reply. The
// sequence ends after a Finish event or at the first non-nil error.
type Provider interface {
	Complete(ctx context.Context, req Request) iter.Seq2[Event, error]
}

// ProviderFunc adapts a plain function to Provider.
type ProviderFunc func(ctx context.Context, req Request) iter.Seq2[Event, error]

// Complete calls f.
func (f ProviderFunc) Complete(ctx context.Context, req Request) iter.Seq2[Event, error] {
	return f(ctx, req)
}

// Response is a fully accumulated model reply.
type Response struct {
	Text      string       `json:"text"`
	ToolCalls []ToolCall   `json:"tool_calls,omitempty"`
	Finish    FinishReason `json:"finish"`
	Usage     Usage        `json:"usage"`
}

// Message returns the assistant message that records r in the history.
func (r Response) Message() Message {
	return Message{Role: RoleAssistant, Content: r.Text, ToolCalls: r.ToolCalls}
}

// Collect drains a stream into a Response. onEvent, if non-nil, sees every
// event as it arrives (the harness uses it to forward live deltas).
//
// Tool-call deltas are merged by Index. A call without an ID gets a stable
// synthetic one ("call_<index>") so tool results can always reference it.
func Collect(seq iter.Seq2[Event, error], onEvent func(Event)) (Response, error) {
	var (
		resp  Response
		text  strings.Builder
		calls = map[int]*ToolCall{}
		args  = map[int]*strings.Builder{}
	)
	for ev, err := range seq {
		if err != nil {
			return Response{}, err
		}
		if onEvent != nil {
			onEvent(ev)
		}
		switch e := ev.(type) {
		case TextDelta:
			text.WriteString(e.Text)
		case ToolCallDelta:
			c, ok := calls[e.Index]
			if !ok {
				c = &ToolCall{}
				calls[e.Index] = c
				args[e.Index] = &strings.Builder{}
			}
			if e.ID != "" {
				c.ID = e.ID
			}
			if e.Name != "" {
				c.Name = e.Name
			}
			args[e.Index].WriteString(e.Arguments)
		case Finish:
			resp.Finish = e.Reason
			resp.Usage = e.Usage
		default:
			return Response{}, fmt.Errorf("model: unknown event %T", ev)
		}
	}
	resp.Text = text.String()
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		c := calls[i]
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d", i)
		}
		raw := strings.TrimSpace(args[i].String())
		if raw == "" {
			raw = "{}"
		}
		c.Arguments = json.RawMessage(raw)
		resp.ToolCalls = append(resp.ToolCalls, *c)
	}
	if resp.Finish == "" {
		resp.Finish = FinishStop
		if len(resp.ToolCalls) > 0 {
			resp.Finish = FinishToolCalls
		}
	}
	return resp, nil
}

// Stream turns an already complete Response into a stream. Fake providers and
// tests use it; text is emitted in word-sized chunks so streaming UIs have
// something to show.
func Stream(r Response) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for _, chunk := range splitWords(r.Text) {
			if !yield(TextDelta{Text: chunk}, nil) {
				return
			}
		}
		for i, c := range r.ToolCalls {
			if !yield(ToolCallDelta{Index: i, ID: c.ID, Name: c.Name, Arguments: string(c.Arguments)}, nil) {
				return
			}
		}
		reason := r.Finish
		if reason == "" {
			reason = FinishStop
			if len(r.ToolCalls) > 0 {
				reason = FinishToolCalls
			}
		}
		yield(Finish{Reason: reason, Usage: r.Usage}, nil)
	}
}

// Fail returns a stream that yields err immediately.
func Fail(err error) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) { yield(nil, err) }
}

func splitWords(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if r == ' ' || r == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// EstimateTokens is a cheap, provider-independent token estimate (about four
// bytes per token). Budget pre-checks and compaction use it before a real
// count exists; actual spend always comes from Finish.Usage.
func EstimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += 4 + len(m.Content)/4
		for _, c := range m.ToolCalls {
			n += (len(c.Name) + len(c.Arguments)) / 4
		}
	}
	return n
}

// LastUser returns the content of the last user message, or "".
func LastUser(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}
