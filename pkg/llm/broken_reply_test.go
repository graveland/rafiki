package llm

// These tests cover what happens when a provider sends back a structurally
// broken reply: one with a missing/wrong role, or one with no content blocks
// at all. Both shapes have been seen from real providers. The rule under
// test: the caller receives the reply exactly as it arrived, but history
// only ever stores messages the API will accept back — so one broken reply
// cannot make every later request in the conversation fail.
//
// Store-less conversations (no WithStore) are used so the tests run without
// a database; the history rules are identical in both modes.

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func brokenReplyConversation(t *testing.T, sender Sender) *Conversation {
	t.Helper()
	client, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	conv, err := client.Conversation(context.Background(),
		NewConversation("", "test"),
		Model("sonnet-latest"),
		SystemText("you are a test"))
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	return conv
}

// requestRolesAllValid fails the test if any message in the request carries a
// role the Messages API would reject.
func requestRolesAllValid(t *testing.T, params anthropic.MessageNewParams) {
	t.Helper()
	for i, m := range params.Messages {
		if m.Role != anthropic.MessageParamRoleUser && m.Role != anthropic.MessageParamRoleAssistant {
			t.Fatalf("request messages[%d] has role %q; the API only accepts user or assistant", i, m.Role)
		}
	}
}

// requestBlocksAllStorable fails the test if any message in the request
// carries a content block the Messages API would reject, or a message with no
// content at all.
func requestBlocksAllStorable(t *testing.T, params anthropic.MessageNewParams) {
	t.Helper()
	for i, m := range params.Messages {
		if len(m.Content) == 0 {
			t.Fatalf("request messages[%d] has no content; the API rejects an empty message", i)
		}
		for j, b := range m.Content {
			if b.OfText != nil && strings.TrimSpace(b.OfText.Text) == "" {
				t.Fatalf("request messages[%d].content[%d] is an empty text block; the API rejects it", i, j)
			}
		}
	}
}

// A reply whose role is not "assistant" is stored with role "assistant", so
// the next request is still valid. The reply returned to the caller keeps
// whatever the provider sent.
func TestReplyWithWrongRoleIsStoredAsAssistant(t *testing.T) {
	badRole := func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage(`{"id":"msg_b","type":"message","role":"tool","model":"m",
			"content":[{"type":"text","text":"first reply"}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
	}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		badRole,
		respondText("second reply"),
	}}
	conv := brokenReplyConversation(t, sender)
	ctx := context.Background()

	resp, err := conv.Send(ctx, UserText("hello"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(resp.Role) != "tool" {
		t.Fatalf("caller's copy of the reply was changed: role = %q, want the provider's original \"tool\"", resp.Role)
	}

	if _, err := conv.Send(ctx, UserText("and again")); err != nil {
		t.Fatalf("Send after wrong-role reply: %v", err)
	}
	second := sender.lastReq[len(sender.lastReq)-1]
	requestRolesAllValid(t, second)
	if text := second.Messages[1].Content[0].OfText; text == nil || text.Text != "first reply" {
		t.Fatalf("stored reply content was not preserved: %+v", second.Messages[1].Content)
	}
}

// A reply with no content at all is returned to the caller but not stored:
// there is nothing to store, and an empty message in history would make the
// API reject every later request.
func TestReplyWithNoContentIsNotStored(t *testing.T) {
	empty := func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage(`{"id":"msg_e","type":"message","role":"","model":"m",
			"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`), nil
	}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		empty,
		respondText("real reply"),
	}}
	conv := brokenReplyConversation(t, sender)
	ctx := context.Background()

	resp, err := conv.Send(ctx, UserText("hello"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(resp.Content) != 0 {
		t.Fatalf("caller's copy of the reply was changed: %d content blocks, want 0", len(resp.Content))
	}

	if _, err := conv.Send(ctx, UserText("and again")); err != nil {
		t.Fatalf("Send after empty reply: %v", err)
	}
	second := sender.lastReq[len(sender.lastReq)-1]
	requestRolesAllValid(t, second)
	requestBlocksAllStorable(t, second)
}

// A normal reply is stored exactly as before — same role, same content.
func TestNormalReplyIsStoredUnchanged(t *testing.T) {
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("fine reply"),
		respondText("also fine"),
	}}
	conv := brokenReplyConversation(t, sender)
	ctx := context.Background()

	if _, err := conv.Send(ctx, UserText("hello")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := conv.Send(ctx, UserText("more")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	second := sender.lastReq[len(sender.lastReq)-1]
	if len(second.Messages) != 3 {
		t.Fatalf("second request has %d messages, want 3 (user, assistant, user)", len(second.Messages))
	}
	if second.Messages[1].Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("stored reply role = %q, want assistant", second.Messages[1].Role)
	}
	if second.Messages[1].Content[0].OfText.Text != "fine reply" {
		t.Fatalf("stored reply text = %q, want \"fine reply\"", second.Messages[1].Content[0].OfText.Text)
	}
}

// An empty text block is rejected by the API exactly like a message with no
// content at all ("text content blocks must be non-empty"), so a reply whose
// only block is empty text has nothing storable and is not appended.
func TestReplyWithOnlyEmptyTextIsNotStored(t *testing.T) {
	emptyText := func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage(`{"id":"msg_t0","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":""}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`), nil
	}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		emptyText,
		respondText("real reply"),
	}}
	conv := brokenReplyConversation(t, sender)
	ctx := context.Background()

	resp, err := conv.Send(ctx, UserText("hello"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "" {
		t.Fatalf("caller's copy of the reply was changed: %+v", resp.Content)
	}

	if _, err := conv.Send(ctx, UserText("and again")); err != nil {
		t.Fatalf("Send after empty-text reply: %v", err)
	}
	second := sender.lastReq[len(sender.lastReq)-1]
	requestRolesAllValid(t, second)
	requestBlocksAllStorable(t, second)
}

// A reply that mixes an empty text block with real content keeps the real
// content: only the block the API would reject is dropped.
func TestReplyWithEmptyTextBlockKeepsRealContent(t *testing.T) {
	mixed := func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage(`{"id":"msg_t1","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":""},{"type":"text","text":"real content"}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
	}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		mixed,
		respondText("second reply"),
	}}
	conv := brokenReplyConversation(t, sender)
	ctx := context.Background()

	resp, err := conv.Send(ctx, UserText("hello"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("caller's copy of the reply was changed: %d content blocks, want 2", len(resp.Content))
	}

	if _, err := conv.Send(ctx, UserText("and again")); err != nil {
		t.Fatalf("Send after mixed reply: %v", err)
	}
	second := sender.lastReq[len(sender.lastReq)-1]
	requestRolesAllValid(t, second)
	requestBlocksAllStorable(t, second)
	stored := second.Messages[1].Content
	if len(stored) != 1 || stored[0].OfText == nil || stored[0].OfText.Text != "real content" {
		t.Fatalf("stored reply lost its real content: %+v", stored)
	}
}
