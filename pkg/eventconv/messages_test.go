package eventconv_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// node is one transcript node carrying a single text block.
func node(ordinal int, role msg.Role, text string) *conv.Node {
	return &conv.Node{
		Ref: conv.Ref{Ordinal: ordinal},
		Msg: msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: text}}},
	}
}

// textNode is one user node carrying several text blocks, written by origin.
func textNode(ordinal int, origin conv.Origin, texts ...string) *conv.Node {
	blocks := make([]msg.Block, 0, len(texts))
	for _, text := range texts {
		blocks = append(blocks, msg.Text{Text: text})
	}
	return &conv.Node{
		Ref:    conv.Ref{Ordinal: ordinal},
		Msg:    msg.Message{Role: msg.User, Content: blocks},
		Origin: origin,
	}
}

func TestEventsFromNodesCarriesOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
	nodes := []*conv.Node{
		node(0, msg.User, "hello"),
		{Ref: conv.Ref{Ordinal: 1}, Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "hi"}}}, StopReason: "end_turn"},
	}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 2, "got %d events, want 2", len(evs))
	c.Eq(0, evs[0].GetOrdinal(), "event 0 ordinal")
	c.Eq(1, evs[1].GetOrdinal(), "event 1 ordinal")
	c.Eq("c_test", evs[0].ChildId, "child id")
	c.NotNil(evs[0].GetUserMessage(), "event 0 is not a user message")
	c.NotNil(evs[1].GetAssistantMessage(), "event 1 is not an assistant message")
	c.Eq(rafikiv1.StopReason_STOP_REASON_END_TURN, evs[1].GetAssistantMessage().StopReason, "stop reason")
	c.Eq("end_turn", evs[1].GetAssistantMessage().RawStopReason, "raw stop reason")
}

// A stored system row is a message the harness injected, not something the user
// typed, so it gets its own event rather than a user turn's. A user row still
// lands on UserMessage.
func TestEventsFromNodesRendersSystemRole(t *testing.T) {
	c := assert.NewAborting(t)
	nodes := []*conv.Node{
		node(0, msg.System, "Environment update: /tmp is now writable"),
		node(1, msg.User, "carry on"),
	}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 2, "got %d events, want 2", len(evs))
	sys := evs[0].GetSystemMessage()
	c.NotNil(sys, "event 0 is %T, want Event_SystemMessage", evs[0].Payload)
	c.Nil(evs[0].GetUserMessage(), "event 0 must not be a user message")
	c.Eq(0, evs[0].GetOrdinal(), "event 0 ordinal")
	c.Len(sys.GetContent(), 1, "system content blocks")
	c.Eq("Environment update: /tmp is now writable", sys.GetContent()[0].GetText().GetText(), "system text")
	user := evs[1].GetUserMessage()
	c.NotNil(user, "event 1 is %T, want Event_UserMessage", evs[1].Payload)
	c.Nil(evs[1].GetSystemMessage(), "event 1 must not be a system message")
	c.Eq(1, evs[1].GetOrdinal(), "event 1 ordinal")
}

// Each event carries its node's created_at as ts. A node with no recorded
// created_at leaves ts unset rather than stamping the zero time, which would
// read as "year 1" and sort before every real event.
func TestEventsFromNodesCarriesTimestamp(t *testing.T) {
	c := assert.NewAborting(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := t0.Add(90 * time.Second)
	first := node(0, msg.User, "first")
	first.CreatedAt = t0
	second := node(1, msg.Assistant, "second")
	second.CreatedAt = t1
	nodes := []*conv.Node{first, second, node(2, msg.User, "undated")}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 3, "got %d events, want 3", len(evs))
	c.NotNil(evs[0].GetTs(), "event 0 ts is nil")
	c.NotNil(evs[1].GetTs(), "event 1 ts is nil")
	c.Eq(t0, evs[0].GetTs().AsTime().UTC(), "event 0 ts")
	c.Eq(t1, evs[1].GetTs().AsTime().UTC(), "event 1 ts")
	c.Nil(evs[2].GetTs(), "event 2 has a zero CreatedAt, want ts nil")
}

