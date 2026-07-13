package llm

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Sender issues one Messages-API call. The SDK client is wrapped behind this
// seam so tests inject fakes and the client stays provider-agnostic.
type Sender interface {
	New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error)
}

type sdkSender struct{ client anthropic.Client }

func (s sdkSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	return s.client.Messages.New(ctx, params)
}

// Anthropic returns a Sender for the Anthropic API.
func Anthropic(apiKey string) Sender {
	return sdkSender{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

// OpenRouter returns a Sender for OpenRouter's Anthropic-compatible endpoint.
// Model ids are translated at failover time via the model catalog; a Sender
// itself is id-agnostic.
func OpenRouter(apiKey string) Sender {
	return sdkSender{client: anthropic.NewClient(
		option.WithBaseURL(openRouterBaseURL),
		option.WithAPIKey(apiKey),
	)}
}

// FromSDK wraps a pre-built SDK client as a Sender — e.g. sc's forward-mode
// diagnose client pointed at the central /v1/messages proxy.
func FromSDK(client anthropic.Client) Sender {
	return sdkSender{client: client}
}
