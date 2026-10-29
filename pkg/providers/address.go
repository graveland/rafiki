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

// Resolve resolves a model id to its provider, the provider-local model id,
// and the matched ModelDecl — everything Split returns, plus the declaration
// the id matched (nil when the model id is not one of the provider's declared
// models).
//
// The declaration is returned BY VALUE COPY (a pointer to a local, not into
// the registry map), so a caller can't mutate the registry through it. The
// pointer is how a declaration's routing metadata reaches the request: Only
// travels with the declaration because two declarations may share one real id
// and each must keep its own provider pin — a lookup by the substituted id
// could not tell them apart.
func (s *Set) Resolve(model string) (Provider, string, *ModelDecl, error) {
	if model == "" {
		return Provider{}, "", nil, fmt.Errorf("providers: no model specified")
	}
	name, modelID := SplitRaw(model)
	if name == "" {
		name = s.DefaultProvider
	}
	if modelID == "" {
		return Provider{}, "", nil, fmt.Errorf("providers: model %q has an empty model id", model)
	}
	p, ok := s.Providers[name]
	if !ok {
		return Provider{}, "", nil, fmt.Errorf("providers: model %q names unknown provider %q (configured: %s)", model, name, strings.Join(s.Names(), ", "))
	}
	if decl, ok := p.Models[modelID]; ok {
		return p, decl.ID, &decl, nil
	}
	return p, modelID, nil, nil
}

// Split resolves a model id to its provider and the provider-local model id.
// A thin wrapper over Resolve, kept because most callers don't need the
// declaration.
//
// A bare id resolves against DefaultProvider. A first segment that names no
// configured provider is an ERROR — never a fallthrough to DefaultProvider.
// This is the whole point of the addressing change: the id names its provider
// explicitly instead of the code inferring one from the id's shape, so
// "deepseek/deepseek-chat" (an OpenRouter id before this change) must fail
// loudly and be respelled "openrouter/deepseek/deepseek-chat".
//
// If the provider declares a models.<key> table matching modelID, the
// declaration's real id is substituted before returning — every caller that
// builds a request from the returned model id (pkg/llm's prepareSend chief
// among them) gets the translation for free, with nothing declaration-aware
// downstream.
func (s *Set) Split(model string) (Provider, string, error) {
	p, modelID, _, err := s.Resolve(model)
	return p, modelID, err
}
