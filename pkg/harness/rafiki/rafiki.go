// SPDX-License-Identifier: Apache-2.0

// Package rafiki is rafiki's own view of the content it wrote: the messages
// its daemon injects into a conversation — a restart nudge, a background job's
// exit, a settled subagent, a third-party notification. pkg/inject owns the
// envelope's shape; this package is the one place that turns it into a marked
// block.
//
// It is a harness package rather than a helper because the envelope is
// ORIGIN-INDEPENDENT: rafiki wrote it, whether the node was captured by Claude
// Code or by fundi, so every origin's harness composes it. It must not import
// pkg/harness (pkg/harness imports it).
package rafiki

import (
	"slices"

	"go.graveland.dev/rafiki/pkg/inject"
	"go.graveland.dev/rafiki/pkg/msg"
)

// Harness is rafiki's view of its own injected content.
type Harness struct{}

// Mark marks every top-level text block of a user message that is one of
// rafiki's envelopes, recording the kind it names. Nothing is dropped and m is
// not written to: the returned message carries a new Content slice.
func (Harness) Mark(m msg.Message) msg.Message {
	if m.Role != msg.User {
		return m
	}
	blocks := slices.Clone(m.Content)
	for i, b := range blocks {
		t, ok := b.(msg.Text)
		if !ok {
			continue
		}
		kind, _, ok := inject.Classify(t.Text)
		if !ok {
			continue
		}
		t.Injected = true
		t.InjectedKind = string(kind)
		blocks[i] = t
	}
	m.Content = blocks
	return m
}

// Unwrap returns an envelope's body for display, and any other text unchanged.
func (Harness) Unwrap(t msg.Text) string {
	if _, body, ok := inject.Classify(t.Text); ok {
		return body
	}
	return t.Text
}
