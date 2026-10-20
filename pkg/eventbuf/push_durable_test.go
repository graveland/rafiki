package eventbuf

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/inbox"

	"github.com/multigres/testkit/assert"
)

// fakeAccepter stands in for the durable inbox: it records exactly what it was
// handed and can be made to fail, which is the only way to observe that
// PushDurable returns the failure instead of falling back to an orphan.
type fakeAccepter struct {
	mu    sync.Mutex
	calls []inbox.Inbound
	err   error
}

func (f *fakeAccepter) Accept(_ context.Context, in inbox.Inbound) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.err != nil {
		return "", f.err
	}
	return "row-1", nil
}

func (f *fakeAccepter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// A caller that must know the fragment is stored answers its own sender only
// after PushDurable returns nil, so a failure must reach it. Falling back to
// the in-memory orphan here would report success to a third party whose
// webhook is about to be dropped by a daemon restart: the orphan path is right
// for fire-and-forget producers, wrong for one that can retry.
func TestPushDurableReturnsAccepterError(t *testing.T) {
	c := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	acc := &fakeAccepter{err: errors.New("store unavailable")}
	rec := &flushRecorder{}
	b := New(Config{Debounce: 5 * time.Second, MaxWait: 60 * time.Second}, clk)
	b.SetAccepter(acc)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })

	err := b.PushDurable(context.Background(), "c_w", "notify:github", "", "an event")
	c.Error(err, "PushDurable returned nil for a failed durable accept; the caller would answer its sender success")
	c.ErrorIs(err, acc.err, "PushDurable must wrap the accepter's error, not replace it")
	c.Eq(1, acc.callCount(), "accepter calls")

	c.Eq(0, rec.n(), "flushes immediately after the failed push")
	c.False(b.Pending("c_w"), "Pending: a failed durable push must not queue an in-memory orphan")
	clk.Advance(b.cfg.Debounce + time.Minute) // debounce plus a margin
	c.Eq(0, rec.n(), "flushes after the debounce window plus a margin; the failed push armed a timer")
	c.False(b.Pending("c_w"), "Pending after the debounce window plus a margin")
}

// On success PushDurable behaves exactly like Push: the row is already in the
// store, and the debounce arms so the batch goes out as one turn.
func TestPushDurableSuccessArmsAndFlushes(t *testing.T) {
	c := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	acc := &fakeAccepter{}
	rec := &flushRecorder{}
	b := New(Config{Debounce: 5 * time.Second, MaxWait: 60 * time.Second}, clk)
	b.SetAccepter(acc)
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return false })

	c.NoError(b.PushDurable(context.Background(), "c_w", "notify:github", "pull_request", "opened #1"),
		"PushDurable")

	c.Eq(1, acc.callCount(), "accepter calls")
	in := acc.calls[0]
	c.False(in.ChildID != "c_w" || in.Source != "notify:github" || in.Key != "pull_request" || in.Text != "opened #1",
		"persisted inbound = %+v; want (c_w, notify:github, pull_request, opened #1)", in)
	c.Eq(inbox.ModePrompt, in.Mode, "mode")
	c.True(b.Pending("c_w"), "Pending after a successful durable push, before the debounce elapses")

	clk.Advance(5 * time.Second)
	c.Eq(1, rec.n(), "flushes after the debounce")
	got := rec.call(0)
	c.False(got.childID != "c_w" || got.source != "notify:github",
		"flush target = (%q, %q); want (c_w, notify:github)", got.childID, got.source)
	c.Empty(got.orphans, "orphans; a stored fragment is not an orphan")
	c.False(b.Pending("c_w"), "Pending after the flush")
}

// Push only logs a "::" source and drops it. A caller that has to answer its
// sender needs the rejection, and the store must never be touched.
func TestPushDurableRejectsDoubleColonSource(t *testing.T) {
	c := assert.NewAborting(t)
	quietLogs(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	acc := &fakeAccepter{}
	b := New(Config{Debounce: time.Second}, clk)
	b.SetAccepter(acc)
	b.SetFlush(func(string, string, []inbox.Inbound) { t.Error("flushed a rejected source") })

	err := b.PushDurable(context.Background(), "c_w", "bad::source", "", "x")
	c.Error(err, "PushDurable accepted a source containing ::")
	c.StrContains(err.Error(), "::", "error text")
	c.Eq(0, acc.callCount(), "accepter calls; a rejected source must never reach the store")
	c.False(b.Pending("c_w"), "Pending after a rejected source")

	clk.Advance(10 * time.Second)
	c.Eq(0, acc.callCount(), "accepter calls after the debounce")
}

// Without an accepter there is nothing durable to write to, so the promise
// "stored before the sender is answered" cannot be kept and the call fails
// rather than degrading to the in-memory path.
func TestPushDurableNoAccepter(t *testing.T) {
	c := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	b := New(Config{Debounce: time.Second}, clk)
	b.SetFlush(func(string, string, []inbox.Inbound) { t.Error("flushed with no accepter") })

	err := b.PushDurable(context.Background(), "c_w", "notify:github", "", "x")
	c.Error(err, "PushDurable with no accepter")
	c.StrContains(strings.ToLower(err.Error()), "accepter", "error text must name the missing accepter")
	c.False(b.Pending("c_w"), "Pending after a push with no accepter")
}
