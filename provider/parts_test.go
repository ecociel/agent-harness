package provider_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/anthropic"
	"github.com/ecociel/agent-harness/provider/openai"
)

func ask(parts ...model.Part) model.Request {
	return model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "what is this?", Parts: parts}}}
}

var (
	png   = model.Part{Kind: model.PartImage, MediaType: "image/png", Ref: "i", Bytes: []byte("PNG")}
	wav   = model.Part{Kind: model.PartAudio, MediaType: "audio/wav", Ref: "a", Bytes: []byte("WAV")}
	ogg   = model.Part{Kind: model.PartAudio, MediaType: "audio/ogg", Ref: "o", Bytes: []byte("OGG"), Text: "a voice note"}
	pdf   = model.Part{Kind: model.PartDocument, MediaType: "application/pdf", Name: "c.pdf", Ref: "d", Bytes: []byte("PDF"), Text: "a contract"}
	notes = model.Part{Kind: model.PartDocument, MediaType: "text/plain", Name: "n.txt", Ref: "n", Bytes: []byte("call Bea")}
	gone  = model.Part{Kind: model.PartImage, MediaType: "image/png", Ref: "g", Text: "a photo, withheld"}
)

func sent(t *testing.T, p model.Provider, s *server, req model.Request) string {
	t.Helper()
	if _, err := model.Collect(p.Complete(context.Background(), req), nil); err != nil {
		t.Fatal(err)
	}
	return content(t, messages(t, s.requests()[0])[0])
}

func TestOpenAISendsPartsBeforeTheText(t *testing.T) {
	s := serve(t, openaiWire.text("ok"))
	got := sent(t, openaiWire.provider(s.URL), s, ask(png, wav, ogg, pdf, gone))
	want := `[{"type":"image_url","image_url":{"url":"data:image/png;base64,UE5H"}},` +
		`{"type":"input_audio","input_audio":{"data":"V0FW","format":"wav"}},` +
		`{"type":"text","text":"a voice note"},{"type":"text","text":"a contract"},` +
		`{"type":"text","text":"a photo, withheld"},{"type":"text","text":"what is this?"}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestOpenAISendsDocumentsAsFilesWhereTheEndpointTakesThem(t *testing.T) {
	s := serve(t, openaiWire.text("ok"))
	got := sent(t, &openai.Provider{BaseURL: s.URL, Files: true}, s, ask(pdf))
	want := `[{"type":"file","file":{"filename":"c.pdf","file_data":"data:application/pdf;base64,UERG"}},{"type":"text","text":"what is this?"}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestAnthropicSendsPartsBeforeTheText(t *testing.T) {
	s := serve(t, anthropicWire.text("ok"))
	got := sent(t, anthropicWire.provider(s.URL), s, ask(png, pdf, notes, ogg, gone))
	want := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"UE5H"}},` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"UERG"},"title":"c.pdf"},` +
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"call Bea"},"title":"n.txt"},` +
		`{"type":"text","text":"a voice note"},{"type":"text","text":"a photo, withheld"},{"type":"text","text":"what is this?"}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, err := model.Collect((&anthropic.Provider{BaseURL: s.URL}).Complete(context.Background(), ask(wav)), nil); model.AsError(err).Class != model.ClassContent {
		t.Fatalf("audio without text: %v", err)
	}
}

func TestAPartWithNeitherBytesNorTextIsRefusedBeforeSending(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t)
		blank := model.Part{Kind: model.PartImage, MediaType: "image/png", Ref: "x"}
		_, err := model.Collect(w.provider(s.URL).Complete(context.Background(), ask(blank)), nil)
		if model.AsError(err).Class != model.ClassContent || len(s.requests()) != 0 {
			t.Fatalf("%v, %d requests", err, len(s.requests()))
		}
	})
}

func TestPartsBelongToUserMessages(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t)
		req := model.Request{Messages: []model.Message{{Role: model.RoleAssistant, Content: "x", Parts: []model.Part{png}}}}
		_, err := model.Collect(w.provider(s.URL).Complete(context.Background(), req), nil)
		if model.AsError(err).Class != model.ClassInvalid || len(s.requests()) != 0 {
			t.Fatalf("%v", err)
		}
	})
}

