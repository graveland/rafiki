// SPDX-License-Identifier: Apache-2.0

package tui

import "charm.land/bubbles/v2/key"

// focusPane names the cockpit's three focusable regions. Which one holds focus
// decides what a keystroke means, which is the whole reason bindings are data
// rather than a switch on msg.String().
type focusPane int

const (
	focusInput focusPane = iota
	focusRail
	focusTranscript
)

func (p focusPane) String() string {
	switch p {
	case focusInput:
		return "input"
	case focusRail:
		return "agents"
	case focusTranscript:
		return "transcript"
	}
	return "?"
}

// keyMap holds every binding the cockpit owns.
//
// Bindings listed as GLOBAL are matched before the focused pane gets the key,
// so each one is a key the textarea can never receive. Keep that list short and
// keep globalConflicts passing.
type keyMap struct {
	// Global.
	Quit          key.Binding
	NextPane      key.Binding
	PrevPane      key.Binding
	NextAttention key.Binding
	PrevAttention key.Binding
	HopPrev       key.Binding
	HopNext       key.Binding
	ToggleRail    key.Binding
	Help          key.Binding
	Abort         key.Binding

	// Input pane.
	Send    key.Binding
	Steer   key.Binding
	Newline key.Binding

	// Input pane: scroll the transcript without leaving the input.
	ScrollPageUp   key.Binding
	ScrollPageDown key.Binding
	ScrollLineUp   key.Binding
	ScrollLineDown key.Binding

	// Transcript pane.
	ScrollTop    key.Binding
	ScrollBottom key.Binding

	// Rail pane.
	SelectUp   key.Binding
	SelectDown key.Binding
	Commit     key.Binding

	// Any pane: return to input.
	Escape key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Quit:          key.NewBinding(key.WithKeys("ctrl+c", "ctrl+d"), key.WithHelp("^C", "quit")),
		NextPane:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("⇥", "next pane")),
		PrevPane:      key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧⇥", "prev pane")),
		NextAttention: key.NewBinding(key.WithKeys("alt+n", "ctrl+pgdown"), key.WithHelp("⌥N", "next needing you")),
		PrevAttention: key.NewBinding(key.WithKeys("alt+p", "ctrl+pgup"), key.WithHelp("⌥P", "prev needing you")),
		HopPrev:       key.NewBinding(key.WithKeys("ctrl+up"), key.WithHelp("^↑", "hop up")),
		HopNext:       key.NewBinding(key.WithKeys("ctrl+down"), key.WithHelp("^↓", "hop down")),
		ToggleRail:    key.NewBinding(key.WithKeys("ctrl+b"), key.WithHelp("^B", "rail")),
		Help:          key.NewBinding(key.WithKeys("ctrl+g"), key.WithHelp("^G", "help")),
		// esc first: it is what muscle memory reaches for to stop a running
		// turn, and it is free in the input pane (the textarea ignores it,
		// and the other two panes match their own Escape before this).
		Abort: key.NewBinding(key.WithKeys("esc", "ctrl+x"), key.WithHelp("esc", "abort turn")),

		Send:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "send")),
		Steer: key.NewBinding(key.WithKeys("alt+enter", "ctrl+s"), key.WithHelp("⌥⏎", "steer")),
		// The textarea's own InsertNewline is bound to enter and ctrl+m --
		// the SAME BYTE, and Send takes it -- so a send-on-⏎ input has no
		// newline key at all unless one is given to it explicitly.
		//
		// ^J is not a nicety. A terminal must speak the Kitty keyboard
		// protocol (bubbletea requests it, but plenty of terminals answer
		// nothing) before shift+enter is even reportable; where it is not,
		// the key arrives as a bare CR and SENDS -- the one outcome a newline
		// binding must never have. ^J is LF, distinct from ^M everywhere, and
		// is claimed by neither the textarea's keymap nor any cockpit global.
		Newline: key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"), key.WithHelp("⇧⏎", "newline")),

		// Scrolling from the INPUT pane. Reaching the transcript should not
		// require leaving the box you type in: the whole point of reading back
		// is usually to decide what to type next.
		//
		// pgup/pgdown are taken outright. They are textarea keys, but a
		// three-line box has no pages, so paging it is meaningless and the
		// transcript is what the user meant. ↑/↓ are shared instead: the
		// textarea gets them first and only an ↑ the cursor CANNOT use — no
		// line above it — falls through to the transcript, which is how a
		// shell's history key behaves and costs a multi-line prompt nothing.
		ScrollPageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "scroll")),
		ScrollPageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "scroll")),
		ScrollLineUp:   key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "scroll")),
		ScrollLineDown: key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "scroll")),

		ScrollTop:    key.NewBinding(key.WithKeys("home"), key.WithHelp("home", "top")),
		ScrollBottom: key.NewBinding(key.WithKeys("end"), key.WithHelp("end", "bottom")),

		SelectUp:   key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up")),
		SelectDown: key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down")),
		Commit:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "open")),

		Escape: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back to input")),
	}
}

