// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"slices"
	"strings"

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

// renderOpts are the display switches that change what the transcript looks
// like without changing the session it is drawn from. Bundled rather than
// passed as four positional bools, which read as a wall of false at every call
// site and silently swap when a fifth arrives.
type renderOpts struct {
	expandArgs   bool
	drawImages   bool
	smallIDs     bool
	showInjected bool
}

// paneSig identifies the rendered content. Two of the fields are geometry
// because a resize genuinely does change every wrapped line, and the last is
// the live tail's fingerprint, which is what moves during streaming.
type paneSig struct {
	blocks       int
	finalized    int
	width        int
	height       int
	expandArgs   bool
	drawImages   bool
	smallIDs     bool
	showInjected bool
	liveFP       string
	// ghost is the unacknowledged sends drawn after the transcript; they come
	// and go without any block changing.
	ghost string
}

// linesFor returns the rendered transcript, or NIL when the pane is already
// showing it. A nil result means "nothing to do" and is distinct from an empty
// one, which means "this transcript has no lines".
func (p *paneState) linesFor(s *session.Session, width, height int, opts renderOpts, ghost []string) []string {
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
		// Same for drawImages — a mode change redraws every image row — and
		// smallIDs, which changes the id colour a ready image renders with.
		// showInjected likewise: it decides which blocks reach the layout.
		expandArgs:   opts.expandArgs,
		drawImages:   opts.drawImages,
		smallIDs:     opts.smallIDs,
		showInjected: opts.showInjected,
		ghost:        strings.Join(ghost, "\x00"),
	}
	if p.sigInit && sig == p.sig {
		return nil
	}
	// A hidden block leaves no line behind, so a cached prefix built in the
	// other mode is not stale, it is wrong. paneSig alone only forces a rebuild
	// through linesFor; the renderer's own cache has to go with it.
	if p.renderer.showInjected != opts.showInjected {
		p.invalidate()
	}
	p.sig, p.sigInit = sig, true
	p.renderer.expandArgs = opts.expandArgs
	p.renderer.drawImages = opts.drawImages
	p.renderer.smallIDs = opts.smallIDs
	p.renderer.showInjected = opts.showInjected
	lines := p.renderer.Lines(s.Blocks, s.Finalized, width)
	if len(ghost) == 0 {
		return lines
	}
	// A copy: lines may be the renderer's cached slice.
	return append(slices.Clone(lines), ghost...)
}

// invalidate forces the next linesFor to rebuild, discarding the renderer's
// cached blocks as well as the viewport signature.
//
// ^L exists for output that never went through slog and landed on the alt
// screen anyway -- an executor subprocess, a library writing to stderr
// directly. Clearing the terminal without this leaves the pane blank until
// the next event happens to change the signature.
func (p *paneState) invalidate() {
	p.sigInit = false
	p.renderer.cached = nil
	p.renderer.cachedUpTo = 0
	p.renderer.lastFP = ""
	p.renderer.liveOut = nil
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
		// The renderer shares the cockpit's ONE image store, so the same bytes
		// seen from two panes are thumbnailed once and share one Kitty id.
		p.renderer.images = c.images
		c.panes[childID] = p
	}
	return p
}

// evictPane drops childID's pane state. Called wherever its session is evicted:
// pane state must never outlive the session it describes.
func (c *Cockpit) evictPane(childID string) {
	delete(c.panes, childID)
}
