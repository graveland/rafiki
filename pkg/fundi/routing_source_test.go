// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// TestRoutingSourceTakesPrecedenceOverStaticRouting pins the config→client
// hop: a non-nil Config.RoutingSource is handed to the llm.Client via
// llm.WithRoutingSource and OVERRIDES the static Routing string, which is then
// not even parsed (so an unparseable static spec cannot fail a normally
// steered child). With no source, the same unparseable static spec still fails
// loudly, exactly as before.
func TestRoutingSourceTakesPrecedenceOverStaticRouting(t *testing.T) {
	ck := assert.NewAborting(t)
	cfg := Config{
		Model:         "openrouter/z-ai/glm-5.3-flash",
		Tools:         fakeToolSet{},
		Providers:     providers.Default(),
		Routing:       "sort=bogus",
		RoutingSource: func() routing.Spec { return routing.Spec{Sort: routing.SortPrice} },
	}
	_, err := cfg.clientOptions()
	ck.NoError(err, "a non-nil RoutingSource must override the static Routing string")

	cfg.RoutingSource = nil
	_, err = cfg.clientOptions()
	ck.Require().Error(err, "with no source the unparseable static Routing must still fail")
	ck.StrContains(err.Error(), "sort=bogus", "the error must name the offending static spec")
}
