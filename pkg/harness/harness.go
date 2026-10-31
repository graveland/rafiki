// Package harness holds what rafiki knows about the harnesses that write
// conversation content (pkg/conv Origin). A harness's own package is the only
// code that recognises its injected text; everything else consults
// msg.Text.Injected.
package harness

import (
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/harness/claudecode"
	"go.graveland.dev/rafiki/pkg/msg"
)

// Harness is one origin's view of its own content.
type Harness interface {
	// Mark returns m with its injected blocks marked. Nothing is dropped.
	Mark(m msg.Message) msg.Message
	// ForeignReplay returns m for a run under another harness: blocks that
	// are false outside this one are dropped, the rest of its injected blocks
	// marked. Order is preserved.
	ForeignReplay(m msg.Message) msg.Message
	// Unwrap returns a marked block's text for display, wrapper removed; an
	// unmarked block's text unchanged.
	Unwrap(t msg.Text) string
}

// For returns origin's harness. An origin with no harness package —
// unclassified, fundi — gets one that marks and drops nothing.
func For(origin conv.Origin) Harness {
	switch origin {
	case conv.OriginClaudeCode:
		return claudecode.Harness{}
	default:
		return none{}
	}
}

// none is the harness for an origin that has no harness package of its own: it
// marks nothing, drops nothing and unwraps nothing.
type none struct{}

func (none) Mark(m msg.Message) msg.Message { return m }

func (none) ForeignReplay(m msg.Message) msg.Message { return m }

func (none) Unwrap(t msg.Text) string { return t.Text }