func TestStopReasonNormalizes(t *testing.T) {
	cases := map[string]rafikiv1.StopReason{
		"end_turn":   rafikiv1.StopReason_STOP_REASON_END_TURN,
		"max_tokens": rafikiv1.StopReason_STOP_REASON_MAX_TOKENS,
		"tool_use":   rafikiv1.StopReason_STOP_REASON_TOOL_USE,
		"stop":       rafikiv1.StopReason_STOP_REASON_END_TURN,
		"length":     rafikiv1.StopReason_STOP_REASON_MAX_TOKENS,
		"tool_calls": rafikiv1.StopReason_STOP_REASON_TOOL_USE,
		"":           rafikiv1.StopReason_STOP_REASON_UNSPECIFIED,
		"who_knows":  rafikiv1.StopReason_STOP_REASON_UNSPECIFIED,
	}
	for in, want := range cases {
		got := eventconv.StopReasonFromString(in)
		assert.NewCollecting(t).Eq(want, got, "StopReasonFromString(%q) = %v, want", in, got)
	}
}

func ptr[T any](v T) *T { return &v }

// A kind='compaction_summary' node maps to a CompactionBoundary event, not to
// the ordinary UserMessage branch its role would otherwise land in. A boundary
// with no recorded output size leaves post_tokens unset — a conversation
// compacted before that size was stored — which is what makes the renderer pick
// the one-sided format.
func TestEventsFromNodesMapsCompactionSummaryNode(t *testing.T) {
	c := assert.NewAborting(t)
	boundary := node(1, msg.User, "summary")
	boundary.Kind = conv.KindCompactionSummary
	boundary.InputTokens = ptr(int64(182000))
	nodes := []*conv.Node{node(0, msg.User, "old context"), boundary}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 2, "got %d events, want 2", len(evs))
	c.NotNil(evs[0].GetUserMessage(), "event 0 is %T, want an ordinary user message", evs[0].Payload)
	ev := evs[1]
	c.Eq(1, ev.GetOrdinal(), "event 1 ordinal")
	cb := ev.GetCompactionBoundary()
	c.NotNil(cb, "event 1 is %T, want Event_CompactionBoundary", ev.Payload)
	if cb.GetPreTokens() != 182000 || cb.PreTokens == nil {
		t.Fatalf("pre_tokens = %v, want 182000", cb.PreTokens)
	}
	c.Nil(cb.PostTokens, "post_tokens")
	c.Eq("", cb.Trigger, "trigger")
}

// A summary node with no recorded input_tokens still becomes a boundary, with
// pre_tokens unset rather than a zero that would read as "compaction dropped
// everything".
func TestEventsFromNodesCompactionSummaryWithoutTokens(t *testing.T) {
	boundary := node(4, msg.User, "summary")
	boundary.Kind = conv.KindCompactionSummary

	ev := eventconv.EventsFromNodes("c_test", []*conv.Node{boundary})[0]

	cb := ev.GetCompactionBoundary()
	assert.NewAborting(t).NotNil(cb, "event is %T, want Event_CompactionBoundary", ev.Payload)
	if cb.PreTokens != nil || cb.PostTokens != nil {
		t.Fatalf("pre_tokens=%v post_tokens=%v, want both unset", cb.PreTokens, cb.PostTokens)
	}
}

// A summary node that records the working set it left behind carries it as
// post_tokens: that number is what a continuation from the boundary will
// borrow, and it is the only durable source for it (nothing text-bearing
// follows a /compact boundary).
func TestEventsFromNodesCompactionSummaryCarriesPostTokens(t *testing.T) {
	c := assert.NewAborting(t)
	boundary := node(3, msg.User, "summary")
	boundary.Kind = conv.KindCompactionSummary
	boundary.InputTokens = ptr(int64(182000))
	boundary.OutputTokens = ptr(int64(12000))

	ev := eventconv.EventsFromNodes("c_test", []*conv.Node{boundary})[0]

	cb := ev.GetCompactionBoundary()
	c.NotNil(cb, "event is %T, want Event_CompactionBoundary", ev.Payload)
	c.Require().NotNil(cb.PostTokens, "post_tokens")
	c.Eq(int32(12000), cb.GetPostTokens(), "post_tokens")
	c.Require().NotNil(cb.PreTokens, "pre_tokens")
	c.Eq(int32(182000), cb.GetPreTokens(), "pre_tokens")
}

