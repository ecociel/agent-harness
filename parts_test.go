package harness_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

const scan = "%PDF-1.7 the bytes of a scanned contract"

func withScan(text string) model.Message {
	return model.Message{Role: model.RoleUser, Content: text, Parts: []model.Part{{
		Kind: model.PartDocument, MediaType: "application/pdf", Name: "contract.pdf", Ref: "sha256:c0ffee", Text: "a contract"}}}
}

func fromStore(_ context.Context, p model.Part) ([]byte, error) {
	if p.Ref != "sha256:c0ffee" {
		return nil, errors.New("no such blob")
	}
	return []byte(scan), nil
}

func carriesBytes(msgs []model.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Bytes != nil {
				return true
			}
		}
	}
	return false
}

// The provider gets the bytes. The journal, the sinks, the hooks and the result
// keep the reference and the text, even when the input carried bytes itself.
func TestResolvedBytesReachOnlyTheProvider(t *testing.T) {
	ctx := context.Background()
	var sunk []harness.Event
	ctx = harness.WithEvents(ctx, func(e harness.Event) { sunk = append(sunk, e) })
	var hookSaw []bool
	spy := spyHook{func(r harness.Run, req *model.Request) {
		hookSaw = append(hookSaw, carriesBytes(r.Messages) || carriesBytes(req.Messages))
	}}
	j := harness.NewMemJournal()
	p := fake.Script(fake.Text("signed by both"))
	in := withScan("who signed this?")
	in.Parts[0].Bytes = []byte("bytes the caller should not have sent")
	a := &harness.Agent{Journal: j, Provider: p, Resolve: fromStore, Hooks: []harness.Hook{spy}}
	res, err := a.Run(ctx, harness.Input{RunID: "r", Messages: []model.Message{in}})
	if err != nil || res.Output != "signed by both" {
		t.Fatalf("%v %+v", err, res)
	}
	sent := p.Requests()[0].Messages[0].Parts
	if len(sent) != 1 || string(sent[0].Bytes) != scan || sent[0].Ref != "sha256:c0ffee" {
		t.Fatalf("provider got %+v", sent)
	}
	stored, _ := j.Load(ctx, "r")
	for name, v := range map[string]any{"journal": stored, "sinks": sunk, "result": res} {
		b, _ := json.Marshal(v)
		for _, leak := range []string{scan, "should not have sent", base64.StdEncoding.EncodeToString([]byte(scan))} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s holds part bytes", name)
			}
		}
		if !strings.Contains(string(b), `"ref":"sha256:c0ffee"`) && name != "sinks" {
			t.Errorf("%s lost the reference", name)
		}
	}
	for _, e := range append(stored, sunk...) {
		if carriesBytes(e.Messages) {
			t.Errorf("%s carries part bytes", e.Kind)
		}
	}
	if len(hookSaw) != 1 || hookSaw[0] {
		t.Fatalf("hook saw bytes: %v", hookSaw)
	}
}

func TestAPartTheAgentCannotResolveFailsTheCall(t *testing.T) {
	for name, c := range map[string]struct {
		resolve model.Resolver
		part    model.Part
		class   model.ErrorClass
	}{
		"no resolver": {nil, model.Part{Kind: model.PartImage, Ref: "sha256:1"}, model.ClassInvalid},
		"no ref":      {fromStore, model.Part{Kind: model.PartImage}, model.ClassInvalid},
		"a refusal": {func(context.Context, model.Part) ([]byte, error) {
			return nil, &model.Error{Class: model.ClassContent, Err: errors.New("blob deleted")}
		}, model.Part{Kind: model.PartImage, Ref: "sha256:1"}, model.ClassContent},
	} {
		t.Run(name, func(t *testing.T) {
			p := fake.Script(fake.Text("never"))
			a := &harness.Agent{Provider: p, Resolve: c.resolve}
			res, _ := a.Run(context.Background(), harness.Input{Messages: []model.Message{{Role: model.RoleUser, Parts: []model.Part{c.part}}}})
			if res == nil || res.Failure == nil || res.Failure.Class != c.class {
				t.Fatalf("%+v", res)
			}
			if len(p.Requests()) != 0 {
				t.Fatal("the provider was called")
			}
		})
	}
}

type spyHook struct {
	f func(harness.Run, *model.Request)
}

func (spyHook) Name() string { return "spy" }
func (h spyHook) BeforeModel(_ context.Context, r harness.Run, req *model.Request) error {
	h.f(r, req)
	return nil
}

type maskAHV struct{}

func (maskAHV) Name() string { return "mask" }
func (maskAHV) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, "756.1234.5678.97", "[AHV]"), nil
}

func TestAPartsTextAndNamePassTheRedactors(t *testing.T) {
	j := harness.NewMemJournal()
	p := fake.Script(fake.Text("ok"))
	in := withScan("read this")
	in.Parts[0].Name, in.Parts[0].Text = "ahv-756.1234.5678.97.pdf", "holder 756.1234.5678.97"
	a := &harness.Agent{Journal: j, Provider: p, Resolve: fromStore, Hooks: []harness.Hook{maskAHV{}}}
	if _, err := a.Run(context.Background(), harness.Input{RunID: "r", Messages: []model.Message{in}}); err != nil {
		t.Fatal(err)
	}
	sent := p.Requests()[0].Messages[0].Parts[0]
	if sent.Name != "ahv-[AHV].pdf" || sent.Text != "holder [AHV]" || string(sent.Bytes) != scan {
		t.Fatalf("provider got %+v", sent)
	}
	stored, _ := j.Load(context.Background(), "r")
	if b, _ := json.Marshal(stored); strings.Contains(string(b), "756.1234.5678.97") {
		t.Fatal("the journal holds the raw value")
	}
}
