// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// TestPreferReachesTheWireProviderObject pins that a spec's prefer list lands on
// the request body as provider.order: the spec branch of applyProviderPrefs
// flows Spec.Prefer through ProviderPrefs.Order, and the SDK marshals it under
// "provider". A nil *routing.ProviderGuard is legal — IgnoredFor is nil-safe.
func TestPreferReachesTheWireProviderObject(t *testing.T) {
	ck := assert.NewAborting(t)
	params := anthropic.MessageNewParams{Model: "deepseek/deepseek-v4.1-flash"}
	applyProviderPrefs(&params, nil, nil, nil, routing.Spec{Prefer: []string{"fireworks"}})
	b, err := json.Marshal(params)
	ck.NoError(err)
	ck.StrContains(string(b), `"provider":{"order":["fireworks"]}`,
		"prefer must ride the provider object as order, body =")
}
