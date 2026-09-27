package main

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestIsBusyMatchesStatus(t *testing.T) {
	c := assert.NewCollecting(t)
	cases := map[protocol.Status]bool{
		protocol.StatusIdle:      false,
		protocol.StatusExited:    false,
		protocol.StatusStreaming: true,
		protocol.StatusSpawning:  true,
	}
	for status, want := range cases {
		st := childstore.New()
		st.Insert(&childstore.Session{ChildID: "c", Status: status})
		got := childIsBusy(st, "c")
		c.Eq(want, got, "childIsBusy(%s) = %v; want", status, got)
	}
	// An unknown child is not busy — a flush targeting a child that has
	// already gone should proceed and fail harmlessly at Send, not hang
	// in the buffer forever.
	c.False(childIsBusy(childstore.New(), "ghost"), "an unknown child must not be reported busy")
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
	c := assert.NewCollecting(t)
	for _, status := range []protocol.Status{
		protocol.StatusStreaming, protocol.StatusSpawning, protocol.StatusToolRunning,
	} {
		st := childstore.New()
		st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript, Status: status})
		st.Insert(&childstore.Session{ChildID: "c_fundi", Kind: protocol.KindFundi, Status: status})
		c.False(childIsBusy(st, "c_script"), "script child at %s must not be reported busy: the busy gate can only withhold its fragments for the full MaxWait, and nothing a flush delivers can interrupt it", status)
		c.True(childIsBusy(st, "c_fundi"), "fundi child at %s must stay busy: a turn may be in flight", status)
	}
	// Exited scripts were already not busy by status; the exemption must not
	// change the fundi answer either.
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript, Status: protocol.StatusExited})
	c.False(childIsBusy(st, "c_script"), "exited script child must not be reported busy")
}

// TestBufferFlushCarriesOrphansWithTheirMode pins the eventbuf->controller
// handoff: the flush names (childID, source) and carries the messages whose
// durable write did not happen. With no Accepter attached every Push is an
// orphan, which is exactly the degraded path the controller must still
// deliver.
func TestBufferFlushCarriesOrphansWithTheirMode(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Require().Eq("c_test", gotChildID, "childID")
	c.Require().Eq("subagents", gotSource, "source")
	c.Require().Len(gotOrphans, 2, "orphans = %d; want 2 (nothing is persisted without an Accepter)", len(gotOrphans))
	for _, o := range gotOrphans {
		c.Eq(inbox.ModePrompt, o.Mode, "orphan mode")
	}
}

func now() time.Time { return time.Now() }

func TestEventBufConfigDefaultsOnGarbage(t *testing.T) {
	t.Setenv("RAFIKI_EVENTBUF_DEBOUNCE_MS", "not-a-number")
	cfg := loadEventBufConfig()
	assert.NewAborting(t).Eq(5*time.Second, cfg.Debounce, "Debounce = %v; want the 5s default. A zero debounce silently "+
		"turns the buffer into a pass-through.", cfg.Debounce)
}

func TestEventBufConfigValidValues(t *testing.T) {
	t.Setenv("RAFIKI_EVENTBUF_DEBOUNCE_MS", "10000")
	t.Setenv("RAFIKI_EVENTBUF_MAX_WAIT_MS", "120000")
	t.Setenv("RAFIKI_EVENTBUF_MAX_BYTES_PER_FRAGMENT", "16000")
	c := assert.NewAborting(t)

	cfg := loadEventBufConfig()
	c.Eq(10*time.Second, cfg.Debounce, "Debounce")
	c.Eq(120*time.Second, cfg.MaxWait, "MaxWait")
	c.Eq(16000, cfg.MaxBytesPerFrag, "MaxBytesPerFrag")
}

// TestInboxBatchConfigReadsTheBatchCaps pins where the two batch caps moved
// to. They used to sit in eventbuf.Config; coalescing happens at delivery now,
// so an unread env var here means the operator's cap silently does nothing.
func TestInboxBatchConfigReadsTheBatchCaps(t *testing.T) {
	t.Setenv("RAFIKI_EVENTBUF_MAX_FRAGMENTS", "50")
	t.Setenv("RAFIKI_EVENTBUF_MAX_BYTES_PER_FLUSH", "16000")
	c := assert.NewCollecting(t)

	cfg := inboxBatchConfig()
	c.Eq(50, cfg.MaxFragments, "MaxFragments")
	c.Eq(16000, cfg.MaxBytesPerFlush, "MaxBytesPerFlush")
}

func TestInboxBatchConfigDefaults(t *testing.T) {
	cfg := inboxBatchConfig()
	assert.NewCollecting(t).False(cfg.MaxFragments != 30 || cfg.MaxBytesPerFlush != 65536, "defaults = %+v; want {30 65536}", cfg)
}
