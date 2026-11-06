package harness

import (
	"context"
	"fmt"
	"iter"
	"slices"

	"github.com/ecociel/agent-harness/model"
)

// resolving gives every part of the request its bytes from resolve, on a copy, so
// the bytes exist only between here and p.
func resolving(p model.Provider, resolve model.Resolver) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		if !slices.ContainsFunc(req.Messages, func(m model.Message) bool { return len(m.Parts) > 0 }) {
			return p.Complete(ctx, req)
		}
		req = req.Clone()
		for _, m := range req.Messages {
			for i := range m.Parts {
				part := &m.Parts[i]
				switch {
				case part.Ref == "":
					return model.Fail(&model.Error{Class: model.ClassInvalid, Err: fmt.Errorf("harness: a %s part has no Ref", part.Kind)})
				case resolve == nil:
					return model.Fail(&model.Error{Class: model.ClassInvalid, Err: fmt.Errorf("harness: part %s needs Agent.Resolve", part.Ref)})
				}
				b, err := resolve(ctx, *part)
				if err != nil {
					return model.Fail(fmt.Errorf("harness: resolve %s: %w", part.Ref, err))
				}
				part.Bytes = b
			}
		}
		return p.Complete(ctx, req)
	})
}
