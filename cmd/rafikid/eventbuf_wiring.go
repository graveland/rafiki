package main

import (
	"os"
	"strconv"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// childIsBusy reports whether a flush to childID must be deferred. Anything
// other than idle or exited means a turn may be in flight.
//
// An unknown child is NOT busy: a batch aimed at a child that has already
// gone must drain and fail at Send rather than sit in the buffer forever.
func childIsBusy(st *childstore.Store, childID string) bool {
	snap, ok := st.Get(childID)
	if !ok {
		return false
	}
	switch snap.Status {
	case protocol.StatusIdle, protocol.StatusExited:
		return false
	}
	return true
}

// wireEventBuffer attaches the buffer to the durable inbox and to the
// controller's flush and busy hooks.
//
// SetAccepter is what makes a pushed fragment durable: without it every Push
// is an orphan, delivered but never persisted. SetFlush hands the buffer's
// "it is time" decision back to the controller, which reads the rows.
func (c *Controller) wireEventBuffer() {
	if c.evbuf == nil {
		return
	}
	c.evbuf.SetAccepter(c.inbox)
	c.evbuf.SetFlush(c.flushInboxSource)
	c.evbuf.SetBusy(func(childID string) bool { return childIsBusy(c.st, childID) })
}

// loadEventBufConfig reads event buffer tunables from the environment.
// A zero or unparseable value falls back to the documented default.
//
// The batch caps (RAFIKI_EVENTBUF_MAX_FRAGMENTS,
// RAFIKI_EVENTBUF_MAX_BYTES_PER_FLUSH) are deliberately absent: coalescing
// happens at delivery over the persisted rows, so they are read by
// inboxBatchConfig instead. What is left here shapes a fragment on the way IN.
func loadEventBufConfig() eventbuf.Config {
	return eventbuf.Config{
		Debounce:        envDuration("RAFIKI_EVENTBUF_DEBOUNCE_MS", 5000),
		MaxWait:         envDuration("RAFIKI_EVENTBUF_MAX_WAIT_MS", 60000),
		MaxBytesPerFrag: envInt("RAFIKI_EVENTBUF_MAX_BYTES_PER_FRAGMENT", 8192),
	}
}

// newEventBuffer constructs the daemon's shared event buffer from environment
// config. Returns nil when the buffer is not configured (a future phase that
// never binds the buffer effectively disables it).
func newEventBuffer() *eventbuf.Buffer {
	cfg := loadEventBufConfig()
	return eventbuf.New(cfg, eventbuf.RealClock())
}

func envInt(name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envDuration(name string, defMs int) time.Duration {
	s := os.Getenv(name)
	if s == "" {
		return time.Duration(defMs) * time.Millisecond
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return time.Duration(defMs) * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}
