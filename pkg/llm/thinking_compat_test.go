// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"

	"github.com/multigres/testkit/assert"
)

// TestThinkingCompatStripsBothDirections pins the foreign-thinking rule in both
// directions of provider failover: every Responses signature is dropped on the
// way to Anthropic (whichever mode minted it), an Anthropic signature is dropped
// on the way to Responses, text blocks are untouched, and the caller's history is
// never mutated (a filter on the way out must not rewrite the stored
// conversation).
func TestThinkingCompatStripsBothDirections(t *testing.T) {
	c := assert.NewCollecting(t)

	const (
		anthropicSig = "EqQBCgIYAhIM-sig-from-claude"
		apiSig       = "openai-responses:api:rs_api:gAAAA-api-blob"
		chatgptSig   = "openai-responses:chatgpt:rs_chat:gAAAA-chat-blob"
	)
	mkMessages := func() []anthropic.MessageParam {
		return []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "question"}},
			}},
			{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
				{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "claude thought", Signature: anthropicSig}},
				{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "api thought", Signature: apiSig}},
				{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "chatgpt thought", Signature: chatgptSig}},
				{OfText: &anthropic.TextBlockParam{Text: "answer"}},
			}},
		}
	}

	msgs := mkMessages()

	// Toward Responses: both Responses modes survive the FAMILY filter, which
	// says nothing about which dialect may resume them (each Responses sender
	// applies that itself, at the HTTP level).
	toResponses := llm.StripForeignThinking(msgs, true)
	c.Require().Len(toResponses, 2, "messages")
	c.Require().Len(toResponses[1].Content, 3, "assistant blocks toward Responses")
	c.Eq(apiSig, toResponses[1].Content[0].OfThinking.Signature, "kept api signature")
	c.Eq(chatgptSig, toResponses[1].Content[1].OfThinking.Signature, "kept chatgpt signature")
	c.Eq("answer", toResponses[1].Content[2].OfText.Text, "text block")

	// Toward Anthropic: only the Anthropic signature survives; NO Responses
	// mode does.
	toAnthropic := llm.StripForeignThinking(msgs, false)
	c.Require().Len(toAnthropic, 2, "messages")
	c.Require().Len(toAnthropic[1].Content, 2, "assistant blocks toward Anthropic")
	c.Eq(anthropicSig, toAnthropic[1].Content[0].OfThinking.Signature, "kept signature")
	c.Eq("answer", toAnthropic[1].Content[1].OfText.Text, "text block")

	// The caller's slice and its blocks are untouched.
	c.Require().Len(msgs[1].Content, 4, "caller's assistant blocks")
	c.Eq(anthropicSig, msgs[1].Content[0].OfThinking.Signature, "caller block 0")
	c.Eq(apiSig, msgs[1].Content[1].OfThinking.Signature, "caller block 1")
	c.Eq(chatgptSig, msgs[1].Content[2].OfThinking.Signature, "caller block 2")

	// A message whose every block was foreign keeps an empty content slice: a
	// caller tolerates it (llm.UserContent documents the shape), and dropping
	// the message would lose the turn's role alternation.
	thinkingOnly := []anthropic.MessageParam{
		{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
			{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "x", Signature: anthropicSig}},
		}},
	}
	stripped := llm.StripForeignThinking(thinkingOnly, true)
	c.Require().Len(stripped, 1, "messages")
	c.NotNil(stripped[0].Content, "content slice")
	c.Len(stripped[0].Content, 0, "content blocks")
}

// TestThinkingCompatSignatureShapes pins the signature format every Responses
// sender agrees on: "<prefix><mode>:<item id>:<blob>", with a blob that may be
// empty and an id that may not. The mode names which backend minted the blob;
// the older two-field shape, which did not, no longer parses.
func TestThinkingCompatSignatureShapes(t *testing.T) {
	// IsResponsesSignature is a prefix test and ParseResponsesSignature a shape
	// test: a Responses-PREFIXED signature that does not parse still belongs to
	// this family (so the other direction must drop it), and the request builder
	// is what refuses to resend it.
	cases := []struct {
		name       string
		sig        string
		wantMode   llm.ResponsesSignatureMode
		wantOK     bool
		wantPrefix bool
		wantID     string
		wantBlob   string
	}{
		{"api mode", "openai-responses:api:rs_1:gAAAA-blob", llm.ResponsesModeAPI, true, true, "rs_1", "gAAAA-blob"},
		{"chatgpt mode", "openai-responses:chatgpt:rs_1:gAAAA-blob", llm.ResponsesModeChatGPT, true, true, "rs_1", "gAAAA-blob"},
		{"empty blob", "openai-responses:chatgpt:rs_1:", llm.ResponsesModeChatGPT, true, true, "rs_1", ""},
		{"blob keeps its colons", "openai-responses:api:rs_1:a:b", llm.ResponsesModeAPI, true, true, "rs_1", "a:b"},
		{"no prefix", "EqQBCgIYAhIM", "", false, false, "", ""},
		{"prefix only", "openai-responses:", "", false, true, "", ""},
		{"no separator after mode", "openai-responses:api", "", false, true, "", ""},
		{"missing id", "openai-responses:api::blob", "", false, true, "", ""},
		{"unknown mode", "openai-responses:other:rs_1:blob", "", false, true, "", ""},
		{"old two-field shape", "openai-responses:rs_1:gAAAA-blob", "", false, true, "", ""},
		{"prefix of another name", "openai-lite:rs_1:blob", "", false, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			mode, id, blob, ok := llm.ParseResponsesSignature(tc.sig)
			c.Eq(tc.wantOK, ok, "ok")
			c.Eq(tc.wantMode, mode, "mode")
			c.Eq(tc.wantID, id, "id")
			c.Eq(tc.wantBlob, blob, "blob")
			c.Eq(tc.wantPrefix, llm.IsResponsesSignature(tc.sig), "IsResponsesSignature")
		})
	}
	c := assert.NewCollecting(t)
	c.Eq("openai-responses:", llm.ResponsesSignaturePrefix, "prefix")

	// IsResponsesSignature is deliberately mode-blind: the Anthropic direction
	// must drop every Responses signature, including one that does not parse.
	c.True(llm.IsResponsesSignature("openai-responses:api:rs_1:blob"), "api mode")
	c.True(llm.IsResponsesSignature("openai-responses:chatgpt:rs_1:blob"), "chatgpt mode")
	c.True(llm.IsResponsesSignature("openai-responses:rs_1:blob"), "old two-field shape")
}
