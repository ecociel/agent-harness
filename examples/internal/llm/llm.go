// Package llm picks a real model provider from the environment for the examples.
package llm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/anthropic"
	"github.com/ecociel/agent-harness/provider/openai"
)

// FromEnv reads HARNESS_PROVIDER through getenv. It returns a nil provider for "",
// "fake" and "demo", so the caller supplies its own mock.
func FromEnv(getenv func(string) string) (p model.Provider, modelName, label string, err error) {
	switch strings.ToLower(getenv("HARNESS_PROVIDER")) {
	case "", "fake", "demo":
		return nil, "", "", nil
	case "openai":
		p := &openai.Provider{BaseURL: getenv("OPENAI_BASE_URL"), APIKey: getenv("OPENAI_API_KEY")}
		if p.BaseURL == "" {
			p.BaseURL = "https://api.openai.com/v1"
		}
		if p.APIKey == "" && !strings.Contains(p.BaseURL, "localhost") && !strings.Contains(p.BaseURL, "127.0.0.1") {
			return nil, "", "", errors.New("HARNESS_PROVIDER=openai needs OPENAI_API_KEY (or a local OPENAI_BASE_URL)")
		}
		m := getenv("OPENAI_MODEL")
		if m == "" {
			m = "gpt-4.1-mini"
		}
		return p, m, "openai-compatible · " + m, nil
	case "anthropic":
		p := &anthropic.Provider{BaseURL: getenv("ANTHROPIC_BASE_URL"), APIKey: getenv("ANTHROPIC_API_KEY")}
		if p.APIKey == "" {
			return nil, "", "", errors.New("HARNESS_PROVIDER=anthropic needs ANTHROPIC_API_KEY")
		}
		m := getenv("ANTHROPIC_MODEL")
		if m == "" {
			m = "claude-opus-5-5"
		}
		return p, m, "anthropic · " + m, nil
	}
	return nil, "", "", fmt.Errorf("unknown HARNESS_PROVIDER %q (want fake, openai or anthropic)", getenv("HARNESS_PROVIDER"))
}
