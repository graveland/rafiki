package child

// Every claude frame in this file is SYNTHETIC: hand-written from the
// stream-json wire shape to pin the acknowledgement contract, never a recording
// from a real claude binary. The real recordings live in testdata/claude (see
// its NOTES.md) and proving the flag against an actual binary belongs to the
// integration task — these fixtures cannot.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// decodeJSONObject decodes one JSON document into a generic map, failing the
// test rather than returning an error: every caller here is asserting on a
// frame it just produced or wrote.
func decodeJSONObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return obj
}

// messageObject returns the envelope's "message" object.
func messageObject(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	msg, ok := obj["message"].(map[string]any)
	if !ok {
		t.Fatalf("frame %v has no message object", obj)
	}
	return msg
}

// A prompt carrying a delivery uuid puts it at the user envelope's TOP level,
// where claude echoes it back — and never reuses the frame's own `id`, which is
// fundi's consumption handle and a different thing entirely.
func TestClaudeDeliveryEncodeOutboundPromptCarriesTheUUID(t *testing.T) {
	c := assert.NewAborting(t)
	const uuid = "0199a3c1-0000-7000-8000-00000000beef"
	out := ClaudeProvider{}.EncodeOutbound([]byte(
		`{"type":"prompt","id":"F1","delivery_uuid":"` + uuid + `","message":"hello there"}`))
	c.Require().NotNil(out, "EncodeOutbound(prompt with delivery_uuid) = nil, want a frame")

	obj := decodeJSONObject(t, out)
	c.Eq("user", obj["type"], "envelope type")
	c.Eq(uuid, obj["uuid"], "envelope uuid (the acknowledgement key)")
	_, hasID := obj["id"]
	c.False(hasID, "envelope %v carries fundi's frame id as its uuid", obj)
	_, hasDelivery := obj["delivery_uuid"]
	c.False(hasDelivery, "envelope %v leaks the normalized delivery_uuid field; got", obj)
	msg := messageObject(t, obj)
	c.Eq("user", msg["role"], "message role")
	c.Eq("hello there", msg["content"], "message content")
}

// A steer is acknowledged the same way: it is a message claude can accept into
// a turn, so it carries a uuid and is echoed.
func TestClaudeDeliveryEncodeOutboundSteerCarriesTheUUID(t *testing.T) {
	c := assert.NewAborting(t)
	const uuid = "0199a3c1-0000-7000-8000-00000000c0de"
	obj := decodeJSONObject(t, ClaudeProvider{}.EncodeOutbound([]byte(
		`{"type":"steer","delivery_uuid":"`+uuid+`","message":"keep going"}`)))
	c.Eq("user", obj["type"], "envelope type")
	c.Eq(uuid, obj["uuid"], "envelope uuid")
	c.Eq("keep going", messageObject(t, obj)["content"], "message content")
}

// Without a delivery uuid the envelope is exactly what it was before the field
// existed — no empty `uuid` key, nothing for claude to echo by accident.
func TestClaudeDeliveryEncodeOutboundWithoutUUIDIsUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	out := ClaudeProvider{}.EncodeOutbound([]byte(`{"type":"prompt","id":"F1","message":"hi"}`))
	c.Require().NotNil(out, "EncodeOutbound(prompt) = nil")

	obj := decodeJSONObject(t, out)
	c.Eq("user", obj["type"], "envelope type")
	_, hasUUID := obj["uuid"]
	c.False(hasUUID, "envelope %v carries a uuid key without a delivery uuid; got", obj)
	c.Eq(`{"type":"user","message":{"role":"user","content":"hi"}}`, string(out),
		"envelope bytes without a delivery uuid; got %s, want", out)
}

// Images and text survive alongside the uuid: a prompt that is acknowledged is
// still the prompt that was written, images first as llm.UserContent orders them.
func TestClaudeDeliveryEncodeOutboundKeepsImagesAndTextWithTheUUID(t *testing.T) {
	c := assert.NewAborting(t)
	const uuid = "0199a3c1-0000-7000-8000-00000000a11a"
	out := ClaudeProvider{}.EncodeOutbound([]byte(
		`{"type":"prompt","delivery_uuid":"` + uuid + `","message":"what is this",` +
			`"attachments":[{"media_type":"image/png","data":"AQID"}]}`))
	obj := decodeJSONObject(t, out)
	c.Eq(uuid, obj["uuid"], "envelope uuid")

	msg := messageObject(t, obj)
	blocks, ok := msg["content"].([]any)
	c.Require().True(ok, "multimodal content is not a block array: %v", msg["content"])
	c.Require().Eq(2, len(blocks), "want an image block then a text block, got %v", blocks)

	img := blocks[0].(map[string]any)
	c.Eq("image", img["type"], "first block type")
	src := img["source"].(map[string]any)
	c.Eq("base64", src["type"], "image source type")
	c.Eq("image/png", src["media_type"], "image media type")
	c.Eq("AQID", src["data"], "image data")
	c.Eq("text", blocks[1].(map[string]any)["type"], "second block type")
	c.Eq("what is this", blocks[1].(map[string]any)["text"], "text block")
}

