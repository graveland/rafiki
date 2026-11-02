// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// TestKindVendor pins the kind→vendor mapping "<tier>-latest" resolution keys on.
// A kind serving arbitrary models reports no vendor, so nothing tries to resolve
// an OpenAI-shaped tier name against a generic gateway.
func TestKindVendor(t *testing.T) {
	ck := assert.NewAborting(t)
	for _, tc := range []struct {
		kind providers.Kind
		want string
	}{
		{providers.KindAnthropic, "anthropic"},
		{providers.KindOpenAIResponses, "openai"},
		{providers.KindOpenAIResponsesChatGPT, "openai"},
		{providers.KindOpenAI, ""},
		{providers.KindAnthropicOpenRouter, ""},
		{providers.Kind("nope"), ""},
	} {
		ck.Eq(tc.want, tc.kind.Vendor(), "Kind(%q).Vendor()", string(tc.kind))
	}
}
