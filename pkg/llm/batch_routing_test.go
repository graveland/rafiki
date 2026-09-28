// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// routingFakeBatcher records Park's provider argument — the wire-side fact
// this file pins — and returns a canned batch reply. (batch_test.go's
// fakeBatcher predates the provider argument; this one records it.)
type routingFakeBatcher struct {
	mu        sync.Mutex
	providers []json.RawMessage
	models    []string // Park's model argument — what the batch envelope would name
}

func (b *routingFakeBatcher) Park(_ context.Context, _ string, model string, _ anthropic.MessageNewParams, provider json.RawMessage) (*anthropic.Message, error) {
	b.mu.Lock()
	b.providers = append(b.providers, provider)
	b.models = append(b.models, model)
	b.mu.Unlock()
	return batchReply(), nil
}

// routingOnlySet registers the alias "glm-flash" -> z-ai/glm-5.3-flash, with
// only as the alias's Only pin (nil = none). A batch request must name the
// ALIAS (openrouter/glm-flash:batch): Set.Resolve matches Models by the
// requested id, and the substituted base id carries no Only of its own.
func routingOnlySet(only []string) *providers.Set {
	alias := providers.ModelAlias{ID: "z-ai/glm-5.3-flash"}
	if only != nil {
		alias.Only = only
	}
	return &providers.Set{
		DefaultProvider: "openrouter",
		Providers: map[string]providers.Provider{
			"openrouter": {
				Name:   "openrouter",
				Kind:   providers.KindAnthropicOpenRouter,
				Models: map[string]providers.ModelAlias{"glm-flash": alias},
			},
		},
	}
}

// newRoutingBatchClient builds a store-less park-path client over the given
// provider set and routing spec.
func newRoutingBatchClient(t *testing.T, set *providers.Set, spec routing.Spec) *Client {
	t.Helper()
	c, err := NewClient(
		WithProviders(set),
		WithProviderSender("openrouter", &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
			respondText("must not be reached"),
		}}),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithBatcher(&routingFakeBatcher{}),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithRouting(spec),
	)
	assert.NewAborting(t).NoError(err, "NewClient")
	return c
}

// parkedProviders runs one park and returns the provider JSON the batcher
// received (nil when Park got no provider argument).
func parkedProviders(t *testing.T, c *Client) json.RawMessage {
	t.Helper()
	b := c.batcher.(*routingFakeBatcher)
	_, err := c.SendParams(context.Background(), SendMeta{ConversationID: "01a0d5e6-2636-7afa-b356-cf9441b16e31", Ordinal: 1}, batchParams())
	assert.NewAborting(t).NoError(err, "SendParams (park)")
	assert.NewAborting(t).Eq(1, len(b.providers), "batcher calls")
	return b.providers[0]
}

