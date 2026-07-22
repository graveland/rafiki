package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"git.graveland.dev/brent/fundi/internal/child"
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
	// and PiProvider must see it as the readiness signal
	if !(child.PiProvider{}).Parse([]byte(line)).FirstResponse {
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
