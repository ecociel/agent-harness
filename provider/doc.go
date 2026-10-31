// Package provider groups the model.Provider implementations:
//
//   - fake: deterministic scripted or computed replies (tests, demo)
//   - openai: any OpenAI-compatible /chat/completions endpoint (OpenAI,
//     xAI, Groq, Together, Ollama, vLLM, LM Studio, OpenRouter, …)
//   - anthropic: the Anthropic Messages API
//
// The real providers stream (server-sent events), map tool calls both ways
// and report token usage; they use only net/http. Each classifies its failures as a
// model.Error, carrying Retry-After and whatever the provider billed, and treats a
// stream that ends with no end marker as a failure, not as a short reply.
package provider
