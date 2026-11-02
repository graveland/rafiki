// SPDX-License-Identifier: Apache-2.0

package providers

// Vendor names the model vendor a provider of this kind serves natively, for
// resolving "<tier>-latest" names against the OpenRouter catalog: "anthropic",
// "openai", or "" for a kind that serves arbitrary models (a generic
// OpenAI-compatible server, a gateway). The strings are pkg/routing's Vendor
// values; pkg/providers does not import pkg/routing, so TestKindVendorNamesAreRoutingVendors
// (pkg/llm) pins the pairing.
func (k Kind) Vendor() string {
	switch k {
	case KindAnthropic:
		return "anthropic"
	case KindOpenAIResponses, KindOpenAIResponsesChatGPT:
		return "openai"
	}
	return ""
}
