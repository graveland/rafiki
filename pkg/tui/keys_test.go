// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
)

// TestNoGlobalBindingStealsATextareaKey is this chunk's most important test.
//
// handleKey ends by forwarding unmatched keys to the textarea, so any key bound
// GLOBALLY is a key you can no longer type. bubbles/v2/textarea's DefaultKeyMap
// is emacs-heavy and claims pgup, pgdown, shift+up, shift+down, ctrl+n, ctrl+p,
// ctrl+f, ctrl+u, ctrl+k, ctrl+a, ctrl+e and more. Every scroll key the first
// design proposed collided with one, which is why the focus ring exists.
//
// ctrl+b, ctrl+g and ctrl+d are grandfathered: the cockpit already took them
// before this chunk and the design accepts the loss.
func TestNoGlobalBindingStealsATextareaKey(t *testing.T) {
	if got := defaultKeyMap().globalConflicts(); len(got) > 0 {
		t.Errorf("global bindings steal textarea keys: %s", strings.Join(got, ", "))
	}
}

// TestCtrlAReachesTheTextarea pins that ToggleRail moved off ctrl+a: it used
// to be the rail toggle, which stole the textarea's line-start binding —
// ctrl+a is now unclaimed by any global, so it reaches the textarea like any
// other unbound emacs key.
func TestCtrlAReachesTheTextarea(t *testing.T) {
	for _, s := range defaultKeyMap().ToggleRail.Keys() {
		if s == "ctrl+a" {
			t.Fatal("ToggleRail must not bind ctrl+a — it belongs to the textarea's line-start")
		}
	}
	if grandfathered["ctrl+a"] {
		t.Error("ctrl+a should not be grandfathered: nothing global claims it anymore")
	}
}

// TestDefaultKeyMapBindsWhatTheFooterClaims keeps the advertised keys and the
// real ones from drifting — they are separate hand-maintained strings today.
func TestDefaultKeyMapBindsWhatTheFooterClaims(t *testing.T) {
	k := defaultKeyMap()
	for _, tc := range []struct {
		name string
		want string
		b    interface{ Keys() []string }
	}{
		{"quit", "ctrl+c", k.Quit},
		{"nextPane", "tab", k.NextPane},
		{"prevPane", "shift+tab", k.PrevPane},
		{"nextAttention", "alt+n", k.NextAttention},
		{"prevAttention", "alt+p", k.PrevAttention},
		{"toggleRail", "ctrl+r", k.ToggleRail},
		{"toggleRail", "ctrl+b", k.ToggleRail},
		{"help", "ctrl+g", k.Help},
		{"send", "enter", k.Send},
		{"abort", "ctrl+x", k.Abort},
	} {
		found := false
		for _, key := range tc.b.Keys() {
			if key == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %q not among %v", tc.name, tc.want, tc.b.Keys())
		}
	}
}

// ── focus ring ───────────────────────────────────────────────────────────────

// TestCyclePaneSkipsHiddenRail: the ring must never focus something invisible.
// ctrl+b hides the rail, and a focus you cannot see is a cockpit you appear to
// be stuck in — keys stop doing what you expect and nothing on screen says why.
// With the rail hidden the ring has one stop, so ⇥ leaves focus alone.
func TestCyclePaneSkipsHiddenRail(t *testing.T) {
	c := newTestCockpit("c_a")
	c.focus = focusInput
	c.railHidden = true

	c.cyclePane(+1)

	if c.focus != focusInput {
		t.Errorf("focus = %v, want input (a hidden rail must be skipped)", c.focus)
	}
}

// The ring is a two-stop toggle: the transcript pane is gone, because the input
// pane scrolls directly and the third stop cost a press on every agent switch.
func TestCyclePaneTogglesInputAndRail(t *testing.T) {
	c := newTestCockpit("c_a")
	c.focus = focusInput

	c.cyclePane(+1)
	if c.focus != focusRail {
		t.Fatalf("after one tab: focus = %v, want rail", c.focus)
	}
	c.cyclePane(+1)
	if c.focus != focusInput {
		t.Fatalf("after two tabs: focus = %v, want input", c.focus)
	}
}

func TestCyclePaneBackwards(t *testing.T) {
	c := newTestCockpit("c_a")
	c.focus = focusInput

	c.cyclePane(-1)

	if c.focus != focusRail {
		t.Errorf("shift+tab from input: focus = %v, want rail", c.focus)
	}
}

// TestHidingTheRailWhileItHasFocusReleasesIt: ctrl+b is global, so it can fire
// while the rail holds focus. Leaving focus on a hidden pane is the trap.
func TestHidingTheRailWhileItHasFocusReleasesIt(t *testing.T) {
	c := newTestCockpit("c_a")
	c.focus = focusRail
	c.railHidden = true

	c.cyclePane(0)

	if c.focus == focusRail {
		t.Error("focus stayed on the hidden rail")
	}
}

// ^E is the obvious mnemonic for expand and is NOT available: bubbles'
// textarea binds it to end-of-line. ^O is free and matches other agent TUIs.
func TestExpandArgsBindingIsFree(t *testing.T) {
	for _, k := range defaultKeyMap().ExpandArgs.Keys() {
		if textareaKeys[k] && !grandfathered[k] {
			t.Errorf("ExpandArgs binds %q, which the textarea already owns", k)
		}
	}
}

// The lifecycle keys are BARE LETTERS, which is only safe because they live in
// the agents pane — the rail swallows every unmatched key, so nothing there is
// being typed into. Promoting either to a global would make it a character you
// can no longer type in the prompt, which is the exact failure globalConflicts
// exists to catch and would not catch here: the conflict is with ordinary text,
// not with a textarea binding.
func TestLifecycleKeysStayRailLocal(t *testing.T) {
	k := defaultKeyMap()
	globals := map[string]key.Binding{
		"Quit": k.Quit, "NextPane": k.NextPane, "PrevPane": k.PrevPane,
		"NextAttention": k.NextAttention, "PrevAttention": k.PrevAttention,
		"HopPrev": k.HopPrev, "HopNext": k.HopNext,
		"ToggleRail": k.ToggleRail, "Help": k.Help,
		"ExpandArgs": k.ExpandArgs, "Redraw": k.Redraw, "Abort": k.Abort,
	}
	rail := map[string]bool{}
	for _, s := range k.NewAgent.Keys() {
		rail[s] = true
	}
	for _, s := range k.EndAgent.Keys() {
		rail[s] = true
	}
	for name, b := range globals {
		for _, s := range b.Keys() {
			if rail[s] {
				t.Errorf("global %s binds %q, which is a rail lifecycle key", name, s)
			}
		}
	}
}
