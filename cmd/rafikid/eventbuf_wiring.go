package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
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

// injectBatch delivers a coalesced batch to a child as a single frame.
//
// The Delivery decides the frame kind, and nothing else does: prompt queues
// for the child's next turn, steer injects into the turn already running.
// The buffer owns that decision because it owns the busy gate.
func (c *Controller) injectBatch(childID, source string, fragments []string, d eventbuf.Delivery) {
	body := "<rafiki-event source=\"" + source + "\">\n" +
		strings.Join(fragments, "\n") + "\n</rafiki-event>"

	kind := "prompt"
	if d == eventbuf.DeliverSteer {
		kind = "steer"
	}
	frame, err := json.Marshal(map[string]string{"type": kind, "message": body})
	if err != nil {
		slog.Warn("eventbuf: marshal injection frame", "childId", childID, "error", err)
		return
	}
	if err := c.Send(childID, json.RawMessage(frame)); err != nil {
		slog.Warn("eventbuf: inject failed", "childId", childID, "source", source, "error", err)
	}
}

func (c *Controller) wireEventBuffer() {
	if c.evbuf == nil {
		return
	}
	c.evbuf.SetFlush(c.injectBatch)
	c.evbuf.SetBusy(func(childID string) bool { return childIsBusy(c.st, childID) })
}

// loadEventBufConfig reads event buffer tunables from the environment.
// A zero or unparseable value falls back to the documented default.
func loadEventBufConfig() eventbuf.Config {
	return eventbuf.Config{
		Debounce:        envDuration("RAFIKI_EVENTBUF_DEBOUNCE_MS", 5000),
		MaxWait:         envDuration("RAFIKI_EVENTBUF_MAX_WAIT_MS", 60000),
		MaxFragments:    envInt("RAFIKI_EVENTBUF_MAX_FRAGMENTS", 30),
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
