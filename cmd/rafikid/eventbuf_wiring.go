package main

import (
	"encoding/json"
	"log/slog"
	"strings"

	"go.graveland.dev/rafiki/pkg/childstore"
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
// urgent selects the steer path, which injects into a turn already running.
// Reserve it for events that invalidate that turn — budget exhausted,
// executor lost. Subagent transitions are news that keeps, and use prompt.
func (c *Controller) injectBatch(childID, source string, fragments []string, urgent bool) {
	body := "<rafiki-event source=\"" + source + "\">\n" +
		strings.Join(fragments, "\n") + "\n</rafiki-event>"

	kind := "prompt"
	if urgent {
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
