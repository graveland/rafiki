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
// package marks ITS OWN content: unclassified and fundi content does not know
// Claude Code's reminder, and is left alone.
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

// rafikiEnvelope is one of the daemon's own injected messages: the envelope
// every origin must recognise, because rafiki wrote it whatever captured the
// node.
const rafikiEnvelope = `<rafiki-event source="jobs">` + "\n" + "job 7 exited 0" + "\n" + "</rafiki-event>"

func TestEveryOriginRecognisesRafikisOwnEnvelope(t *testing.T) {
	c := assert.NewAborting(t)

	for _, origin := range []conv.Origin{conv.OriginUnclassified, conv.OriginFundi, conv.OriginClaudeCode} {
		h := harness.For(origin)
		got := h.Mark(msg.Message{Role: msg.User, Content: []msg.Block{
			msg.Text{Text: "the user's own words"},
			msg.Text{Text: rafikiEnvelope},
		}})
		c.Len(got.Content, 2, "%q: nothing is dropped", origin)
		c.False(got.Content[0].(msg.Text).Injected, "%q: the user's text is not injected", origin)
		t1 := got.Content[1].(msg.Text)
		c.True(t1.Injected, "%q: rafiki's envelope is injected", origin)
		c.Eq("jobs", t1.InjectedKind, "%q: the kind is recorded", origin)
		c.Eq("job 7 exited 0", h.Unwrap(t1), "%q: the body is what is displayed", origin)
	}
}

func TestClaudeReminderCarriesItsOwnKind(t *testing.T) {
	c := assert.NewAborting(t)
	got := harness.For(conv.OriginClaudeCode).Mark(
		msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: reminder}}})
	t0 := got.Content[0].(msg.Text)
	c.True(t0.Injected, "marked")
	c.Eq("claude-reminder", t0.InjectedKind, "claude's reminder has a kind of its own")
}

// A message that merely mentions the envelope is not one: the match is anchored
// at both ends, so prose quoting a frame stays user text.
func TestAnEnvelopeQuotedInProseIsNotInjected(t *testing.T) {
	c := assert.NewAborting(t)
	got := harness.For(conv.OriginFundi).Mark(msg.Message{Role: msg.User, Content: []msg.Block{
		msg.Text{Text: "look at this: " + rafikiEnvelope + " — what does it mean?"},
	}})
	c.False(got.Content[0].(msg.Text).Injected, "quoted prose is not injected")
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
