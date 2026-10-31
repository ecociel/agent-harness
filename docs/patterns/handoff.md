# Handoff

```
triage ──transfer_to_billing──▶ billing ──transfer_to_tech──▶ tech → answer
```

Peer agents transfer the conversation to each other. Unlike a sub-agent call, **control moves and does not come back**: the new owner sees the whole transcript and answers the user itself.

```go
h := &multi.Handoff{Agents: []*harness.Agent{triage, billing, tech}, Sticky: true, MaxHops: 4}
```

- **Tools.** Each agent automatically gets one `transfer_to_<peer>` tool per peer, described by the peer's first instruction line. Calling it returns a `*tool.Handoff`. The current run ends with `StopHandoff`, and the pattern starts the target with the transcript plus a note.
- **Hop cap.** Hops are counted outside the agents. A ping-pong is stopped with `loop_detected` after `MaxHops`.
- **Sticky.** For chat sessions, the last owner keeps the conversation on the next `Run`. The owner's reply carries its name as `Message.Source`, so this survives a restart with the session's history, and `Owner(messages)` reports who that is.
- **Durable.** A child's run ID names its hop and its agent, and each transfer is read from the recorded run. A transfer has no effect of its own, so a crash inside one asks nobody. It drops provider state, because a signature covers the instructions and tools the next owner doesn't share.
- **Policy.** Transfers are tools, so guard and approval hooks can restrict who may hand off to whom.
- **Events.** `owner` (who holds the conversation), `transfer` (from → to, with the note), `end`.

**Sources:** ecociel/agents `handoff` and `escalated`; Dani Zhu's *programmed workflows*; the handoff pattern popularised by OpenAI's Swarm/Agents SDK.

**Example:** `ExampleHandoff`.
