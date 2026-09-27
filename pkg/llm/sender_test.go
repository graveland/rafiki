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
