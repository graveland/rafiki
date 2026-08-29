package fundi

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
)

type fakeHandler struct {
	prompts, steers []string
	aborts          int
}

func (h *fakeHandler) HandlePrompt(t string) { h.prompts = append(h.prompts, t) }
func (h *fakeHandler) HandleSteer(t string)  { h.steers = append(h.steers, t) }
func (h *fakeHandler) HandleAbort()          { h.aborts++ }
func (h *fakeHandler) State() StateData {
	return StateData{SessionID: "conv-abc", SessionName: "w1", ModelID: "m1", Provider: "anthropic"}
}

func TestGetStateResponseSniffableByDaemon(t *testing.T) {
	in := strings.NewReader(`{"type":"get_state","id":"__bootstrap__"}` + "\n")
	var out bytes.Buffer
	f := NewFrontend(in, &out, &fakeHandler{})
	if err := f.Run(); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(out.String())
	// the daemon's real sniffer must extract our session id + model
	md, ok := child.ExtractMetadata([]byte(line))
	if !ok || md.SessionID != "conv-abc" || md.Model != "anthropic/m1" {
		t.Fatalf("sniff failed: ok=%v md=%+v line=%s", ok, md, line)
	}
	// and identityProvider must see it as the readiness signal
	if !(child.IdentityProvider{}).Parse([]byte(line)).FirstResponse {
		t.Fatal("get_state response did not signal FirstResponse")
	}
}

func TestPromptSteerAbortDispatch(t *testing.T) {
	in := strings.NewReader(
		`{"type":"prompt","message":"do the thing"}` + "\n" +
			`{"type":"steer","message":"also this"}` + "\n" +
			`{"type":"abort"}` + "\n")
	h := &fakeHandler{}
	f := NewFrontend(in, &bytes.Buffer{}, h)
	if err := f.Run(); err != nil {
		t.Fatal(err)
	}
	if len(h.prompts) != 1 || h.prompts[0] != "do the thing" {
		t.Fatalf("prompts: %v", h.prompts)
	}
	if len(h.steers) != 1 || h.steers[0] != "also this" {
		t.Fatalf("steers: %v", h.steers)
	}
	if h.aborts != 1 {
		t.Fatalf("aborts: %d", h.aborts)
	}
}

func TestEmitMarshalFailureWritesFallbackFrame(t *testing.T) {
	var out bytes.Buffer
	f := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})

	// chan int is not JSON-marshalable, forcing Emit's fallback path.
	f.Emit(struct {
		Ch chan int `json:"ch"`
	}{Ch: make(chan int)})

	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatal("Emit wrote nothing on marshal failure")
	}
	var resp struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("fallback frame is not valid JSON: %v (line=%s)", err, line)
	}
	if resp.Type != "agent_error" {
		t.Fatalf("fallback frame type = %q, want agent_error", resp.Type)
	}
	if resp.Error == "" {
		t.Fatal("fallback frame error field is empty")
	}
}

func TestUnknownRequestGetsErrorResponse(t *testing.T) {
	in := strings.NewReader(`{"type":"set_model","id":"7"}` + "\n")
	var out bytes.Buffer
	f := NewFrontend(in, &out, &fakeHandler{})
	_ = f.Run()
	var resp struct {
		Type    string
		Command string
		ID      string
		Success bool
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "response" || resp.Command != "set_model" || resp.ID != "7" || resp.Success {
		t.Fatalf("bad error response: %+v", resp)
	}
}

// idRecorder implements IDHandler as well as Handler, recording which of the
// two pairs Frontend.Run actually called: an entry is "<id>|<text>", and the
// plain methods record an empty id. A handler that never sees the frame id
// cannot ack an inbox row, so this is the seam the durable inbox rests on.
type idRecorder struct {
	prompts []string
	steers  []string
}

func (r *idRecorder) HandlePrompt(text string)       { r.prompts = append(r.prompts, "|"+text) }
func (r *idRecorder) HandleSteer(text string)        { r.steers = append(r.steers, "|"+text) }
func (r *idRecorder) HandlePromptID(id, text string) { r.prompts = append(r.prompts, id+"|"+text) }
func (r *idRecorder) HandleSteerID(id, text string)  { r.steers = append(r.steers, id+"|"+text) }
func (r *idRecorder) HandleAbort()                   {}
func (r *idRecorder) State() StateData               { return StateData{} }

func TestFrontendPassesFrameIDToAnIDHandler(t *testing.T) {
	rec := &idRecorder{}
	in := strings.NewReader(
		`{"type":"prompt","id":"F1","message":"hi"}` + "\n" +
			`{"type":"steer","id":"F2","message":"stop"}` + "\n")
	if err := NewFrontend(in, io.Discard, rec).Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.prompts) != 1 || rec.prompts[0] != "F1|hi" {
		t.Errorf("prompts = %v, want [F1|hi]", rec.prompts)
	}
	if len(rec.steers) != 1 || rec.steers[0] != "F2|stop" {
		t.Errorf("steers = %v, want [F2|stop]", rec.steers)
	}
}

// TestFrontendFallsBackToThePlainHandler is the other half of the extension:
// a Handler that does not implement IDHandler keeps working untouched, which
// is why the id is an extension rather than a widened Handler.
func TestFrontendFallsBackToThePlainHandler(t *testing.T) {
	h := &fakeHandler{}
	in := strings.NewReader(
		`{"type":"prompt","id":"F1","message":"hi"}` + "\n" +
			`{"type":"steer","id":"F2","message":"stop"}` + "\n")
	if err := NewFrontend(in, io.Discard, h).Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.prompts) != 1 || h.prompts[0] != "hi" {
		t.Errorf("prompts = %v, want [hi]", h.prompts)
	}
	if len(h.steers) != 1 || h.steers[0] != "stop" {
		t.Errorf("steers = %v, want [stop]", h.steers)
	}
}
