// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// TestRoutingConfigParsesSpecForClientOptions pins Config.Routing's last hop
// before the wire: clientOptions parses the canonical spec (routing.
// ParseSpec) once and hands it to the engine's llm.Client as llm.WithRouting;
// a spec that no longer parses is a config validation error — BuildEngine
// refuses the child rather than starting it half-configured — and an empty
// Routing stays the pre-spec no-op.
func TestRoutingConfigParsesSpecForClientOptions(t *testing.T) {
	ck := assert.NewAborting(t)
	cfg := Config{
		Model:     "openrouter/z-ai/glm-5.3-flash",
		Tools:     fakeToolSet{},
		Providers: providers.Default(),
		Routing:   "sort=price,nodata",
	}
	_, err := cfg.clientOptions()
	ck.NoError(err, "clientOptions with a valid spec")

	cfg.Routing = "sort=bogus"
	_, err = cfg.clientOptions()
	ck.Require().Error(err, "clientOptions with an unparseable spec: want error, got nil")
	ck.StrContains(err.Error(), "sort=bogus", "the error must name the offending spec")

	cfg.Routing = ""
	_, err = cfg.clientOptions()
	ck.NoError(err, "clientOptions with no spec: the pre-spec no-op")
}
