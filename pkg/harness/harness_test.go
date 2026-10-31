package harness_test

import (
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/harness"
	"go.graveland.dev/rafiki/pkg/msg"
)

const reminder = "<system-reminder>\nx\n</system-reminder>"

// TestForUnclassifiedAndFundiAreNoOps pins that only an origin with a harness
// package marks anything: unclassified and fundi content is left alone.
func TestForUnclassifiedAndFundiAreNoOps(t *testing.T) {
	c := assert.NewAborting(t)

	in := msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: reminder}}}

	for _, origin := range []conv.Origin{conv.OriginUnclassified, conv.OriginFundi} {
		got := harness.For(origin).Mark(in)
		c.Len(got.Content, 1, "%q: nothing is dropped", origin)
		c.False(got.Content[0].(msg.Text).Injected, "%q: nothing is marked", origin)
		c.Eq(reminder, harness.For(origin).Unwrap(msg.Text{Text: reminder}), "%q: nothing is unwrapped", origin)
	}

	got := harness.For(conv.OriginClaudeCode).Mark(in)
	c.True(got.Content[0].(msg.Text).Injected, "claude-code marks its own reminder")
}
