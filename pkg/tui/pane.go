// SPDX-License-Identifier: Apache-2.0

package tui

import "charm.land/bubbles/v2/viewport"

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
}

// pane returns childID's pane state, creating it on first use.
func (c *Cockpit) pane(childID string) *paneState {
	if c.panes == nil {
		c.panes = map[string]*paneState{}
	}
	p := c.panes[childID]
	if p == nil {
		vp := viewport.New()
		// SoftWrap defaults to FALSE, which truncates a long line and hides
		// the remainder behind horizontal scrolling. An assistant's prose
		// paragraph is ONE content line, so without this the transcript is
		// readable only by scrolling sideways.
		vp.SoftWrap = true
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