// Only prompt and steer are user messages; a uuid on anything else changes
// nothing about what is dropped.
func TestClaudeDeliveryEncodeOutboundDropsUnsupportedFramesWithAUUID(t *testing.T) {
	c := assert.NewAborting(t)
	for _, frame := range []string{
		`{"type":"abort","delivery_uuid":"0199a3c1-0000-7000-8000-00000000dead"}`,
		`{"type":"set_session_name","delivery_uuid":"0199a3c1-0000-7000-8000-00000000dead","name":"x"}`,
		`{"type":"get_state","delivery_uuid":"0199a3c1-0000-7000-8000-00000000dead"}`,
	} {
		c.Nil(ClaudeProvider{}.EncodeOutbound([]byte(frame)), "EncodeOutbound(%s) = want nil (dropped), got", frame)
	}
}

// The acknowledgement is exactly one shape: a user frame claude marked isReplay,
// carrying a uuid, from the main thread. Everything else — a normal user frame,
// an echo without its uuid, a subagent frame, a control_response, an assistant
// or result frame — acknowledges nothing.
func TestClaudeDeliveryParseAcknowledgesOnlyAValidReplayUserFrame(t *testing.T) {
	c := assert.NewAborting(t)
	const uuid = "0199a3c1-0000-7000-8000-000000000001"

	for name, tc := range map[string]struct {
		line string
		want string // "" = no acknowledgement
	}{
		"replay text": {
			line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"uuid":"` + uuid + `","isReplay":true}`,
			want: uuid,
		},
		"replay tool result": {
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},"uuid":"` + uuid + `","isReplay":true}`,
			want: uuid,
		},
		"isReplay false": {
			line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"uuid":"` + uuid + `","isReplay":false}`,
		},
		"isReplay absent": {
			line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"uuid":"` + uuid + `"}`,
		},
		"uuid absent": {
			line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"isReplay":true}`,
		},
		"uuid empty": {
			line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"uuid":"","isReplay":true}`,
		},
		"subagent frame": {
			line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"uuid":"` + uuid + `","isReplay":true,"parent_tool_use_id":"toolu_1"}`,
		},
		"control_response": {
			line: `{"type":"control_response","uuid":"` + uuid + `","isReplay":true,"response":{"subtype":"success"}}`,
		},
		"assistant": {
			line: `{"type":"assistant","uuid":"` + uuid + `","isReplay":true,"message":{"id":"m1","content":[{"type":"text","text":"hi"}]}}`,
		},
		"result": {
			line: `{"type":"result","uuid":"` + uuid + `","isReplay":true,"subtype":"success"}`,
		},
		"malformed": {
			line: `{"type":"user","uuid":`,
		},
	} {
		res := ClaudeProvider{}.Parse([]byte(tc.line))
		got := ""
		for _, e := range res.Events {
			if e.Type != "user_replay" {
				continue
			}
			c.Eq("", got, "%s: more than one user_replay event; got", name)
			got = e.DeliveryUUID
		}
		c.Eq(tc.want, got, "%s: acknowledgement uuid for %s; got", name, tc.line)
	}
}

// A replay echo is a receipt, not content. Neither translator may turn it into
// transcript or tool events — for a text echo or one carrying tool results —
// while the same frames WITHOUT isReplay still translate, which is what makes
// the assertion non-vacuous.
func TestClaudeDeliveryReplayFramesManufactureNoTranscriptOrToolEvents(t *testing.T) {
	c := assert.NewAborting(t)

	frames := map[string]struct{ replay, plain string }{
		"text": {
			replay: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"uuid":"0199a3c1-0000-7000-8000-000000000002","isReplay":true}`,
			plain:  `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		},
		"tool result": {
			replay: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},"uuid":"0199a3c1-0000-7000-8000-000000000003","isReplay":true}`,
			plain:  `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		},
	}

	for name, f := range frames {
		// Parse: the replay frame yields the acknowledgement and NOTHING else —
		// in particular no tool_execution_end, which would move the status
		// machine out of tool_running.
		res := ClaudeProvider{}.Parse([]byte(f.replay))
		c.Require().Eq(1, len(res.Events), "%s: replay Parse events %v; want exactly the acknowledgement, got", name, res.Events)
		c.Eq("user_replay", res.Events[0].Type, "%s: replay Parse event type", name)
		c.False(res.FirstResponse, "%s: a replay echo claims first response", name)
		c.False(res.HasMeta, "%s: a replay echo carries metadata", name)

		// Both translators must suppress it. Fresh providers per frame: the
		// plain twin mutates translator state.
		c.Empty(newClaudeProvider().BusFrames([]byte(f.replay), 1), "%s: replay frame produced bus frames; got", name)
		c.Empty(newClaudeProvider().BusFramesNative([]byte(f.replay), 1), "%s: replay frame produced native events; got", name)

		// The same frame without isReplay still translates (a tool result opens
		// a turn and completes its call; the text frame is inert in both
		// translators, so only the tool-result half is asserted here).
		if name == "tool result" {
			plainBus := newClaudeProvider().BusFrames([]byte(f.plain), 1)
			c.True(len(plainBus) > 0, "%s: the non-replay twin produced no bus frames, so the suppression above proves nothing", name)
			plainNative := newClaudeProvider().BusFramesNative([]byte(f.plain), 1)
			c.Require().Eq(1, len(plainNative), "%s: the non-replay twin's native events; got", name)
			c.NotNil(plainNative[0].GetToolExecutionEnd(), "%s: the non-replay twin emitted %T, not a tool end", name, plainNative[0].GetPayload())
		}
	}
}

// The acknowledgement must reach the daemon while the message gate is holding
// an unpersisted assistant message: it arrives BEFORE the capture, so a
// receipt gated behind that row would arrive after the very thing it precedes.
// The gate is the REAL one, armed by the real provider through a spawned child,
// and the events held behind it stay held until the response is reported.
func TestClaudeDeliveryCallbackFiresWhileTheGateHolds(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newResettableStubRunner()
	native := make(chan *rafikiv1.Event, 8)
	replays := make(chan string, 4)
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:      "c_delivery_gate",
		Cwd:          t.TempDir(),
		Runner:       stub,
		Provider:     ClaudeProvider{},
		NativeSink:   func(ev *rafikiv1.Event) { native <- ev },
		OnUserReplay: func(uuid string) { replays <- uuid },
	})
	c.Require().NoError(err, "spawn")
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })
	c.Require().NotNil(ch.gate, "claude child has no message gate")
	ch.gate.mu.Lock()
	ch.gate.stallAfter = time.Minute // the report, not the backstop, must release this
	ch.gate.mu.Unlock()

	write := func(line string) {
		t.Helper()
		if _, werr := stub.stdoutW.Write([]byte(line + "\n")); werr != nil {
			t.Fatalf("write stdout: %v", werr)
		}
	}

	// One assistant message the capture has NOT persisted, plus the turn end
	// that follows it: both are held behind msg_1.
	write(`{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-5","content":[{"type":"text","text":"working"}]}}`)
	write(`{"type":"result","subtype":"success","session_id":"sess-1"}`)

	deadline := time.Now().Add(5 * time.Second)
	for !ch.gate.holding("msg_1", 2) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	c.Require().True(ch.gate.holding("msg_1", 2), "the assistant frame did not arm the gate behind msg_1")

	// A plain user frame naming a uuid is not an echo, and a replay echo is.
	// Both are written to the same stdout, so the order they reach the callback
	// is the order they were read: receiving the replay's uuid first proves the
	// plain frame acknowledged nothing, without a sleep.
	const (
		noAckUUID = "0199a3c1-0000-7000-8000-0000000000aa"
		ackUUID   = "0199a3c1-0000-7000-8000-0000000000bb"
	)
	write(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"not an echo"}]},"uuid":"` + noAckUUID + `"}`)
	write(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"accepted"}]},"uuid":"` + ackUUID + `","isReplay":true}`)

	select {
	case got := <-replays:
		c.Eq(ackUUID, got, "the callback's uuid")
	case <-time.After(5 * time.Second):
		t.Fatal("the replay echo never reached OnUserReplay while the gate held")
	}

	// The receipt did not release the gate: the held turn events are still
	// waiting on the capture, and none reached the sink.
	c.True(ch.gate.holding("msg_1", 2), "the replay echo released the message gate")
	select {
	case ev := <-native:
		t.Fatalf("event %T reached the sink while msg_1 was unpersisted", ev.GetPayload())
	default:
	}
	select {
	case extra := <-replays:
		t.Fatalf("a second acknowledgement %q fired; only one echo was written", extra)
	default:
	}

	// The capture's report is still what releases the held events, in order.
	ch.MessagePersisted("msg_1")
	seen := make([]string, 0, 2)
	for time.Now().Before(deadline) {
		select {
		case ev := <-native:
			if ev.GetTurnEnd() != nil {
				seen = append(seen, "turn_end")
				c.EqDeep([]string{"turn_start", "turn_end"}, seen, "released in order")
				return
			}
			seen = append(seen, "turn_start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("held events were never released after the report; got %v", seen)
}
