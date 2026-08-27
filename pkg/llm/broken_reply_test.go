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
	if text, ok := second.Messages[1].Content[0].OfText, true; !ok || text == nil || text.Text != "first reply" {
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
	for i, m := range second.Messages {
		if len(m.Content) == 0 {
			t.Fatalf("request messages[%d] has no content; the empty reply leaked into history", i)
		}
	}
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
