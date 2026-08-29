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
