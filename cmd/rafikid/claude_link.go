// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"strings"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// claudeLinkSkew bounds how long AFTER the request arrived a delivery may still
// count as one of its messages. On a perfect clock the bound would be zero: an
// accepted message's echo lands before the request it triggers reaches the proxy.
// The echo and the arrival are stamped by two different goroutines (the child's
// stdout, the HTTP server), so a second absorbs the scheduling jitter and no
// more. Anything later belongs to a later request, and linking it here would
// name the wrong row.
const claudeLinkSkew = 1 * time.Second

// claudeLinkQueueCap bounds the deliveries a child may have accepted and not had
// linked to a capture. Overflow drops the OLDEST: a message whose capture never
// arrives is one nothing will ever link, so keeping the newest is what leaves the
// live ones matchable. Exceeding it at all means captures are not being reported
// (a proxy failing every request), which is why the drop is logged.
const claudeLinkQueueCap = 64

// claudeLinkLogBytes bounds the text a link warning quotes. The point of the log
// is to say WHICH message missed, and the first 200 bytes do; a whole prompt
// would put the payload in the daemon's log for no diagnostic gain.
const claudeLinkLogBytes = 200

// linkUnit is what one captured user message says, in the form a match is
// decided on: its own text and how many images it carried. Injected blocks are
// never part of it — an injected block is the daemon's own word, not the message
// a human sent, and it is what a capture reports alongside the real content.
type linkUnit struct {
	text   string
	images int
}

// linkUnitOf reads the unit out of a captured user message. A message with
// neither text content nor an image is not a unit: it is a tool result, a
// reminder that was fully injected, or a row this link has nothing to say about.
func linkUnitOf(um *rafikiv1.UserMessage) (linkUnit, bool) {
	var u linkUnit
	var texts []string
	for _, b := range um.GetContent() {
		switch blk := b.GetBlock().(type) {
		case *rafikiv1.ContentBlock_Text:
			texts = append(texts, blk.Text.GetText())
		case *rafikiv1.ContentBlock_Image:
			u.images++
		}
	}
	u.text = strings.Join(texts, "\n")
	if u.text == "" && u.images == 0 {
		return linkUnit{}, false
	}
	return u, true
}

// looseMatch reports whether captured unit u could be the message sentText and
// sentImages delivered.
//
// It is deliberately loose, because the two sides are not the same string: the
// sent text is what a client wrote, and the captured text is what Claude Code
// reported after its own rewriting (an edited message carries its edit, a
// structured message carries its markup). Whitespace is collapsed on both sides,
// so a reflow is not a mismatch, and containment is accepted in either
// direction — one side may carry a wrapper the other does not. An image-only
// send has no text to compare, so it matches on the image count alone.
//
// A near miss is not a guess: a unit that does not match stays unlinked, and the
// capture is published with no message_id rather than the wrong one.
func looseMatch(sentText string, sentImages int, u linkUnit) bool {
	sent := linkNorm(sentText)
	if sent == "" {
		return sentImages > 0 && u.images == sentImages
	}
	captured := linkNorm(u.text)
	if captured == "" {
		return false
	}
	return strings.Contains(captured, sent) || strings.Contains(sent, captured)
}

func linkNorm(s string) string { return strings.Join(strings.Fields(s), " ") }

func linkTruncate(s string) string {
	if len(s) <= claudeLinkLogBytes {
		return s
	}
	return s[:claudeLinkLogBytes]
}

// linkLog is one deferred WARN from a link decision. The registry decides under
// its own lock and logs nothing there; the caller emits these after unlocking.
type linkLog struct {
	msg      string
	rowID    string
	sent     string
	captured string
	reason   string
}

