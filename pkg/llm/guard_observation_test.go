// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// guardModel is the provider-local model id every guard test routes.
const guardModel = "z-ai/glm-5.3-flash"

// guardSet is the one-provider registry the guard tests use: an
// anthropic-openrouter entry, because the provider cache guard is only
// consulted for that kind.
func guardSet() *providers.Set {
	return &providers.Set{
		DefaultProvider: "openrouter",
		Providers: map[string]providers.Provider{
			"openrouter": {Name: "openrouter", Kind: providers.KindAnthropicOpenRouter},
		},
	}
}

// nativeFakeSender answers every send with resp, and every stream with the
// recorded SSE bytes — the real Anthropic codec decodes them, so a provider
// field in message_start travels exactly the path production's does.
type nativeFakeSender struct {
	resp *msg.Response
	sse  []byte
}

func (s nativeFakeSender) Send(context.Context, *msg.Request, msg.EncodeOptions) (*msg.Response, error) {
	return s.resp, nil
}

func (s nativeFakeSender) Stream(context.Context, *msg.Request, msg.EncodeOptions) (msg.StreamDecoder, error) {
	return msganthropic.NewStreamDecoder(io.NopCloser(bytes.NewReader(s.sse))), nil
}

// guardTestConversation builds a captured conversation over the one-provider
// registry, so beginTurn supplies the conversation id and prefix hash the
// guard's qualification rules require.
func guardTestConversation(t *testing.T, pool *pgxpool.Pool, g *routing.ProviderGuard, sender Sender) *Conversation {
	t.Helper()
	ck := assert.NewAborting(t)
	c, err := NewClient(
		WithProviders(guardSet()),
		WithProviderSender("openrouter", sender),
		WithStore(pool),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithProviderGuard(g),
	)
	ck.NoError(err, "NewClient")
	conv, err := c.Conversation(context.Background(), NewConversation("", "test"),
		Model("openrouter/"+guardModel), SystemText("sys"))
	ck.NoError(err, "Conversation")
	return conv
}

// cacheMissResponse is one completed turn reporting a large prompt and NO
// prompt-cache hits, from provider. cacheRead nil models a provider that
// reported no cache-read count at all; a pointer to 0 models one that reported
// a genuine miss.
func cacheMissResponse(provider string, cacheRead *int64) *msg.Response {
	return &msg.Response{
		ID: "msg_g", Model: guardModel, Provider: provider,
		StopReason: msg.StopEndTurn,
		Usage:      msg.Usage{Input: msg.Ptr(5000), Output: msg.Ptr(5), CacheRead: cacheRead},
		Content:    []msg.Block{msg.Text{Text: "ok"}},
	}
}

// cacheMissSSE is the streaming form of cacheMissResponse: an OpenRouter
// Messages-API stream whose message_start names the backend that served it.
const cacheMissSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_g","type":"message","role":"assistant","model":"z-ai/glm-5.3-flash","content":[],"usage":{"input_tokens":5000,"output_tokens":0,"cache_read_input_tokens":0},"provider":"Together"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

// runTurns sends n turns through conv, streaming when stream is set.
func runTurns(t *testing.T, conv *Conversation, n int, stream bool) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		var err error
		if stream {
			_, err = conv.Send(ctx, UserText("hi"), WithStreamHandler(func(*msg.Response) {}))
		} else {
			_, err = conv.Send(ctx, UserText("hi"))
		}
		assert.NewAborting(t).NoError(err, "turn %d", i+1)
	}
}

// TestGuardObservesAbsentCacheReadAsZero pins the guard's evidence rule: an
// OpenRouter turn that reports its input tokens but no cache-read figure is a
// cache miss and reaches the guard as CacheReadTokens 0 — five such turns eject
// the provider exactly as a reported zero does. A turn reporting no input count
// at all is no evidence and observes nothing.
func TestGuardObservesAbsentCacheReadAsZero(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := convTestPool(t)

	g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
	conv := guardTestConversation(t, pool, g, nativeFakeSender{resp: cacheMissResponse("Together", nil)})
	runTurns(t, conv, 8, false)
	c.EqDeep([]string{"together"}, g.IgnoredFor(time.Now(), guardModel),
		"an absent cache_read count must be observed as a miss")

	g2 := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
	conv2 := guardTestConversation(t, pool, g2, nativeFakeSender{resp: cacheMissResponse("Together", msg.Ptr(0))})
	runTurns(t, conv2, 8, false)
	c.EqDeep([]string{"together"}, g2.IgnoredFor(time.Now(), guardModel),
		"a reported zero cache read must reach the guard as a miss")

	// Neither figure reported: no evidence, however many turns.
	noUsage := cacheMissResponse("Together", nil)
	noUsage.Usage.Input = nil
	g3 := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
	conv3 := guardTestConversation(t, pool, g3, nativeFakeSender{resp: noUsage})
	runTurns(t, conv3, 8, false)
	c.Nil(g3.IgnoredFor(time.Now(), guardModel), "a turn with no input count was treated as evidence")
}

// TestStreamingProviderAttributionReachesGuard pins that the backend a response
// NAMES reaches the shared guard — not the provider config's name — on both the
// non-streaming and the streaming path (an OpenRouter SSE fixture carries it in
// message_start), and that a response naming no backend stays unattributed
// rather than being attributed to whoever served it.
func TestStreamingProviderAttributionReachesGuard(t *testing.T) {
	pool := convTestPool(t)

	t.Run("non-streaming response", func(t *testing.T) {
		c := assert.NewCollecting(t)
		g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
		conv := guardTestConversation(t, pool, g, nativeFakeSender{resp: cacheMissResponse("Together", msg.Ptr(0))})
		runTurns(t, conv, 8, false)
		c.EqDeep([]string{"together"}, g.IgnoredFor(time.Now(), guardModel),
			"the guard must eject the backend the response named")
	})

	t.Run("openrouter SSE fixture", func(t *testing.T) {
		c := assert.NewCollecting(t)
		g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
		conv := guardTestConversation(t, pool, g, nativeFakeSender{sse: []byte(cacheMissSSE)})
		runTurns(t, conv, 8, true)
		c.EqDeep([]string{"together"}, g.IgnoredFor(time.Now(), guardModel),
			"the provider reported in message_start must reach the guard on the streaming path")
	})

	t.Run("absent provider stays unattributed", func(t *testing.T) {
		c := assert.NewCollecting(t)
		g := routing.NewProviderGuard(routing.DefaultEjectTTL, testLogger(t))
		conv := guardTestConversation(t, pool, g, nativeFakeSender{resp: cacheMissResponse("", msg.Ptr(0))})
		runTurns(t, conv, 8, false)
		c.Nil(g.IgnoredFor(time.Now(), guardModel),
			"a response naming no backend must not be attributed to the provider config")
	})
}
