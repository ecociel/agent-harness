package guard_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

// sender asks for a tool call whose arguments use the token it was given, in a
// declared path and elsewhere, then answers with what the tool returned.
func sender() *fake.Provider {
	return fake.NewFunc(func(req model.Request) (model.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleTool {
			return fake.Text("done: " + last.Content), nil
		}
		tok := tokenRe.FindString(model.LastUser(req.Messages))
		return fake.Call("send", map[string]any{"to": tok, "body": "hello " + tok, "cc": []string{tok}}), nil
	})
}

func recorder(got *string) tool.Tool {
	return tool.Func(tool.Spec{Name: "send"}, func(_ context.Context, args json.RawMessage) (string, error) {
		*got = string(args)
		var a struct{ To string }
		_ = json.Unmarshal(args, &a)
		return "sent to " + a.To, nil
	})
}

func TestRevealPathsRevealsOnlyWhereDeclared(t *testing.T) {
	v := guard.NewMemVault()
	var got string
	j := harness.NewMemJournal()
	a := &harness.Agent{Name: "a", Journal: j, Provider: sender(), Tools: tool.NewRegistry(recorder(&got)),
		Hooks: []harness.Hook{pseudo(v), guard.RevealPaths(v, map[string][]string{"send": {"to", "cc"}})}}
	in := harness.Prompt("mail " + addr)
	in.RunID = "r"
	res, err := a.Run(context.Background(), in)
	if err != nil || res.Stop != harness.StopCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var args struct {
		To   string
		Body string
		Cc   []string
	}
	if err := json.Unmarshal([]byte(got), &args); err != nil {
		t.Fatal(err, got)
	}
	if args.To != addr || len(args.Cc) != 1 || args.Cc[0] != addr {
		t.Fatalf("declared paths not revealed: %s", got)
	}
	if strings.Contains(args.Body, "anna") || !tokenRe.MatchString(args.Body) {
		t.Fatalf("body was revealed: %s", got)
	}
	evs, _ := j.Load(context.Background(), "r")
	jb, _ := json.Marshal(evs)
	if strings.Contains(string(jb), "anna") || strings.Contains(res.Output, "anna") || !tokenRe.MatchString(res.Output) {
		t.Fatalf("the journal or the answer holds the address: %s", res.Output)
	}
}

func TestRevealRevealsEveryTokenInTheArguments(t *testing.T) {
	v := guard.NewMemVault()
	var got string
	a := &harness.Agent{Provider: sender(), Tools: tool.NewRegistry(recorder(&got)), Hooks: []harness.Hook{pseudo(v), guard.Reveal(v)}}
	if _, err := a.Run(context.Background(), harness.Prompt("mail "+addr)); err != nil {
		t.Fatal(err)
	}
	var args struct{ To, Body string }
	_ = json.Unmarshal([]byte(got), &args)
	if args.To != addr || args.Body != "hello "+addr || tokenRe.MatchString(got) {
		t.Fatal(got)
	}
}

func TestRevealLeavesAnUnknownTokenAndOtherToolsAlone(t *testing.T) {
	v := guard.NewMemVault()
	_ = v.Put(context.Background(), "⟦email:aaaaaaaaaaaa⟧", `a"b\c`)
	args := json.RawMessage(`{"to":"⟦email:aaaaaaaaaaaa⟧","x":"⟦email:bbbbbbbbbbbb⟧","n":1}`)
	call := func(mw harness.ToolMiddleware, name string) string {
		var got string
		next := func(_ context.Context, c harness.Call) (string, error) { got = string(c.Arguments); return "", nil }
		_, _ = mw.WrapTool(harness.Run{}, next)(context.Background(), harness.Call{ToolCall: model.ToolCall{Name: name, Arguments: args}})
		return got
	}
	if got := call(guard.Reveal(v), "t"); got != `{"to":"a\"b\\c","x":"⟦email:bbbbbbbbbbbb⟧","n":1}` {
		t.Fatal(got)
	}
	paths := guard.RevealPaths(v, map[string][]string{"t": {"to", "x"}})
	if got := call(paths, "t"); !strings.Contains(got, `"to":"a\"b\\c"`) || !strings.Contains(got, "⟦email:bbbbbbbbbbbb⟧") {
		t.Fatal(got)
	}
	if got := call(paths, "other"); got != string(args) {
		t.Fatal(got)
	}
}

func TestRevealPathsReachesInsideArrays(t *testing.T) {
	v := guard.NewMemVault()
	_ = v.Put(context.Background(), "⟦email:aaaaaaaaaaaa⟧", "REAL")
	mw := guard.RevealPaths(v, map[string][]string{"t": {"attachments.name"}})
	var got string
	next := func(_ context.Context, c harness.Call) (string, error) { got = string(c.Arguments); return "", nil }
	args := `{"attachments":[{"name":"⟦email:aaaaaaaaaaaa⟧","id":"⟦email:aaaaaaaaaaaa⟧"}]}`
	_, _ = mw.WrapTool(harness.Run{}, next)(context.Background(), harness.Call{ToolCall: model.ToolCall{Name: "t", Arguments: json.RawMessage(args)}})
	if got != `{"attachments":[{"id":"⟦email:aaaaaaaaaaaa⟧","name":"REAL"}]}` {
		t.Fatal(got)
	}
}

func TestRevealFailsClosedWhenTheVaultFails(t *testing.T) {
	ran := false
	next := func(context.Context, harness.Call) (string, error) { ran = true; return "", nil }
	mw := guard.Reveal(failingGet{guard.NewMemVault()})
	_, err := mw.WrapTool(harness.Run{}, next)(context.Background(),
		harness.Call{ToolCall: model.ToolCall{Name: "t", Arguments: json.RawMessage(`{"a":"⟦email:aaaaaaaaaaaa⟧"}`)}})
	if err == nil || ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
}

type failingGet struct{ guard.Vault }

func (failingGet) Get(context.Context, string) (string, bool, error) {
	return "", false, context.DeadlineExceeded
}

func TestRevealText(t *testing.T) {
	v := guard.NewMemVault()
	ctx := context.Background()
	_ = v.Put(ctx, "⟦email:aaaaaaaaaaaa⟧", addr)
	got, err := guard.RevealText(ctx, v, "To ⟦email:aaaaaaaaaaaa⟧ and ⟦email:bbbbbbbbbbbb⟧ and ⟦email:aaaaaaaaaaaa⟧")
	if err != nil || got != "To "+addr+" and ⟦email:bbbbbbbbbbbb⟧ and "+addr {
		t.Fatal(got, err)
	}
	if _, err := guard.RevealText(ctx, failingGet{v}, "⟦email:aaaaaaaaaaaa⟧"); err == nil {
		t.Fatal("a vault failure was swallowed")
	}
}