// kind='compaction_tail' nodes are copies of nodes that already sit before the
// compaction horizon, so the full-history reader must drop them entirely — no
// event, no ordinal — leaving the summary's boundary and the pre-horizon
// messages behind. conv.Store.Page already drops them; this pins the renderer's
// own rule so a caller handing it a raw walk cannot leak the copies.
func TestCompactionEventsFromNodesSkipsTail(t *testing.T) {
	c := assert.NewAborting(t)
	summary := node(2, msg.User, "summary")
	summary.Kind = conv.KindCompactionSummary
	summary.InputTokens = ptr(int64(900))
	tailOne := node(3, msg.User, "hello")
	tailOne.Kind = conv.KindCompactionTail
	tailTwo := node(4, msg.Assistant, "hi")
	tailTwo.Kind = conv.KindCompactionTail
	nodes := []*conv.Node{
		node(0, msg.User, "hello"),
		{Ref: conv.Ref{Ordinal: 1}, Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "hi"}}}, StopReason: "end_turn"},
		summary,
		tailOne,
		tailTwo,
	}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 3, "got %d events, want 3", len(evs))
	c.Eq(0, evs[0].GetOrdinal(), "event 0 ordinal")
	c.Eq(1, evs[1].GetOrdinal(), "event 1 ordinal")
	c.Eq(2, evs[2].GetOrdinal(), "event 2 ordinal")
	c.NotNil(evs[0].GetUserMessage(), "event 0 is %T, want an ordinary user message", evs[0].Payload)
	c.NotNil(evs[1].GetAssistantMessage(), "event 1 is %T, want an ordinary assistant message", evs[1].Payload)
	cb := evs[2].GetCompactionBoundary()
	c.NotNil(cb, "event 2 is %T, want Event_CompactionBoundary", evs[2].Payload)
	if cb.PreTokens == nil || cb.GetPreTokens() != 900 {
		t.Fatalf("pre_tokens = %v, want 900", cb.PreTokens)
	}
}

func TestEventsFromNodesRendersFundiClearAsBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	clearBoundary := node(1, msg.User, store.ClearBoundaryText)
	clearBoundary.Kind = conv.KindClear
	// A claude clear row is the real first message of the new head.
	claudeClear := node(2, msg.User, "real head")
	claudeClear.Kind = conv.KindClear
	nodes := []*conv.Node{node(0, msg.User, "before"), clearBoundary, claudeClear}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Require().Len(evs, 3, "events")
	c.NotNil(evs[0].GetUserMessage(), "ordinary row")
	c.Eq("clear", evs[1].GetCompactionBoundary().GetTrigger(), "synthetic clear row renders as a divider")
	c.NotNil(evs[2].GetUserMessage(), "a claude clear row stays a message")
}

// A claude-code user row carries the harness's reminders in the same block
// list as the user's own text. The split keeps every block's index in the
// stored message, so the original interleaving stays recoverable, and the
// injected half has its wrapper removed.
func TestInjectedSplitForClaudeCodeNode(t *testing.T) {
	c := assert.NewAborting(t)
	const (
		r1       = "<system-reminder>\nreminder one\n</system-reminder>"
		userText = "the user's own words"
		r2       = "<system-reminder>\nreminder two\n</system-reminder>"
	)

	n := textNode(0, conv.OriginClaudeCode, r1, userText, r2)

	evs := eventconv.EventsFromNodes("c_test", []*conv.Node{n})

	c.Len(evs, 1, "got %d events, want 1", len(evs))
	um := evs[0].GetUserMessage()
	c.NotNil(um, "event 0 is %T, want Event_UserMessage", evs[0].Payload)

	content := um.GetContent()
	c.Len(content, 1, "content blocks")
	c.Eq(int32(1), content[0].GetIndex(), "the user's text keeps index 1")
	c.Eq(userText, content[0].GetText().GetText(), "content text")

	injected := um.GetInjected()
	c.Len(injected, 2, "injected blocks")
	c.Eq(int32(0), injected[0].GetIndex(), "first reminder keeps index 0")
	c.Eq("reminder one", injected[0].GetText().GetText(), "first reminder body, wrapper removed")
	c.Eq(int32(2), injected[1].GetIndex(), "second reminder keeps index 2")
	c.Eq("reminder two", injected[1].GetText().GetText(), "second reminder body, wrapper removed")
}

