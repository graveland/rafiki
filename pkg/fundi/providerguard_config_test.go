// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// recordingSender answers every call with a one-word reply and keeps the
// params it was handed, which carry the SDK's ExtraFields ("provider").
type recordingSender struct{ reqs []anthropic.MessageNewParams }

func (s *recordingSender) New(_ context.Context, p anthropic.MessageNewParams) (*anthropic.Message, error) {
	s.reqs = append(s.reqs, p)
	return &anthropic.Message{
		Role:       "assistant",
		StopReason: anthropic.StopReasonEndTurn,
		Content:    []anthropic.ContentBlockUnion{{Type: "text", Text: "ok"}},
	}, nil
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
	cfg := Config{
		Model:           "openrouter/z-ai/glm-5.3-flash",
		Tools:           fakeToolSet{},
		Providers:       providers.Default(),
		Routing:         "sort=price",
		Catalog:         routing.NewModelCatalog(nil, time.Hour, nil),
		ProviderGuard:   g,
		ProviderSenders: map[string]llm.Sender{"openrouter": openrouter},
	}
	opts, err := cfg.clientOptions()
	ck.NoError(err, "clientOptions")
	client, err := llm.NewClient(opts...)
	ck.NoError(err, "NewClient")

	params := anthropic.MessageNewParams{Model: "openrouter/z-ai/glm-5.3-flash", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	_, err = client.SendParams(context.Background(), llm.SendMeta{}, params)
	ck.NoError(err, "SendParams")
	ck.Require().Eq(1, len(openrouter.reqs), "requests sent to openrouter")

	body, err := json.Marshal(openrouter.reqs[0])
	ck.NoError(err, "marshal wire params")
	ck.StrContains(string(body), `"ignore":["open-inference"]`, "operator ban missing from the child's wire body")
}
