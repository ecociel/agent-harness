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

// Vault keeps the values behind tokens. A later Put of a token replaces its value:
// the latest Put wins. A token keyed on an entity (Tokenize, a Span with a Key)
// keeps its token when the entity is renamed, and reveal must return the new name.
// Put may be called concurrently. A durable run needs a durable vault, because a
// resumed run holds tokens whose values only the vault knows.
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

// Put binds token to value, replacing what it named before.
func (v *MemVault) Put(_ context.Context, token, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
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
// Finders add spans a pattern cannot describe, such as names from a Dictionary. A
// span's token is "⟦<Class>:<hash>⟧" with the hash taken of its Key, so two
// spellings of one entity share a token, and the vault keeps its Value. Rules and
// finders run on the same text: of overlapping spans the longest wins, then the
// earlier source, rules before finders in declared order. A span that touches a
// token already in the text is left alone, so redacting twice changes nothing.
//
// It is a harness.Redactor: everything durable, every hook, the model and the
// tools' output see tokens. Reveal and RevealPaths put values back in a tool's
// arguments where the consumer decides an original is needed.
//
// Window is how many bytes of a reply are held back to keep a match whole, as in
// Stream. Zero means the longest span any rule or finder can make, or, when one has
// no upper bound, the whole reply, which keeps the ring buffering as Redactor does.
// A vault failure fails closed, as any redactor error does, and its text is
// dropped, because it may hold the value.
type Pseudonymizer struct {
	Rules   []Rule
	Finders []Finder
	Key     []byte
	Vault   Vault
	Window  int
}

// Name implements harness.Hook.
func (p Pseudonymizer) Name() string { return "pseudonymize" }

// Validate reports a configuration the stream cannot honour: no key or vault, a
// rule name that cannot stand in a token, or a Window smaller than the longest span
// of a rule or finder. Run it at start-up. An invalid
// Pseudonymizer fails every model call.
func (p Pseudonymizer) Validate() error {
	_, err := p.streamer()
	return err
}

func (p Pseudonymizer) engine() engine {
	return engine{rules: p.Rules, finders: p.Finders, replace: func(ctx context.Context, s Span, match string) (string, error) {
		return p.Tokenize(ctx, s.Class, cmp(s.Key, match), cmp(s.Value, match))
	}}
}

var errNoKey = errors.New("guard: a Pseudonymizer needs a Key and a Vault")

// Tokenize returns the token "⟦<class>:<hash>⟧" of key, the hash taken under Key,
// and puts value in the vault behind it. A span a finder returns becomes the same
// token, so code outside the ring, such as a tool that tokenises the fields it
// returns, names an entity as the ring does. A class that cannot stand in a token
// (empty, over 64 runes, or holding whitespace, ⟦ or ⟧) is an error.
func (p Pseudonymizer) Tokenize(ctx context.Context, class, key, value string) (string, error) {
	if len(p.Key) == 0 || p.Vault == nil {
		return "", errNoKey
	}
	if err := checkClass(class); err != nil {
		return "", fmt.Errorf("guard: %w", err)
	}
	tok := token(p.Key, class, key)
	if err := p.Vault.Put(ctx, tok, value); err != nil {
		return "", errVault
	}
	return tok, nil
}

func (p Pseudonymizer) streamer() (streamer, error) {
	if len(p.Key) == 0 || p.Vault == nil {
		return streamer{}, errNoKey
	}
	for _, r := range p.Rules {
		if err := checkClass(r.Name); err != nil {
			return streamer{}, fmt.Errorf("guard: rule: %w", err)
		}
	}
	if p.Window < 0 {
		return streamer{}, fmt.Errorf("guard: window %d is negative", p.Window)
	}
	if p.Window > 0 {
		return newStreamer(p.engine(), p.Window)
	}
	n, err := longest(p.Rules, p.Finders)
	if err != nil {
		return streamer{}, err
	}
	if n < 0 {
		return streamer{p.engine(), -1}, nil
	}
	return streamer{p.engine(), max(n, 1)}, nil
}

func token(secret []byte, class, key string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(key))
	return "⟦" + class + ":" + hex.EncodeToString(m.Sum(nil))[:12] + "⟧"
}

// Redact implements harness.Redactor.
func (p Pseudonymizer) Redact(ctx context.Context, _ harness.Kind, text string) (string, error) {
	if len(p.Key) == 0 || p.Vault == nil {
		return "", errNoKey
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
