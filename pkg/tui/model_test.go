// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

// The event-handling tests moved to pkg/tui/session with the state machine.
// What is left here is what genuinely belongs to the shell: rendering.

func TestRenderProducesOutput(t *testing.T) {
	r := newRenderer()
	blocks := []session.Block{
		{Kind: session.KindUser, Text: "hello", Final: true},
		{Kind: session.KindAssistant, Text: "hi back", Final: true},
	}
	if out := r.Lines(blocks, 2); len(out) == 0 {
		t.Fatal("Lines returned no output")
	}
}

func TestFingerprintChanges(t *testing.T) {
	b1 := session.Block{Kind: session.KindAssistant, Text: "hello"}
	b2 := session.Block{Kind: session.KindAssistant, Text: "world"}
	if b1.Fingerprint() == b2.Fingerprint() {
		t.Error("different text should have different fingerprint")
	}
	b3 := session.Block{Kind: session.KindAssistant, Text: "hello", Final: true}
	if b1.Fingerprint() == b3.Fingerprint() {
		t.Error("final vs non-final should have different fingerprint")
	}
}