// textareaKeys is bubbles/v2/textarea's DefaultKeyMap, flattened. Duplicated
// here on purpose: nothing in either library exports it as a set, and a global
// binding that collides with one of these silently stops you typing that
// character. Re-check this list when bubbles is upgraded.
var textareaKeys = map[string]bool{
	"right": true, "ctrl+f": true, "left": true, "ctrl+b": true,
	"alt+right": true, "ctrl+right": true, "alt+f": true,
	"alt+left": true, "ctrl+left": true, "alt+b": true,
	"down": true, "ctrl+n": true, "up": true, "ctrl+p": true,
	"alt+backspace": true, "ctrl+w": true, "ctrl+backspace": true,
	"alt+delete": true, "alt+d": true, "ctrl+delete": true,
	"ctrl+k": true, "ctrl+u": true, "enter": true, "ctrl+m": true,
	"backspace": true, "ctrl+h": true, "delete": true, "ctrl+d": true,
	"home": true, "ctrl+a": true, "end": true, "ctrl+e": true,
	"pgup": true, "pgdown": true, "ctrl+v": true,
	"alt+<": true, "ctrl+home": true, "alt+>": true, "ctrl+end": true,
	"alt+c": true, "alt+l": true, "alt+u": true, "ctrl+t": true,
	"shift+right": true, "shift+left": true,
	"ctrl+shift+right": true, "alt+shift+right": true, "alt+shift+f": true,
	"ctrl+shift+left": true, "alt+shift+left": true, "alt+shift+b": true,
	"shift+up": true, "shift+down": true, "ctrl+g": true, "ctrl+shift+c": true,
}

// grandfathered are textarea keys the cockpit already took before this chunk.
// ctrl+b (rail) and ctrl+g (help) must stay reachable from every pane, and
// ctrl+d quits. The design accepts that you cannot use them while typing.
var grandfathered = map[string]bool{"ctrl+b": true, "ctrl+g": true, "ctrl+d": true}

// globalConflicts returns the names of global bindings that steal a textarea
// key, ignoring the grandfathered three. It must return empty.
func (k keyMap) globalConflicts() []string {
	globals := map[string]key.Binding{
		"Quit": k.Quit, "NextPane": k.NextPane, "PrevPane": k.PrevPane,
		"NextAttention": k.NextAttention, "PrevAttention": k.PrevAttention,
		"HopPrev": k.HopPrev, "HopNext": k.HopNext,
		"ToggleRail": k.ToggleRail, "Help": k.Help, "Abort": k.Abort,
	}
	var out []string
	for name, b := range globals {
		for _, s := range b.Keys() {
			if textareaKeys[s] && !grandfathered[s] {
				out = append(out, name+" ("+s+")")
			}
		}
	}
	return out
}
