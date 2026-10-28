// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"

	"github.com/multigres/testkit/assert"
)

// TestThinkingCompatStripsBothDirections pins the foreign-thinking rule in both
// directions of provider failover: a Responses signature is dropped on the way to
// Anthropic, an Anthropic signature is dropped on the way to Responses, text
// blocks are untouched, and the caller's history is never mutated (a filter on the
// way out must not rewrite the stored conversation).
func TestThinkingCompatStripsBothDirections(t *testing.T) {
	c := assert.NewCollecting(t)

	const (
		anthropicSig = "EqQBCgIYAhIM-sig-from-claude"
		responsesSig = "openai-responses:rs_abc:gAAAA-blob"
	)
	mkMessages := func() []anthropic.MessageParam {
		return []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "question"}},
			}},
			{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
				{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "claude thought", Signature: anthropicSig}},
				{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "openai thought", Signature: responsesSig}},
				{OfText: &anthropic.TextBlockParam{Text: "answer"}},
			}},
		}
	}

	msgs := mkMessages()

	// Toward Responses: only the Responses signature survives.
	toResponses := llm.StripForeignThinking(msgs, true)
	c.Require().Len(toResponses, 2, "messages")
	c.Require().Len(toResponses[1].Content, 2, "assistant blocks toward Responses")
	c.Eq(responsesSig, toResponses[1].Content[0].OfThinking.Signature, "kept signature")
	c.Eq("answer", toResponses[1].Content[1].OfText.Text, "text block")

	// Toward Anthropic: only the Anthropic signature survives.
	toAnthropic := llm.StripForeignThinking(msgs, false)
	c.Require().Len(toAnthropic, 2, "messages")
	c.Require().Len(toAnthropic[1].Content, 2, "assistant blocks toward Anthropic")
	c.Eq(anthropicSig, toAnthropic[1].Content[0].OfThinking.Signature, "kept signature")
	c.Eq("answer", toAnthropic[1].Content[1].OfText.Text, "text block")

	// The caller's slice and its blocks are untouched.
	c.Require().Len(msgs[1].Content, 3, "caller's assistant blocks")
	c.Eq(anthropicSig, msgs[1].Content[0].OfThinking.Signature, "caller block 0")
	c.Eq(responsesSig, msgs[1].Content[1].OfThinking.Signature, "caller block 1")

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

// TestThinkingCompatSignatureShapes pins the signature format both senders agree
// on: "<prefix><item id>:<blob>", with a blob that may be empty and an id that may
// not.
func TestThinkingCompatSignatureShapes(t *testing.T) {
	// IsResponsesSignature is a prefix test and ParseResponsesSignature a shape
	// test: a Responses-PREFIXED signature that does not parse still belongs to
	// this family (so the other direction must drop it), and the request builder
	// is what refuses to resend it.
	cases := []struct {
		name       string
		sig        string
		wantOK     bool
		wantPrefix bool
		wantID     string
		wantBlob   string
	}{
		{"well formed", "openai-responses:rs_1:gAAAA-blob", true, true, "rs_1", "gAAAA-blob"},
		{"empty blob", "openai-responses:rs_1:", true, true, "rs_1", ""},
		{"blob keeps its colons", "openai-responses:rs_1:a:b", true, true, "rs_1", "a:b"},
		{"no prefix", "EqQBCgIYAhIM", false, false, "", ""},
		{"prefix only", "openai-responses:", false, true, "", ""},
		{"no separator", "openai-responses:rs_1", false, true, "", ""},
		{"empty id", "openai-responses::blob", false, true, "", ""},
		{"prefix of another name", "openai-lite:rs_1:blob", false, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			id, blob, ok := llm.ParseResponsesSignature(tc.sig)
			c.Eq(tc.wantOK, ok, "ok")
			c.Eq(tc.wantID, id, "id")
			c.Eq(tc.wantBlob, blob, "blob")
			c.Eq(tc.wantPrefix, llm.IsResponsesSignature(tc.sig), "IsResponsesSignature")
		})
	}
	c := assert.NewCollecting(t)
	c.Eq("openai-responses:", llm.ResponsesSignaturePrefix, "prefix")
}
