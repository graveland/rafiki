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
	// Replayer returns the replayer for a run under another harness whose tool
	// set is has.
	Replayer(has func(name string) bool) Replayer
	// Unwrap returns a marked block's text for display, wrapper removed; an
	// unmarked block's text unchanged.
	Unwrap(t msg.Text) string
}

// Replayer rewrites one origin's messages for a run under another harness,
// in history order. It may carry state between messages (a tool call it
// rendered as text renders its result as text too), so one Replayer serves
// one request and is then discarded.
type Replayer interface {
	Replay(m msg.Message) msg.Message
}

// For returns origin's harness. An origin with no harness package —
// unclassified, fundi — gets one that marks and drops nothing.
func For(origin conv.Origin) Harness {
	switch origin {
	case conv.OriginClaudeCode:
		return claude{}
	default:
		return none{}
	}
}

// claude is claudecode's harness plus the replayer method claudecode cannot
// declare itself: the method returns this package's Replayer interface, and
// claudecode must not import pkg/harness (this package imports it).
type claude struct{ claudecode.Harness }

func (claude) Replayer(has func(name string) bool) Replayer { return claudecode.NewReplayer(has) }

// none is the harness for an origin that has no harness package of its own: it
// marks nothing, drops nothing and unwraps nothing.
type none struct{}

func (none) Mark(m msg.Message) msg.Message { return m }

func (none) ForeignReplay(m msg.Message) msg.Message { return m }

func (none) Replayer(func(name string) bool) Replayer { return noneReplayer{} }

func (none) Unwrap(t msg.Text) string { return t.Text }

// noneReplayer rewrites nothing: an origin with no harness package of its own
// has nothing to replay under another one.
type noneReplayer struct{}

func (noneReplayer) Replay(m msg.Message) msg.Message { return m }