// TestBatchRoutingOnParkedRequest pins the parked call's wire: the batch
// envelope's provider object carries ONLY {"only": [...]} — the spec's Only
// when set (beating the alias pin and the static pin), else the SAME pinOnly
// rule applyProviderPrefs computes (alias Only > static pin) — and is absent
// entirely when nothing pins the model. Never the full prefs object: sort,
// quantizations, data_collection, zdr and ignore are submit-time 400s on the
// Batch API (Task 0.1's probe).
func TestBatchRoutingOnParkedRequest(t *testing.T) {
	decodeOnly := func(t *testing.T, raw json.RawMessage) []string {
		t.Helper()
		if len(raw) == 0 {
			return nil
		}
		var obj struct {
			Only []string `json:"only"`
		}
		assert.NewAborting(t).NoError(json.Unmarshal(raw, &obj), "decode provider object")
		var full map[string]json.RawMessage
		assert.NewAborting(t).NoError(json.Unmarshal(raw, &full), "decode provider object keys")
		assert.NewAborting(t).Eq(1, len(full), "provider object keys = %v, want only \"only\"", full)
		return obj.Only
	}

	t.Run("spec only wins over alias and pin", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newRoutingBatchClient(t, routingOnlySet([]string{"alias-host"}),
			routing.Spec{Only: []string{"spec-host"}})
		ck.Eq("[spec-host]", fmt.Sprint(decodeOnly(t, parkedProviders(t, c))), "spec Only")
	})

	t.Run("alias only when the spec carries none", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newRoutingBatchClient(t, routingOnlySet([]string{"alias-host"}),
			routing.Spec{Sort: routing.SortPrice})
		// Name the ALIAS with the suffix: Set.Resolve's alias lookup sees the
		// stripped id (prepareSend resolves :batch ids through the base), so
		// the alias's Only pin reaches the parked envelope.
		b := c.batcher.(*routingFakeBatcher)
		aliasParams := batchParams()
		aliasParams.Model = "openrouter/glm-flash:batch"
		_, err := c.SendParams(context.Background(), SendMeta{ConversationID: "01a0d5e6-2636-7afa-b356-cf9441b16e31", Ordinal: 1}, aliasParams)
		ck.Require().NoError(err, "SendParams (park, alias-named)")
		ck.Require().Eq(1, len(b.providers), "batcher calls")
		ck.Eq("[alias-host]", fmt.Sprint(decodeOnly(t, b.providers[0])), "alias Only")
	})

	t.Run("static pin when no spec and no alias only", func(t *testing.T) {
		ck := assert.NewAborting(t)
		set := routingOnlySet(nil)
		set.Providers["openrouter"].Models["glm52"] = providers.ModelAlias{ID: "z-ai/glm-5.2"}
		c := newRoutingBatchClient(t, set, routing.Spec{})
		params := batchParams()
		params.Model = "openrouter/glm52:batch" // providerPins: fireworks
		b := c.batcher.(*routingFakeBatcher)
		_, err := c.SendParams(context.Background(), SendMeta{ConversationID: "01a0d5e6-2636-7afa-b356-cf9441b16e31", Ordinal: 1}, params)
		ck.Require().NoError(err, "SendParams (park)")
		ck.Require().Eq(1, len(b.providers), "batcher calls")
		ck.Eq("[fireworks]", fmt.Sprint(decodeOnly(t, b.providers[0])), "static pin Only")
	})

	t.Run("no pin at all sends no provider key", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newRoutingBatchClient(t, routingOnlySet(nil), routing.Spec{})
		ck.Nil(parkedProviders(t, c), "provider argument must be absent, not an empty object")
	})
}

// TestBatchAliasToBatchIDParksFirstCallOnly pins fix-6's regression: an alias
// whose ID ends in :batch is a supported shape, and it must behave exactly
// like the literal :batch request — the FIRST call parks (params.Model keeps
// the suffix), a LATER call goes live with the suffix stripped. The old code
// tested only the pre-alias string, so batched stayed false, the strip never
// ran, and every turn parked.
func TestBatchAliasToBatchIDParksFirstCallOnly(t *testing.T) {
	ck := assert.NewCollecting(t)
	set := routingOnlySet(nil)
	set.Providers["openrouter"].Models["glmb"] = providers.ModelAlias{ID: "z-ai/glm-5.3-flash:batch"}
	b := &routingFakeBatcher{}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("live"),
	}}
	c, err := NewClient(
		WithProviders(set),
		WithProviderSender("openrouter", sender),
		WithProviderSender("anthropic", sender),
		WithBatcher(b),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err, "NewClient")
	meta := SendMeta{ConversationID: "01a0d5e6-2636-7afa-b356-cf9441b16e31", Ordinal: 1}

	// First call: parks, model keeps :batch — exactly ONE suffix. The request
	// names the ALIAS whose id already ends in :batch; a doubled suffix would
	// name a model no endpoint serves (review-fix R1).
	first := batchParams()
	first.Model = "openrouter/glmb"
	resp, err := c.SendParams(context.Background(), meta, first)
	ck.Require().NoError(err, "first SendParams (alias to :batch) must park")
	ck.Eq("batched", resp.Content[0].Text, "first call response from the batcher")
	ck.Require().Eq(1, len(b.providers), "batcher calls after the first send")
	ck.Eq("z-ai/glm-5.3-flash:batch", b.models[0], "the parked call's model carries exactly one :batch")

	// Later call: goes live, suffix stripped.
	later := laterCallParams()
	later.Model = "openrouter/glmb"
	resp, err = c.SendParams(context.Background(), meta, later)
	ck.Require().NoError(err, "later SendParams (alias to :batch) must go live")
	ck.Eq("live", resp.Content[0].Text, "later call response from the live sender")
	ck.Require().Eq(1, sender.calls, "live sender calls")
	ck.Eq("z-ai/glm-5.3-flash", string(sender.lastReq[0].Model), "the :batch suffix must be stripped on the live call")
	ck.Eq(1, len(b.providers), "the later call must not park again")
}