// refusal is how each API turns down an image, from its published error shapes.
var refusal = map[string]string{
	"openai":    `{"error":{"message":"Invalid image.","type":"invalid_request_error","param":null,"code":"invalid_image"}}`,
	"anthropic": `{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.image.source.base64: image exceeds 5 MB maximum: 7864320 bytes > 5242880 bytes"}}`,
}

func TestARefusedRequestWithAPartIsAContentError(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		for _, c := range []struct {
			name   string
			status int
			body   string
			req    model.Request
			class  model.ErrorClass
		}{
			{"refused with a part", 400, refusal[w.name], ask(png), model.ClassContent},
			{"too large with a part", 413, `{}`, ask(png), model.ClassContent},
			{"unsupported media type", 415, `{}`, ask(png), model.ClassContent},
			{"a full context stays a full context", 400, w.tooLong, ask(png), model.ClassContextLength},
			{"refused with only text alternatives", 400, refusal[w.name], ask(gone), model.ClassInvalid},
			{"refused without parts", 400, refusal[w.name], ask(), model.ClassInvalid},
		} {
			t.Run(c.name, func(t *testing.T) {
				s := serve(t, status(c.status, c.body))
				_, err := model.Collect(w.provider(s.URL).Complete(context.Background(), c.req), nil)
				if e := model.AsError(err); e.Class != c.class {
					t.Fatalf("got %s, want %s: %v", e.Class, c.class, err)
				}
			})
		}
	})
}

// withhold answers a content refusal the way a consumer would: retry once, and send
// the parts as their text alternatives.
type withhold struct{}

func (withhold) Name() string { return "withhold" }
func (withhold) Recover(_ context.Context, _ harness.Run, f harness.Failure) harness.Recovery {
	return harness.Recovery{Retry: f.Class == model.ClassContent && f.Attempt < 2}
}
func (withhold) BeforeModel(_ context.Context, r harness.Run, req *model.Request) error {
	if r.Failure == nil || r.Failure.Class != model.ClassContent {
		return nil
	}
	for i, m := range req.Messages {
		req.Messages[i].Parts = nil
		for _, p := range m.Parts {
			req.Messages[i].Content += "\n[" + p.Text + "]"
		}
	}
	return nil
}

func TestARefusedPartIsWithheldOnTheRetryAndNeverJournaled(t *testing.T) {
	eachWire(t, func(t *testing.T, w wire) {
		s := serve(t, status(400, refusal[w.name]), w.text("a cat"))
		j := &jsonJournal{runs: map[string][][]byte{}}
		a := agent(w.provider(s.URL), j, withhold{})
		a.Resolve = func(context.Context, model.Part) ([]byte, error) { return []byte("PNG"), nil }
		in := harness.Input{RunID: "r", Messages: []model.Message{{Role: model.RoleUser, Content: "what is this?",
			Parts: []model.Part{{Kind: model.PartImage, MediaType: "image/png", Ref: "sha256:9f", Text: "a photo"}}}}}
		res, err := a.Run(context.Background(), in)
		if err != nil || res.Output != "a cat" {
			t.Fatalf("%v %+v", err, res)
		}
		reqs := s.requests()
		if len(reqs) != 2 || !bytes.Contains(reqs[0], []byte("UE5H")) || bytes.Contains(reqs[1], []byte("UE5H")) ||
			!bytes.Contains(reqs[1], []byte(`[a photo]`)) {
			t.Fatalf("requests:\n%s\n%s", reqs[0], reqs[len(reqs)-1])
		}
		if f := failures(t, j); len(f) != 1 || f[0].Class != model.ClassContent {
			t.Fatalf("failures %+v", f)
		}
		stored := journalOf(t, j)
		if bytes.Contains(stored, []byte("UE5H")) || bytes.Contains(stored, []byte("PNG")) || !bytes.Contains(stored, []byte(`"ref":"sha256:9f"`)) {
			t.Fatalf("journal: %s", stored)
		}
	})
}
