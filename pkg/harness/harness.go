// Package harness holds what rafiki knows about the harnesses that write
// conversation content (pkg/conv Origin). A harness's own package is the only
// code that recognises its injected text; everything else consults
// msg.Text.Injected. The one envelope every origin shares is rafiki's own
// (pkg/inject), because its daemon wrote it whatever captured the node.
package harness

import (
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/harness/claudecode"
	"go.graveland.dev/rafiki/pkg/harness/rafiki"
	"go.graveland.dev/rafiki/pkg/inject"
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
	// SplitRelays returns m with a message the user sent mid-turn, which this
	// harness relayed inside content it returned, taken out as the user's own
	// text. It is display-only: eventconv.UserMessageFrom is its only caller,
	// and the stored node keeps its bytes.
	SplitRelays(m msg.Message) (msg.Message, []claudecode.Relayed)
}

// Replayer rewrites one origin's messages for a run under another harness,
// in history order. It may carry state between messages (a tool call it
// rendered as text renders its result as text too), so one Replayer serves
// one request and is then discarded.
type Replayer interface {
	Replay(m msg.Message) msg.Message
}

// For returns origin's harness. An origin with no harness package —
// unclassified, fundi — gets one that recognises rafiki's own envelope and
// drops nothing.
func For(origin conv.Origin) Harness {
	switch origin {
	case conv.OriginClaudeCode:
		return claude{}
	default:
		return plain{}
	}
}

// claude is claudecode's harness composed with rafiki's own: the replayer
// method claudecode cannot declare itself (it returns this package's Replayer
// interface, and claudecode must not import pkg/harness), and the envelope
// rafiki's daemon writes, which is true whatever captured the node.
type claude struct{ claudecode.Harness }

func (claude) Mark(m msg.Message) msg.Message {
	return rafiki.Harness{}.Mark(claudecode.Harness{}.Mark(m))
}

func (claude) ForeignReplay(m msg.Message) msg.Message {
	return rafiki.Harness{}.Mark(claudecode.Harness{}.ForeignReplay(m))
}

func (claude) Unwrap(t msg.Text) string {
	if _, body, ok := inject.Classify(t.Text); ok {
		return body
	}
	return claudecode.Harness{}.Unwrap(t)
}

func (claude) SplitRelays(m msg.Message) (msg.Message, []claudecode.Relayed) {
	return claudecode.Harness{}.SplitRelays(m)
}

func (claude) Replayer(has func(name string) bool) Replayer { return claudecode.NewReplayer(has) }

// plain is the harness for an origin with no harness package of its own
// (fundi, unclassified): it knows only rafiki's own envelope — which its daemon
// wrote, and which no other harness can claim — and marks nothing else, drops
// nothing and unwraps nothing.
type plain struct{}

func (plain) Mark(m msg.Message) msg.Message { return rafiki.Harness{}.Mark(m) }

func (plain) ForeignReplay(m msg.Message) msg.Message { return rafiki.Harness{}.Mark(m) }

func (plain) Replayer(func(name string) bool) Replayer { return noneReplayer{} }

func (plain) Unwrap(t msg.Text) string { return rafiki.Harness{}.Unwrap(t) }

func (plain) SplitRelays(m msg.Message) (msg.Message, []claudecode.Relayed) { return m, nil }

// noneReplayer rewrites nothing: an origin with no harness package of its own
// has nothing to replay under another one.
type noneReplayer struct{}

func (noneReplayer) Replay(m msg.Message) msg.Message { return m }
