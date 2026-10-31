// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// Every key-based kind is a native Sender, and streaming is part of the
// interface rather than an optional capability: a sender that could not stream
// would silently degrade every turn to the non-streamed path.
func TestEverySenderForKeyStreams(t *testing.T) {
	c := assert.NewAborting(t)
	for _, p := range []providers.Provider{
		{Name: "a", Kind: providers.KindAnthropic, BaseURL: "http://127.0.0.1:1"},
		{Name: "or", Kind: providers.KindAnthropicOpenRouter, BaseURL: "http://127.0.0.1:1"},
		{Name: "oa", Kind: providers.KindOpenAI, BaseURL: "http://127.0.0.1:1"},
		{Name: "resp", Kind: providers.KindOpenAIResponses, BaseURL: "http://127.0.0.1:1"},
	} {
		s, err := SenderForKey(p, "k", nil)
		c.Require().NoError(err, "SenderForKey(%s)", p.Kind)
		c.NotNil(s, "SenderForKey(%s)", p.Kind)
	}
}

// TestSenderForKeyChatGPTKindRefused pins the owner-bound constructor as the only
// way to build the ChatGPT dialect: a key cannot stand in for an owner's login, and
// a sender built from one would send daemon-wide credentials on a user's behalf.
func TestSenderForKeyChatGPTKindRefused(t *testing.T) {
	c := assert.NewAborting(t)
	p := providers.Provider{Name: "chatgpt", Kind: providers.KindOpenAIResponsesChatGPT}
	s, err := SenderForKey(p, "sk-not-a-login", nil)
	c.Error(err, "SenderForKey(openai-responses-chatgpt) must refuse a key")
	c.Nil(s, "SenderForKey(openai-responses-chatgpt) returned a sender")
	c.StrContains(err.Error(), "NewChatGPTResponsesSender", "error names the constructor to use")
	c.StrContains(err.Error(), "cannot be built from a key", "error")
	// The refusal is a distinguishable fact, not an anonymous failure: the
	// client logs this one quietly and keeps WARN for real breakage.
	c.ErrorIs(err, ErrNeedsOwnerSender, "SenderForKey(openai-responses-chatgpt) must carry ErrNeedsOwnerSender")
	c.StrContains(err.Error(), "chatgpt", "error names the provider")
}

// TestSenderForKeyBuildsResponsesSender pins that the api-key dialect is an
// ordinary SenderForKey case, and that a keyless entry builds without one.
func TestSenderForKeyBuildsResponsesSender(t *testing.T) {
	c := assert.NewAborting(t)
	for _, key := range []string{"sk-test", ""} {
		s, err := SenderForKey(providers.Provider{Name: "oa", Kind: providers.KindOpenAIResponses}, key, nil)
		c.NoError(err, "SenderForKey(openai-responses, key=%q)", key)
		c.NotNil(s, "SenderForKey(openai-responses, key=%q)", key)
	}
}
