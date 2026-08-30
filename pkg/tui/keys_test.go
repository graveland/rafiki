// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
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
