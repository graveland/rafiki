// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

func TestSdkSenderImplementsStreamingSender(t *testing.T) {
	c := assert.NewAborting(t)
	s1, err := llm.SenderFor(providers.Provider{Name: "a", Kind: providers.KindAnthropic}, nil)
	c.NoError(err, "SenderFor(anthropic)")
	if _, ok := s1.(llm.StreamingSender); !ok {
		t.Fatal("SenderFor(anthropic) must also satisfy StreamingSender")
	}
	s2, err := llm.SenderFor(providers.Provider{Name: "a", Kind: providers.KindAnthropicOpenRouter}, nil)
	c.NoError(err, "SenderFor(openrouter)")
	_, ok := s2.(llm.StreamingSender)
	c.True(ok, "SenderFor(openrouter) must also satisfy StreamingSender")
}

// A Sender that does NOT stream must still satisfy Sender, so existing fakes
// and the non-streaming path keep working. The compile-time assertion below
// is the entire guarantee: if NewStreaming were ever folded into Sender
// itself (making it non-optional), this line — not a runtime check — would
// fail to build. nonStreamingSender deliberately has no NewStreaming method,
// so it can never satisfy StreamingSender by construction; a runtime assertion
// to that effect would prove nothing a test function could meaningfully fail.
type nonStreamingSender struct{}

func (nonStreamingSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, nil
}

var _ llm.Sender = nonStreamingSender{}

// TestSenderForKeyChatGPTKindRefused pins the owner-bound constructor as the only
// way to build the ChatGPT dialect: a key cannot stand in for an owner's login, and
// a sender built from one would send daemon-wide credentials on a user's behalf.
func TestSenderForKeyChatGPTKindRefused(t *testing.T) {
	c := assert.NewAborting(t)
	p := providers.Provider{Name: "chatgpt", Kind: providers.KindOpenAIResponsesChatGPT}
	s, err := llm.SenderForKey(p, "sk-not-a-login", nil)
	c.Error(err, "SenderForKey(openai-responses-chatgpt) must refuse a key")
	c.Nil(s, "SenderForKey(openai-responses-chatgpt) returned a sender")
	c.StrContains(err.Error(), "NewChatGPTResponsesSender", "error names the constructor to use")
	c.StrContains(err.Error(), "cannot be built from a key", "error")
}

// TestSenderForKeyBuildsResponsesSender pins that the api-key dialect is an
// ordinary SenderForKey case, and that a keyless entry builds without one.
func TestSenderForKeyBuildsResponsesSender(t *testing.T) {
	c := assert.NewAborting(t)
	for _, key := range []string{"sk-test", ""} {
		s, err := llm.SenderForKey(providers.Provider{Name: "oa", Kind: providers.KindOpenAIResponses}, key, nil)
		c.NoError(err, "SenderForKey(openai-responses, key=%q)", key)
		if _, ok := s.(llm.StreamingSender); !ok {
			t.Fatalf("SenderForKey(openai-responses, key=%q) must also satisfy StreamingSender", key)
		}
	}
}
