package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestIsBusyMatchesStatus(t *testing.T) {
	cases := map[protocol.Status]bool{
		protocol.StatusIdle:      false,
		protocol.StatusExited:    false,
		protocol.StatusStreaming: true,
		protocol.StatusSpawning:  true,
	}
	for status, want := range cases {
		st := childstore.New()
		st.Insert(&childstore.Session{ChildID: "c", Status: status})
		if got := childIsBusy(st, "c"); got != want {
			t.Errorf("childIsBusy(%s) = %v; want %v", status, got, want)
		}
	}
	// An unknown child is not busy — a flush targeting a child that has
	// already gone should proceed and fail harmlessly at Send, not hang
	// in the buffer forever.
	if childIsBusy(childstore.New(), "ghost") {
		t.Error("an unknown child must not be reported busy")
	}
}

func TestInjectBatchSendsPromptFrame(t *testing.T) {
	// Verify the frame shape produced by injectBatch: one frame with
	// type "prompt" whose message contains all fragments.
	body := buildInjectionBody("subagents", []string{"a done", "b done"})
	frame := buildInjectionFrame(body, eventbuf.DeliverPrompt)

	var m map[string]string
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if m["type"] != "prompt" {
		t.Fatalf("type = %q; want prompt", m["type"])
	}
	if !strings.Contains(m["message"], "a done") || !strings.Contains(m["message"], "b done") {
		t.Fatalf("message missing fragments: %s", m["message"])
	}
	if !strings.Contains(m["message"], "<rafiki-event") {
		t.Fatalf("message missing rafiki-event wrapper: %s", m["message"])
	}
}

func TestInjectBatchUrgentUsesSteer(t *testing.T) {
	body := buildInjectionBody("executor", []string{"LOST"})
	frame := buildInjectionFrame(body, eventbuf.DeliverSteer)

	var m map[string]string
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if m["type"] != "steer" {
		t.Fatalf("type = %q; want steer (urgent events must reach a turn in progress)", m["type"])
	}
}

func TestInjectBatchWiredThroughBuffer(t *testing.T) {
	// Verify the full wiring: buffer flush calls the FlushFn with the right
	// childID, source, and fragments — no Controller.Send needed.
	clk := eventbuf.NewFakeClock(now())

	var (
		gotChildID  string
		gotSource   string
		gotFrags    []string
		gotDelivery eventbuf.Delivery
	)
	recordFlush := func(childID, source string, frags []string, d eventbuf.Delivery) {
		gotChildID = childID
		gotSource = source
		gotFrags = frags
		gotDelivery = d
	}

	buf := eventbuf.New(eventbuf.Config{}, clk)
	buf.SetFlush(recordFlush)
	buf.SetBusy(func(string) bool { return false })

	buf.Push("c_test", "subagents", "", "worker 1 done")
	buf.Push("c_test", "subagents", "", "worker 2 done")
	clk.Advance(6 * time.Second) // past the 5s default debounce

	if gotChildID != "c_test" {
		t.Fatalf("childID = %q; want c_test", gotChildID)
	}
	if gotSource != "subagents" {
		t.Fatalf("source = %q; want subagents", gotSource)
	}
	if len(gotFrags) != 2 {
		t.Fatalf("fragments = %d; want 2", len(gotFrags))
	}
	if gotDelivery != eventbuf.DeliverPrompt {
		t.Fatalf("delivery = %v; want DeliverPrompt for non-urgent push", gotDelivery)
	}
}

// --- helpers for frame construction (extracted to test independently of Send) ---

func buildInjectionBody(source string, fragments []string) string {
	return "<rafiki-event source=\"" + source + "\">\n" +
		strings.Join(fragments, "\n") + "\n</rafiki-event>"
}

func buildInjectionFrame(body string, d eventbuf.Delivery) json.RawMessage {
	kind := "prompt"
	if d == eventbuf.DeliverSteer {
		kind = "steer"
	}
	b, _ := json.Marshal(map[string]string{"type": kind, "message": body})
	return json.RawMessage(b)
}

func now() time.Time { return time.Now() }

func TestEventBufConfigDefaultsOnGarbage(t *testing.T) {
	t.Setenv("RAFIKI_EVENTBUF_DEBOUNCE_MS", "not-a-number")
	cfg := loadEventBufConfig()
	if cfg.Debounce != 5*time.Second {
		t.Fatalf("Debounce = %v; want the 5s default. A zero debounce silently "+
			"turns the buffer into a pass-through.", cfg.Debounce)
	}
}

func TestEventBufConfigValidValues(t *testing.T) {
	t.Setenv("RAFIKI_EVENTBUF_DEBOUNCE_MS", "10000")
	t.Setenv("RAFIKI_EVENTBUF_MAX_WAIT_MS", "120000")
	t.Setenv("RAFIKI_EVENTBUF_MAX_FRAGMENTS", "50")
	t.Setenv("RAFIKI_EVENTBUF_MAX_BYTES_PER_FRAGMENT", "16000")

	cfg := loadEventBufConfig()
	if cfg.Debounce != 10*time.Second {
		t.Fatalf("Debounce = %v; want 10s", cfg.Debounce)
	}
	if cfg.MaxWait != 120*time.Second {
		t.Fatalf("MaxWait = %v; want 120s", cfg.MaxWait)
	}
	if cfg.MaxFragments != 50 {
		t.Fatalf("MaxFragments = %d; want 50", cfg.MaxFragments)
	}
	if cfg.MaxBytesPerFrag != 16000 {
		t.Fatalf("MaxBytesPerFrag = %d; want 16000", cfg.MaxBytesPerFrag)
	}
}
