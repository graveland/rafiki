// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
)

// The event-handling tests moved to pkg/tui/session with the state machine.
// What is left here is what genuinely belongs to the shell: rendering.

func TestRenderProducesOutput(t *testing.T) {
	r := newRenderer()
	blocks := []session.Block{
		{Kind: session.KindUser, Text: "hello", Final: true},
		{Kind: session.KindAssistant, Text: "hi back", Final: true},
	}
	assert.NewAborting(t).NotEmpty(r.Lines(blocks, 2, 100), "Lines returned no output")
}

func TestFingerprintChanges(t *testing.T) {
	c := assert.NewCollecting(t)
	b1 := session.Block{Kind: session.KindAssistant, Text: "hello"}
	b2 := session.Block{Kind: session.KindAssistant, Text: "world"}
	c.NotEq(b2.Fingerprint(), b1.Fingerprint(), "different text should have different fingerprint")
	b3 := session.Block{Kind: session.KindAssistant, Text: "hello", Final: true}
	c.NotEq(b3.Fingerprint(), b1.Fingerprint(), "final vs non-final should have different fingerprint")
}
