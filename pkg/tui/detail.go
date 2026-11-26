// SPDX-License-Identifier: Apache-2.0

package tui

// detailLevel is how much of the transcript's collapsible material is drawn in
// full. ^O steps through the levels in order and wraps, so one key reaches
// every state and each level includes everything the one before it shows.
type detailLevel int

const (
	// detailCollapsed folds tool arguments and shortens injected messages.
	detailCollapsed detailLevel = iota
	// detailTools also draws tool-call arguments and results in full.
	detailTools
	// detailAll also draws injected messages at full length.
	detailAll

	detailLevels = int(detailAll) + 1
)

func (d detailLevel) String() string {
	switch d {
	case detailCollapsed:
		return "collapsed"
	case detailTools:
		return "tool calls expanded"
	case detailAll:
		return "tool calls and injected messages expanded"
	}
	return "?"
}

// cycleDetail advances to the next level and derives the two renderer flags
// from it, so the flags can never disagree with the level.
func (c *Cockpit) cycleDetail() {
	c.detail = detailLevel((int(c.detail) + 1) % detailLevels)
	c.expandArgs = c.detail >= detailTools
	c.expandInjected = c.detail >= detailAll
}
