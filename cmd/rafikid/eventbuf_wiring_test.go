package main

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/inbox"
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

// TestIsBusyExemptsScriptChildren pins the wave-5 exemption: a script child
// is never mid-turn — its whole life is one streaming run and delivery is its
// Receive stream's pull — so the busy gate must never withhold a fragment
// from it. Without the exemption a coordinating script waiting on Receive
// for its worker's settle fragment waits the full RAFIKI_EVENTBUF_MAX_WAIT_MS
// (default 60s) every time, against the one consumer actively waiting for
// exactly that news. A fundi child at the SAME status stays busy: the two
// kinds must not drift to the same answer.
func TestIsBusyExemptsScriptChildren(t *testing.T) {
	for _, status := range []protocol.Status{
		protocol.StatusStreaming, protocol.StatusSpawning, protocol.StatusToolRunning,
	} {
		st := childstore.New()
		st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript, Status: status})
		st.Insert(&childstore.Session{ChildID: "c_fundi", Kind: protocol.KindFundi, Status: status})
		if childIsBusy(st, "c_script") {
			t.Errorf("script child at %s must not be reported busy: the busy gate can only withhold its fragments for the full MaxWait, and nothing a flush delivers can interrupt it", status)
		}
		if !childIsBusy(st, "c_fundi") {
			t.Errorf("fundi child at %s must stay busy: a turn may be in flight", status)
		}
	}
	// Exited scripts were already not busy by status; the exemption must not
	// change the fundi answer either.
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript, Status: protocol.StatusExited})
	if childIsBusy(st, "c_script") {
		t.Error("exited script child must not be reported busy")
	}
}

// TestBufferFlushCarriesOrphansWithTheirMode pins the eventbuf->controller
// handoff: the flush names (childID, source) and carries the messages whose
// durable write did not happen. With no Accepter attached every Push is an
// orphan, which is exactly the degraded path the controller must still
// deliver.
func TestBufferFlushCarriesOrphansWithTheirMode(t *testing.T) {
	clk := eventbuf.NewFakeClock(now())

	var (
		gotChildID string
		gotSource  string
		gotOrphans []inbox.Inbound
	)
	buf := eventbuf.New(eventbuf.Config{}, clk)
	buf.SetFlush(func(childID, source string, orphans []inbox.Inbound) {
		gotChildID = childID
		gotSource = source
		gotOrphans = orphans
	})
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
	if len(gotOrphans) != 2 {
		t.Fatalf("orphans = %d; want 2 (nothing is persisted without an Accepter)", len(gotOrphans))
	}
	for _, o := range gotOrphans {
		if o.Mode != inbox.ModePrompt {
			t.Errorf("orphan mode = %v; want prompt for a plain Push", o.Mode)
		}
	}
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
	t.Setenv("RAFIKI_EVENTBUF_MAX_BYTES_PER_FRAGMENT", "16000")

	cfg := loadEventBufConfig()
	if cfg.Debounce != 10*time.Second {
		t.Fatalf("Debounce = %v; want 10s", cfg.Debounce)
	}
	if cfg.MaxWait != 120*time.Second {
		t.Fatalf("MaxWait = %v; want 120s", cfg.MaxWait)
	}
	if cfg.MaxBytesPerFrag != 16000 {
		t.Fatalf("MaxBytesPerFrag = %d; want 16000", cfg.MaxBytesPerFrag)
	}
}

// TestInboxBatchConfigReadsTheBatchCaps pins where the two batch caps moved
// to. They used to sit in eventbuf.Config; coalescing happens at delivery now,
// so an unread env var here means the operator's cap silently does nothing.
func TestInboxBatchConfigReadsTheBatchCaps(t *testing.T) {
	t.Setenv("RAFIKI_EVENTBUF_MAX_FRAGMENTS", "50")
	t.Setenv("RAFIKI_EVENTBUF_MAX_BYTES_PER_FLUSH", "16000")

	cfg := inboxBatchConfig()
	if cfg.MaxFragments != 50 {
		t.Errorf("MaxFragments = %d; want 50", cfg.MaxFragments)
	}
	if cfg.MaxBytesPerFlush != 16000 {
		t.Errorf("MaxBytesPerFlush = %d; want 16000", cfg.MaxBytesPerFlush)
	}
}

func TestInboxBatchConfigDefaults(t *testing.T) {
	cfg := inboxBatchConfig()
	if cfg.MaxFragments != 30 || cfg.MaxBytesPerFlush != 65536 {
		t.Errorf("defaults = %+v; want {30 65536}", cfg)
	}
}
