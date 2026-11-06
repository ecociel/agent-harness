package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/ecociel/agent-harness"
)

var tokenRe = regexp.MustCompile(`⟦[^⟦⟧\s]{1,64}:[0-9a-f]{12}⟧`)

// revealString puts values back for the tokens in s that the vault knows. A token
// it does not know stays as it is.
func revealString(ctx context.Context, v Vault, s string) (string, error) {
	var err error
	known := map[string]*string{}
	out := tokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		if err != nil {
			return tok
		}
		val, seen := known[tok]
		if !seen {
			value, ok, gerr := v.Get(ctx, tok)
			if gerr != nil {
				err = errVault
				return tok
			}
			if ok {
				val = &value
			}
			known[tok] = val
		}
		if val == nil {
			return tok
		}
		return *val
	})
	return out, err
}

var errVault = errors.New("guard: the vault failed")

// RevealText puts values back into text for a reader the consumer trusts, such as a
// rendered card for its owner. It never runs inside a run. A token the vault does
// not know stays as it is.
func RevealText(ctx context.Context, v Vault, text string) (string, error) {
	return revealString(ctx, v, text)
}

type revealer struct {
	v     Vault
	paths map[string][]string // nil reveals everywhere
}

func (r revealer) Name() string { return "guard.reveal" }

// Reveal is a harness.ToolMiddleware that replaces every token in a call's
// arguments with its value before the tool runs. It sits outside the ring, so the
// journal records the call with tokens and the tool's output is tokenised again on
// the way back. A token the vault does not know stays as it is, which the tool reads
// as an invalid value. Use RevealPaths when only some arguments may hold originals.
// A vault failure fails the call without running the tool.
func Reveal(v Vault) harness.ToolMiddleware { return revealer{v: v} }

// RevealPaths is Reveal for named tools and named arguments only. paths maps a tool
// name to JSON paths in its arguments, written with dots (`to`, `attachments.name`).
// A path steps through arrays, so `attachments.name` reaches every element's name.
// Every other argument, and every tool not named, keeps its tokens. Revealed
// arguments are re-encoded, so object keys come out sorted.
func RevealPaths(v Vault, paths map[string][]string) harness.ToolMiddleware {
	return revealer{v: v, paths: paths}
}

// WrapTool implements harness.ToolMiddleware.
func (r revealer) WrapTool(_ harness.Run, next harness.ToolFunc) harness.ToolFunc {
	return func(ctx context.Context, c harness.Call) (string, error) {
		args, err := r.arguments(ctx, c.Name, c.Arguments)
		if err != nil {
			return "", err
		}
		c.Arguments = args
		return next(ctx, c)
	}
}

func (r revealer) arguments(ctx context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	if r.paths == nil {
		return r.everywhere(ctx, raw)
	}
	paths := r.paths[name]
	if len(paths) == 0 || !json.Valid(raw) {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw, nil
	}
	for _, p := range paths {
		var err error
		if doc, err = r.at(ctx, doc, strings.Split(p, ".")); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// everywhere replaces tokens in the raw bytes, so nothing else about the arguments
// changes. The value is escaped for a JSON string.
func (r revealer) everywhere(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if !tokenRe.Match(raw) {
		return raw, nil
	}
	var err error
	out := tokenRe.ReplaceAllFunc(raw, func(tok []byte) []byte {
		if err != nil {
			return tok
		}
		val, serr := revealString(ctx, r.v, string(tok))
		if serr != nil {
			err = serr
			return tok
		}
		if val == string(tok) {
			return tok
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(val)
		b := bytes.TrimSpace(buf.Bytes())
		return b[1 : len(b)-1]
	})
	return out, err
}

func (r revealer) at(ctx context.Context, node any, path []string) (any, error) {
	switch x := node.(type) {
	case []any:
		for i := range x {
			n, err := r.at(ctx, x[i], path)
			if err != nil {
				return nil, err
			}
			x[i] = n
		}
		return x, nil
	case string:
		if len(path) == 0 {
			return revealString(ctx, r.v, x)
		}
	case map[string]any:
		if len(path) == 0 {
			return x, nil
		}
		child, ok := x[path[0]]
		if !ok {
			return x, nil
		}
		n, err := r.at(ctx, child, path[1:])
		if err != nil {
			return nil, err
		}
		x[path[0]] = n
	}
	return node, nil
}