// link pairs the captured user messages of one request with the deliveries that
// request could have carried, and reports the inbox row for each unit ("" for one
// it could not place).
//
// A delivery is eligible only if it was accepted by the time the request reached
// the proxy (within claudeLinkSkew). The capture for a turn is written AFTER the
// request is forwarded, so a message accepted during this turn cannot be in it:
// without the bound, a capture's rows would be matched against deliveries made
// while the turn ran and the wrong row would be named.
//
// Two shapes decide the pairing:
//
//   - counts agree: pair positionally. Both sides are one request's worth of
//     messages in order, so position IS the answer, and a pair whose text differs
//     is linked anyway with a warning — a rewritten prompt must not cost the row
//     its name.
//   - counts differ: match greedily in order, each unit to the first eligible
//     delivery it loosely matches. A delivery skipped on the way to a later match
//     is dropped as never linked, because the unit that would have named it
//     matched something after it.
//
// Rows linked (and rows skipped) leave the queue; rows no unit matched stay, so a
// later capture can still name them. Linking never mutates a delivery's identity
// and publishes nothing: the caller applies the ids to the events it is about to
// publish.
func (r *claudeDeliveryRegistry) link(childID string, received time.Time, units []linkUnit) (ids []string, logs []linkLog) {
	if len(units) == 0 {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	queue := r.accepted[childID]
	eligible := 0
	for eligible < len(queue) && (received.IsZero() || queue[eligible].echoAt.Before(received.Add(claudeLinkSkew))) {
		eligible++
	}
	upTo := queue[:eligible]
	ids = make([]string, len(units))
	removed := make([]bool, eligible)

	if len(units) == eligible {
		for i := range units {
			ids[i] = upTo[i].rowID
			removed[i] = true
			if !looseMatch(upTo[i].text, upTo[i].images, units[i]) {
				logs = append(logs, linkLog{
					msg:      "claude delivery linked by order; text differs",
					rowID:    upTo[i].rowID,
					sent:     linkTruncate(upTo[i].text),
					captured: linkTruncate(units[i].text),
				})
			}
		}
	} else {
		cursor := 0
		for i, u := range units {
			j := -1
			for k := cursor; k < eligible; k++ {
				if looseMatch(upTo[k].text, upTo[k].images, u) {
					j = k
					break
				}
			}
			if j < 0 {
				if eligible > 0 {
					logs = append(logs, linkLog{
						msg:      "captured user message matched no accepted delivery",
						captured: linkTruncate(u.text),
					})
				}
				continue
			}
			ids[i] = upTo[j].rowID
			removed[j] = true
			for k := cursor; k < j; k++ {
				removed[k] = true
				logs = append(logs, linkLog{
					msg:    "claude delivery never linked",
					rowID:  upTo[k].rowID,
					reason: "a later delivery linked",
				})
			}
			cursor = j + 1
		}
	}

	kept := make([]acceptedDelivery, 0, len(queue))
	for i, a := range queue {
		if i < eligible && removed[i] {
			continue
		}
		kept = append(kept, a)
	}
	if len(kept) == 0 {
		delete(r.accepted, childID)
	} else {
		r.accepted[childID] = kept
	}
	return ids, logs
}

// linkCapturedUserMessages sets MessageId on the captured user messages of one
// main-thread request that delivered a tracked inbox row.
//
// It runs BEFORE the publish loop, on the events that loop is about to publish,
// and publishes nothing itself: the events reach subscribers in ordinal order
// through publishEvent, and linking is part of describing them, not a step
// between them. Everything it decides is in memory; the log lines it emits are
// the registry's deferred warnings, emitted here because the registry never logs
// under its lock.
func (c *Controller) linkCapturedUserMessages(childID string, received time.Time, evs []*rafikiv1.Event) {
	var at []int
	var units []linkUnit
	for i, ev := range evs {
		um := ev.GetUserMessage()
		if um == nil {
			continue
		}
		u, ok := linkUnitOf(um)
		if !ok {
			continue
		}
		at = append(at, i)
		units = append(units, u)
	}
	ids, logs := c.claudeDeliveries.link(childID, received, units)
	for i, id := range ids {
		if id != "" {
			evs[at[i]].GetUserMessage().MessageId = id
		}
	}
	for _, l := range logs {
		attrs := []any{"child", childID}
		if l.rowID != "" {
			attrs = append(attrs, "message_id", l.rowID)
		}
		if l.sent != "" {
			attrs = append(attrs, "sent", l.sent)
		}
		if l.captured != "" {
			attrs = append(attrs, "captured", l.captured)
		}
		if l.reason != "" {
			attrs = append(attrs, "reason", l.reason)
		}
		slog.Warn(l.msg, attrs...)
	}
}
