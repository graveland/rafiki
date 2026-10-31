// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/llm"

	"github.com/multigres/testkit/assert"
)

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
