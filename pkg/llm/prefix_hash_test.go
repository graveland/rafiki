// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

// prefixRequest is a request with every hashed field populated, so a test can
// change exactly one of them.
func prefixRequest() *msg.Request {
	temp := 0.4
	var k int64 = 20
	return &msg.Request{
		Model:          "z-ai/glm-5.3-flash",
		System:         []msg.Text{{Text: "you are a test"}},
		Messages:       []msg.Message{{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hi"}}}},
		Tools:          []msg.ToolDef{{Name: "bash", Description: "run", InputSchema: []byte(`{"type":"object"}`)}},
		ForceTool:      "bash",
		MaxTokens:      16,
		Temperature:    &temp,
		TopK:           &k,
		ThinkingBudget: 1000,
		Cache:          CachePolicy{SystemTTL: Cache5m, MessagesTTL: Cache5m, Breakpoints: 2},
	}
}

// TestNativePrefixHashIgnoresMessagesAndExtra pins what the hash is FOR: two
// turns of one conversation share a prefix hash exactly when the provider's
// cacheable prefix is unchanged, and the messages are the part that grows every
// turn. A caller's top-level extra fields are not part of msg.Request at all —
// they ride EncodeOptions and are merged into the encoded body afterwards — so
// they cannot move the hash either.
func TestNativePrefixHashIgnoresMessagesAndExtra(t *testing.T) {
	c := assert.NewCollecting(t)
	base := prefixRequest()
	hash := nativePrefixHash(base)
	c.Require().NotEmpty(hash, "prefix hash")

	grown := prefixRequest()
	grown.Messages = append(grown.Messages,
		msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "reply"}}},
		msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "next"}}})
	c.Eq(hash, nativePrefixHash(grown), "the prefix hash changed with the message history")

	// Extras are not a msg.Request field at all: they ride msg.EncodeOptions and
	// are merged into the encoded body afterwards. Encode the same request with
	// and without them — proving the extras really reach the wire body, so this
	// half is not a no-op — and check the request, and so the hash, is untouched.
	extra := map[string]any{
		"provider":  map[string]any{"only": []string{"together"}},
		"reasoning": map[string]any{"effort": "high"},
	}
	plain, err := msganthropic.Encode(prefixRequest(), msg.EncodeOptions{})
	c.Require().NoError(err, "encode without extras")
	withExtras, err := msganthropic.Encode(base, msg.EncodeOptions{Extra: extra})
	c.Require().NoError(err, "encode with extras")
	c.NotEq(string(plain), string(withExtras), "the fixture's extras never reached the encoded body")
	c.StrContains(string(withExtras), `"reasoning"`, "a non-provider extra must reach the encoded body")
	c.StrContains(string(withExtras), `"provider"`, "the provider extra must reach the encoded body")
	c.Eq(hash, nativePrefixHash(base), "the prefix hash changed after an extra-field build")
}

// TestNativePrefixHashChangesWithSystem pins the other half: the system prompt
// is part of the cached prefix, so changing it must change the hash — otherwise
// the guard would read a real cache miss as a provider fault.
func TestNativePrefixHashChangesWithSystem(t *testing.T) {
	c := assert.NewCollecting(t)
	base := prefixRequest()
	hash := nativePrefixHash(base)

	other := prefixRequest()
	other.System = []msg.Text{{Text: "you are something else"}}
	c.NotEq(hash, nativePrefixHash(other), "the prefix hash ignored a system-prompt change")

	// A second system block is a different prefix too.
	longer := prefixRequest()
	longer.System = append(longer.System, msg.Text{Text: "second block"})
	c.NotEq(hash, nativePrefixHash(longer), "the prefix hash ignored an added system block")

	// And a model change, which is the most obvious prefix change.
	model := prefixRequest()
	model.Model = "z-ai/glm-5.2"
	c.NotEq(hash, nativePrefixHash(model), "the prefix hash ignored a model change")
}
