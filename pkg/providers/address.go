// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"fmt"
	"strings"
)

// SplitRaw splits "provider/model" on the FIRST slash without consulting any
// registry. A bare id yields an empty provider name. Everything after the first
// slash is the provider's model id, slashes and all — an OpenRouter id
// ("openrouter/deepseek/deepseek-chat") and a path-shaped local id
// ("vmlx/models/Qwen3.8-27B") both survive intact.
func SplitRaw(model string) (name, modelID string) {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

// Split resolves a model id to its provider and the provider-local model id.
//
// A bare id resolves against DefaultProvider. A first segment that names no
// configured provider is an ERROR — never a fallthrough to DefaultProvider.
// This is the whole point of the addressing change: the id names its provider
// explicitly instead of the code inferring one from the id's shape, so
// "deepseek/deepseek-chat" (an OpenRouter id before this change) must fail
// loudly and be respelled "openrouter/deepseek/deepseek-chat".
//
// If the provider declares a models.<alias> table matching modelID, the
// alias's real id is substituted before returning — every caller that builds
// a request from the returned model id (pkg/llm's prepareSend chief among
// them) gets the translation for free, with nothing alias-aware downstream.
func (s *Set) Split(model string) (Provider, string, error) {
	if model == "" {
		return Provider{}, "", fmt.Errorf("providers: no model specified")
	}
	name, modelID := SplitRaw(model)
	if name == "" {
		name = s.DefaultProvider
	}
	if modelID == "" {
		return Provider{}, "", fmt.Errorf("providers: model %q has an empty model id", model)
	}
	p, ok := s.Providers[name]
	if !ok {
		return Provider{}, "", fmt.Errorf("providers: model %q names unknown provider %q (configured: %s)", model, name, strings.Join(s.Names(), ", "))
	}
	if alias, ok := p.Models[modelID]; ok {
		modelID = alias.ID
	}
	return p, modelID, nil
}
