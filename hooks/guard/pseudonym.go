package guard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"sync"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Vault keeps the values behind tokens. Put is idempotent: the same token always
// names the same value. It may be called concurrently. A durable run needs a durable
// vault, because a resumed run holds tokens whose values only the vault knows.
type Vault interface {
	Put(ctx context.Context, token, value string) error
	Get(ctx context.Context, token string) (value string, ok bool, err error)
}

// MemVault is a Vault in memory, for tests and demos. It is not durable: after a
// restart a journal holds tokens that nothing can reveal.
type MemVault struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemVault returns an empty MemVault.
func NewMemVault() *MemVault { return &MemVault{m: map[string]string{}} }

// Put binds token to value. Binding a token to a second value is an error.
func (v *MemVault) Put(_ context.Context, token, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if old, ok := v.m[token]; ok && old != value {
		return errors.New("guard: token already names another value")
	}
	v.m[token] = value
	return nil
}

// Get returns the value behind token.
func (v *MemVault) Get(_ context.Context, token string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.m[token]
	return value, ok, nil
}

// Pseudonymizer replaces each match of Rules with a token "⟦<rule>:<hash>⟧", where
// hash is the first 12 hex characters of the HMAC-SHA256 of the value under Key, and
// puts the pair in Vault. The same value always becomes the same token under one
// Key, so a model can still tell two addresses apart and a resume sees the same text.
//
// It is a harness.Redactor: everything durable, every hook, the model and the
// tools' output see tokens. Reveal and RevealPaths put values back in a tool's
// arguments where the consumer decides an original is needed.
//
// Window is how many bytes of a reply are held back to keep a match whole, as in
// Stream. Zero means the longest match any rule can make, or, when a rule has no
// upper bound, the whole reply, which keeps the ring buffering as Redactor does.
// A vault failure fails closed, as any redactor error does, and its text is
// dropped, because it may hold the value.
type Pseudonymizer struct {
	Rules  []Rule
	Key    []byte
	Vault  Vault
	Window int
}

// Name implements harness.Hook.
func (p Pseudonymizer) Name() string { return "pseudonymize" }

// Validate reports a configuration the stream cannot honour: no key or vault, or a
// Window smaller than a rule's longest match. Run it at start-up. An invalid
// Pseudonymizer fails every model call.
func (p Pseudonymizer) Validate() error {
	_, err := p.streamer()
	return err
}

func (p Pseudonymizer) engine() engine {
	return engine{rules: p.Rules, replace: func(ctx context.Context, r Rule, match string) (string, error) {
		tok := token(p.Key, r.Name, match)
		if err := p.Vault.Put(ctx, tok, match); err != nil {
			return "", errors.New("guard: the vault failed")
		}
		return tok, nil
	}}
}

func (p Pseudonymizer) streamer() (streamer, error) {
	if len(p.Key) == 0 || p.Vault == nil {
		return streamer{}, errors.New("guard: a Pseudonymizer needs a Key and a Vault")
	}
	if p.Window < 0 {
		return streamer{}, fmt.Errorf("guard: window %d is negative", p.Window)
	}
	if p.Window > 0 {
		return newStreamer(p.engine(), p.Window)
	}
	n, err := longest(p.Rules)
	if err != nil {
		return streamer{}, err
	}
	if n < 0 {
		return streamer{p.engine(), -1}, nil
	}
	return streamer{p.engine(), max(n, 1)}, nil
}

func token(key []byte, rule, value string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return "⟦" + rule + ":" + hex.EncodeToString(m.Sum(nil))[:12] + "⟧"
}

// Redact implements harness.Redactor.
func (p Pseudonymizer) Redact(ctx context.Context, _ harness.Kind, text string) (string, error) {
	if len(p.Key) == 0 || p.Vault == nil {
		return "", errors.New("guard: a Pseudonymizer needs a Key and a Vault")
	}
	return p.engine().apply(ctx, "", text)
}

// WrapModel implements harness.ModelMiddleware, so the ring streams live.
func (p Pseudonymizer) WrapModel(r harness.Run, next model.Provider) model.Provider {
	s, err := p.streamer()
	if err != nil {
		return model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
			return model.Fail(err)
		})
	}
	return s.WrapModel(r, next)
}
