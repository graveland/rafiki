// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

// TestPaneStateIsPerChild pins the fix for C1b's shared-renderer finding: one
// renderer served every child, so hopping had to reset it, and a hop mid-stream
// painted the previous child's live tail into the new child's pane.
func TestPaneStateIsPerChild(t *testing.T) {
	c := &Cockpit{panes: map[string]*paneState{}}

	a := c.pane("c_a")
	b := c.pane("c_b")

	if a == b {
		t.Fatal("two children share one paneState")
	}
	if a.renderer == b.renderer {
		t.Fatal("two children share one renderer — this is the C1b bug")
	}
	if got := c.pane("c_a"); got != a {
		t.Error("pane() did not return the same paneState for the same child")
	}
}

// TestPaneEvictionFollowsSessions: pane state is view state for a session, so
// it must not outlive one. maxSessions bounds memory; panes must obey it too.
func TestPaneEvictionFollowsSessions(t *testing.T) {
	c := &Cockpit{panes: map[string]*paneState{}}
	c.pane("c_gone")

	c.evictPane("c_gone")

	if _, ok := c.panes["c_gone"]; ok {
		t.Error("evictPane left the paneState behind")
	}
}
