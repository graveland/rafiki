// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"charm.land/bubbles/v2/viewport"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

// paneState is the per-child view state of the conversation pane.
//
// One renderer PER CHILD, not one shared. The shared renderer is why hop had
// to call reset(), and it is what C1b's review caught painting one child's
// half-finished paragraph into another child's pane for a whole turn. Per-child
// renderers make that unrepresentable and let a hop back reuse cached work
// instead of re-rendering the transcript.
//
// This is view state, deliberately not on session.Session, which is a pure
// event state machine.
type paneState struct {
	renderer *renderer
	vp       viewport.Model
	// atBottom drives the follow-mode decision and the position readout.
	atBottom bool
	// contentLines is the transcript's REAL length, before the blank rows
	// syncViewport prepends to bottom-anchor a short one. The viewport counts
	// the padding, so asking it would report "12/12 100%" for a one-line
	// conversation and move the number around as the window resized.
	contentLines int

	// sig is what the viewport was last filled from. Rebuilding it is the
	// expensive part of a frame — SetContentLines over a couple of thousand
	// lines — and View runs on EVERY message, so a held arrow key rebuilt the
	// whole transcript per keystroke and the scroll ran on after the key came
	// up. Scrolling changes the offset, not the content, so nothing needs
	// rebuilding for it.
	sig     paneSig
	sigInit bool
}

// paneSig identifies the rendered content. Two of the fields are geometry
// because a resize genuinely does change every wrapped line, and the last is
// the live tail's fingerprint, which is what moves during streaming.
type paneSig struct {
	blocks     int
	finalized  int
	width      int
	height     int
	expandArgs bool
	liveFP     string
}

// linesFor returns the rendered transcript, or NIL when the pane is already
// showing it. A nil result means "nothing to do" and is distinct from an empty
// one, which means "this transcript has no lines".
func (p *paneState) linesFor(s *session.Session, width, height int, expandArgs bool) []string {
	sig := paneSig{
		blocks:    len(s.Blocks),
		finalized: s.Finalized,
		width:     width,
		height:    height,
		// Every unfinalized block, not just the last one: an assistant block
		// stays live until its tool calls resolve, so the one that changes is
		// routinely not the tail.
		liveFP: session.LiveFingerprint(s.Blocks, s.Finalized),
		// expandArgs changes every argument line the renderer draws, so it is
		// part of the signature: without it the toggle flips a flag nothing reads.
		expandArgs: expandArgs,
	}
	if p.sigInit && sig == p.sig {
		return nil
	}
	p.sig, p.sigInit = sig, true
	p.renderer.expandArgs = expandArgs
	return p.renderer.Lines(s.Blocks, s.Finalized, width)
}

// pane returns childID's pane state, creating it on first use.
func (c *Cockpit) pane(childID string) *paneState {
	if c.panes == nil {
		c.panes = map[string]*paneState{}
	}
	p := c.panes[childID]
	if p == nil {
		vp := viewport.New()
		// SoftWrap stays OFF and the RENDERER wraps instead. The viewport
		// re-wraps every line on every Update and every View — 10.9ms and
		// 10.6ms on a 6933-line transcript, against 2.4µs and 167µs without —
		// so a held arrow key outran the screen. Wrapping in the renderer is
		// cached with the rest of the block and repeats the gutter on
		// continuation rows, which soft wrap left bare.
		p = &paneState{renderer: newRenderer(), vp: vp, atBottom: true}
		c.panes[childID] = p
	}
	return p
}

// evictPane drops childID's pane state. Called wherever its session is evicted:
// pane state must never outlive the session it describes.
func (c *Cockpit) evictPane(childID string) {
	delete(c.panes, childID)
}
