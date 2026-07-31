// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

func TestSdkSenderImplementsStreamingSender(t *testing.T) {
	if _, ok := llm.Anthropic("test-key").(llm.StreamingSender); !ok {
		t.Fatal("llm.Anthropic's Sender must also satisfy StreamingSender")
	}
	if _, ok := llm.OpenRouter("test-key").(llm.StreamingSender); !ok {
		t.Fatal("llm.OpenRouter's Sender must also satisfy StreamingSender")
	}
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