// A fundi node is split too — of rafiki's own envelope, which is the one
// wrapper the daemon wrote rather than the harness. Its kind travels with the
// block, which is what lets a client decorate a job exit differently from a
// restart.
func TestInjectedSplitForFundiNodeCarriesTheKind(t *testing.T) {
	c := assert.NewAborting(t)
	const (
		envelope = `<rafiki-event source="daemon-restart">` + "\n" +
			"the daemon restarted" + "\n" + "</rafiki-event>"
		userText = "the user's own words"
	)

	n := textNode(0, conv.OriginFundi, envelope, userText)

	evs := eventconv.EventsFromNodes("c_test", []*conv.Node{n})
	c.Len(evs, 1, "got %d events, want 1", len(evs))
	um := evs[0].GetUserMessage()
	c.NotNil(um, "event 0 is %T, want Event_UserMessage", evs[0].Payload)

	content := um.GetContent()
	c.Len(content, 1, "content blocks")
	c.Eq(userText, content[0].GetText().GetText(), "the user's text is content")
	c.Eq("", content[0].GetInjectedKind(), "user content names no kind")

	injected := um.GetInjected()
	c.Len(injected, 1, "injected blocks")
	c.Eq("the daemon restarted", injected[0].GetText().GetText(), "the body, wrapper removed")
	c.Eq("daemon-restart", injected[0].GetInjectedKind(), "the kind rides the block")
}

// A fundi node does not know Claude Code's reminder: only rafiki's own envelope
// is recognised there.
func TestInjectedNotSplitForFundiNode(t *testing.T) {
	c := assert.NewAborting(t)
	const (
		r1       = "<system-reminder>\nreminder one\n</system-reminder>"
		userText = "the user's own words"
		r2       = "<system-reminder>\nreminder two\n</system-reminder>"
	)

	n := textNode(0, conv.OriginFundi, r1, userText, r2)

	evs := eventconv.EventsFromNodes("c_test", []*conv.Node{n})

	c.Len(evs, 1, "got %d events, want 1", len(evs))
	um := evs[0].GetUserMessage()
	c.NotNil(um, "event 0 is %T, want Event_UserMessage", evs[0].Payload)

	content := um.GetContent()
	c.Len(content, 3, "content blocks")
	for i, want := range []string{r1, userText, r2} {
		c.Eq(int32(i), content[i].GetIndex(), "block %d index", i)
		c.Eq(want, content[i].GetText().GetText(), "block %d text is unchanged", i)
	}
	c.Len(um.GetInjected(), 0, "a fundi node splits nothing out")
}

// The recorded capture is the real reminder shape: four reminders around the
// user's one line of text, each shown without its wrapper.
func TestInjectedSplitUsesFixture(t *testing.T) {
	c := assert.NewAborting(t)

	raw, err := os.ReadFile(filepath.Join("..", "capture", "testdata", "interim-capture-rows.json"))
	c.NoError(err, "read the capture fixture")

	var doc struct {
		Messages []struct {
			Ordinal int             `json:"ordinal"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	c.NoError(json.Unmarshal(raw, &doc), "decode the capture fixture")

	var in *conv.Node
	for _, m := range doc.Messages {
		if m.Ordinal != 0 {
			continue
		}
		c.Eq("user", m.Role, "message ordinal 0 is a user message")
		var blocks []struct {
			Text string `json:"text"`
			Type string `json:"type"`
		}
		c.NoError(json.Unmarshal(m.Content, &blocks), "decode ordinal 0's content")
		texts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			c.Eq("text", b.Type, "ordinal 0 is all text blocks")
			texts = append(texts, b.Text)
		}
		in = textNode(0, conv.OriginClaudeCode, texts...)
	}
	c.NotNil(in, "the capture fixture has message ordinal 0")

	evs := eventconv.EventsFromNodes("c_test", []*conv.Node{in})

	c.Len(evs, 1, "got %d events, want 1", len(evs))
	um := evs[0].GetUserMessage()
	c.NotNil(um, "event 0 is %T, want Event_UserMessage", evs[0].Payload)
	c.Len(um.GetInjected(), 4, "the fixture's four reminders are injected")
	c.Len(um.GetContent(), 1, "the user's one line is the content")

	for _, cb := range um.GetInjected() {
		text := cb.GetText().GetText()
		c.False(strings.HasPrefix(text, "<system-reminder>"), "injected text %q still carries its wrapper", text)
	}
	c.Eq("Reply with exactly one word: ready", um.GetContent()[0].GetText().GetText(), "the user's own text")
}
