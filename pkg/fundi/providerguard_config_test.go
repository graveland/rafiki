// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/llm/llmtest"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// recordingSender answers every call with a one-word reply and keeps the
// request and encode options it was handed, which carry the caller's extras
// ("provider").
type recordingSender struct {
	reqs []*msg.Request
	opts []msg.EncodeOptions
}

func (s *recordingSender) Send(_ context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	s.reqs = append(s.reqs, req)
	s.opts = append(s.opts, opts)
	return &msg.Response{
		StopReason: msg.StopEndTurn,
		Content:    []msg.Block{msg.Text{Text: "ok"}},
	}, nil
}

func (s *recordingSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := s.Send(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	return llmtest.NewDecoder(llmtest.Events(resp)), nil
}

// TestProviderGuardConfigReachesTheWire pins Config.ProviderGuard's last hop:
// an operator ban on the daemon's guard lands in the provider.ignore of a
// request built from the child's clientOptions. Without it an in-process
// child calls OpenRouter with no ignore list, and a sort=price route hands it
// straight to the cheapest provider — banned or not.
func TestProviderGuardConfigReachesTheWire(t *testing.T) {
	ck := assert.NewAborting(t)
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, slog.New(slog.DiscardHandler))
	_, err := g.Ban(context.Background(), time.Now(), "open-inference", 0, "")
	ck.NoError(err, "Ban")

	openrouter := &recordingSender{}
	// A seeded catalog never refreshes, so resolving the slash model does not
	// reach the network.
	catalog := routing.NewModelCatalog(nil, time.Hour, nil)
	catalog.SeedForTest([]routing.CatalogEntry{{ID: "z-ai/glm-5.3-flash"}})
	cfg := Config{
		Model:           "openrouter/z-ai/glm-5.3-flash",
		Tools:           fakeToolSet{},
		Providers:       providers.Default(),
		Routing:         "sort=price",
		Catalog:         catalog,
		ProviderGuard:   g,
		ProviderSenders: map[string]llm.Sender{"openrouter": openrouter},
	}
	opts, err := cfg.clientOptions()
	ck.NoError(err, "clientOptions")
	client, err := llm.NewClient(opts...)
	ck.NoError(err, "NewClient")

	conv, err := client.Conversation(context.Background(), llm.NewConversation("", "test"),
		llm.Model("openrouter/z-ai/glm-5.3-flash"), llm.MaxTokens(16))
	ck.NoError(err, "Conversation")
	_, err = conv.Send(context.Background(), llm.UserText("hi"))
	ck.NoError(err, "Send")
	ck.Require().Eq(1, len(openrouter.reqs), "requests sent to openrouter")

	body, err := msganthropic.Encode(openrouter.reqs[0], openrouter.opts[0])
	ck.NoError(err, "encode wire body")
	ck.StrContains(string(body), `"ignore":["open-inference"]`, "operator ban missing from the child's wire body")
}
