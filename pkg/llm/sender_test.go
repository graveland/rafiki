// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// sendParams issues one non-streamed call through a native Sender, converting
// the SDK params these tests speak. It is the same conversion the client does.
func sendParams(t *testing.T, s llm.Sender, ctx context.Context, params anthropic.MessageNewParams) (*msg.Response, error) {
	t.Helper()
	req, extra, err := msganthropic.RequestFromSDK(params)
	if err != nil {
		return nil, err
	}
	return s.Send(ctx, req, msg.EncodeOptions{Extra: extra})
}

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
		s, err := llm.SenderForKey(p, "k", nil)
		c.Require().NoError(err, "SenderForKey(%s)", p.Kind)
		c.NotNil(s, "SenderForKey(%s)", p.Kind)
	}
}

// An SDK-typed sender that cannot stream still satisfies the native Sender
// through the compat adapter: Stream falls back to Send and replays the
// response as events. The compile-time assertion is the guarantee.
type nonStreamingSender struct{}

func (nonStreamingSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, nil
}

var _ llm.SDKSender = nonStreamingSender{}

func TestFromSDKSenderAcceptsANonStreamingFake(t *testing.T) {
	c := assert.NewAborting(t)
	s := llm.FromSDKSender(nonStreamingSender{})
	c.NotNil(s, "FromSDKSender(nonStreamingSender)")
}

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
	// The refusal is a distinguishable fact, not an anonymous failure: the
	// client logs this one quietly and keeps WARN for real breakage.
	c.ErrorIs(err, llm.ErrNeedsOwnerSender, "SenderForKey(openai-responses-chatgpt) must carry ErrNeedsOwnerSender")
	c.StrContains(err.Error(), "chatgpt", "error names the provider")
}

// TestSenderForKeyBuildsResponsesSender pins that the api-key dialect is an
// ordinary SenderForKey case, and that a keyless entry builds without one.
func TestSenderForKeyBuildsResponsesSender(t *testing.T) {
	c := assert.NewAborting(t)
	for _, key := range []string{"sk-test", ""} {
		s, err := llm.SenderForKey(providers.Provider{Name: "oa", Kind: providers.KindOpenAIResponses}, key, nil)
		c.NoError(err, "SenderForKey(openai-responses, key=%q)", key)
		c.NotNil(s, "SenderForKey(openai-responses, key=%q)", key)
	}
}

// TestFromSDKSenderMapsAnthropicError pins the compat adapter's error mapping:
// an SDK error carrying a 429 and a Retry-After becomes the native APIError the
// classifiers read, with both the kind and the delay intact.
func TestFromSDKSenderMapsAnthropicError(t *testing.T) {
	c := assert.NewAborting(t)
	req, rerr := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	c.NoError(rerr, "build the request the SDK error carries")
	sdkErr := &anthropic.Error{
		StatusCode: http.StatusTooManyRequests,
		Request:    req,
		Response: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"7"}},
		},
	}
	c.NoError(sdkErr.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)), "seed the SDK error")

	s := llm.FromSDKSender(errorSender{err: sdkErr})
	_, err := sendParams(t, s, context.Background(), externalMinimalParams())
	c.Require().Error(err, "Send must fail")

	var apiErr *msg.APIError
	c.Require().True(errors.As(err, &apiErr), "error %T is not a *msg.APIError", err)
	c.Eq(http.StatusTooManyRequests, apiErr.Status)
	c.Eq(msg.ErrRateLimit, apiErr.Kind, "kind")
	c.Eq(7*time.Second, apiErr.RetryAfter, "Retry-After")
	c.Eq(msg.DialectAnthropic, apiErr.Dialect, "dialect")
	c.Eq("slow down", apiErr.Message, "message")
}

// externalMinimalParams is the smallest params the SDK requires, for the
// external test package.
func externalMinimalParams() anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		MaxTokens: 16,
		Model:     "claude-haiku-4-5",
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "hi"}},
			}},
		},
	}
}

// errorSender is an SDK-typed fake that always fails.
type errorSender struct{ err error }

func (s errorSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, s.err
}
