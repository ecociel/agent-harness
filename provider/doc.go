// Package provider groups the model.Provider implementations:
//
//   - fake: deterministic scripted or computed replies (tests, demo)
//   - openai: any OpenAI-compatible /chat/completions endpoint (OpenAI,
//     xAI, Groq, Together, Ollama, vLLM, LM Studio, OpenRouter, …)
//   - anthropic: the Anthropic Messages API
//
// The real providers stream (server-sent events), map tool calls both ways
// and report token usage; they use only net/http.
package provider
