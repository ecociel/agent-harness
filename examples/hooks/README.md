# Hook examples

One small program per hook. Each prints what happens as it runs, so you learn a hook by running its example and reading its `main.go`. Next to each `main.go`, `output.txt` holds what the mock run prints.

## Run one

Start with the tour. It sends one run through all 12 plugin points.

```sh
go run ./examples/hooks/tour
```

Every example runs on a mock model by default, so it needs no key and prints the same text every time. Set `HARNESS_PROVIDER` to run the same program on a real model:

```sh
HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/tour
HARNESS_PROVIDER=openai OPENAI_API_KEY=… go run ./examples/hooks/tour
HARNESS_PROVIDER=openai OPENAI_BASE_URL=http://localhost:11434/v1 OPENAI_MODEL=llama3.1 go run ./examples/hooks/tour
```

A real model can answer differently from the mock. It may not call the tool the mock calls, so some hooks may not fire, and the example still exits 0. Examples that test a failure inject it with `demo.FailFirst`, so those failures happen on a real model too.

| Variable | Meaning |
|---|---|
| `HARNESS_PROVIDER` | `fake` (default), `openai` or `anthropic` |
| `OPENAI_API_KEY` | key for OpenAI or an OpenAI-compatible endpoint, not needed on localhost |
| `OPENAI_BASE_URL` | default `https://api.openai.com/v1` |
| `OPENAI_MODEL` | default `gpt-4.1-mini` |
| `ANTHROPIC_API_KEY` | key for the Anthropic Messages API |
| `ANTHROPIC_BASE_URL` | default is the public API |
| `ANTHROPIC_MODEL` | default `claude-opus-5-5` |

## Read the output

- `> text` is the prompt. `< text` is the answer.
- An indented line comes from the timeline, an event sink in `internal/demo`. It reports what the run recorded.
- `[name] text` is printed by the example's own code, so the hook says why it did something.
- `stop:` and `path:` come from the returned `Run`. They say how it ended and which states it visited.

## The examples

Read them in this order. The points follow the order a run meets them, and the built-in packages come after the hand-written hooks they generalise.

### Plugin points

Each is a hand-written hook that implements one interface.

| Example | What it teaches |
|---|---|
| [tour](tour/main.go) | all 12 points firing in one run. Start here |
| [beforerun](points/beforerun/main.go) | admission: let one run in and `Stop` another before it spends anything |
| [context](points/context/main.go) | a recorded `Edit` that loads a memory once, with `Source` set, and skips later turns |
| [beforemodel](points/beforemodel/main.go) | retrieval for one request, which never appears in `res.Messages` |
| [wrapmodel](points/wrapmodel/main.go) | wrap the stream, see each chunk, and `Stop` mid-reply |
| [aftermodel](points/aftermodel/main.go) | revise a reply before it is recorded |
| [recover](points/recover/main.go) | a hand-written retry decision after injected failures |
| [beforetool](points/beforetool/main.go) | `Allow`, `Deny` with the model adapting, and `Halt` |
| [wraptool](points/wraptool/main.go) | wrap one execution and read the idempotency key `c.Key` |
| [aftertool](points/aftertool/main.go) | mask a field in a result before it is recorded |
| [checkoutput](points/checkoutput/main.go) | `Revise` once, then `Accept` |
| [afterrun](points/afterrun/main.go) | save a summary keyed on `r.ID` |
| [redact](points/redact/main.go) | a value never reaches the model, the tools or the journal, with the journal read back as proof |

### Built-in packages

Each uses the real package from `hooks/`.

| Example | What it teaches |
|---|---|
| [guard](builtin/guard/main.go) | refuse an input, deny a tool, revise an output |
| [budget](builtin/budget/main.go) | a reached limit stops the run, and `Spent` comes from the `Run` |
| [approval](builtin/approval/main.go) | an `Approver` that decides on the spot, `Ask` with suspend and resume, and `Audit` |
| [compact](builtin/compact/main.go) | old messages replaced by one recorded summary |
| [offload](builtin/offload/main.go) | a large result replaced by a handle, which the model reads with `offload.Reader` |
| [retry](builtin/retry/main.go) | recorded retries, then one try on a fallback model |
| [profile](builtin/profile/main.go) | per-model instructions and a hidden tool |
| [trajectory](builtin/trajectory/main.go) | a finished run read back as steps |
| [trace](builtin/trace/main.go) | a sink, not a hook: spans with stable IDs |
| [metrics](builtin/metrics/main.go) | a sink, not a hook: counters that include failed calls |
| [logging](builtin/logging/main.go) | a sink, not a hook: one `log/slog` line per event |

## When you change one

`go test ./examples/hooks` runs every example against its `output.txt`, and runs every example on a stub provider to prove none depends on the mock's script. A new plugin point or a new package under `hooks/` fails the test until it has an example here. After a change on purpose, regenerate the files:

```sh
UPDATE_DOCS=1 go test ./examples/hooks
```
