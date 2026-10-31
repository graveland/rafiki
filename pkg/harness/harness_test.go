package harness_test

import (
	"encoding/json"
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

// TestNoneReplayerIsIdentity pins that an origin with no harness package has
// nothing to replay: its replayer returns every message unchanged.
func TestNoneReplayerIsIdentity(t *testing.T) {
	c := assert.NewAborting(t)

	r := harness.For(conv.OriginFundi).Replayer(func(string) bool { return true })
	in := msg.Message{Role: msg.User, Content: []msg.Block{
		msg.Text{Text: reminder},
		msg.ToolUse{ID: "t1", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/a"}`)},
	}}
	c.EqDeep(in, r.Replay(in), "the none replayer rewrites nothing")

	unclassified := harness.For(conv.OriginUnclassified).Replayer(nil)
	c.EqDeep(in, unclassified.Replay(in), "and neither does the unclassified one")
}
