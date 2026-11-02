package llm

import (
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// ResolveProviderLatest pins "<provider>/<name>-latest" to the concrete,
// provider-qualified model id for the whole conversation, so the cached prefix
// cannot drift with catalog refreshes. The provider's KIND decides which vendor
// the name is resolved against (providers.Kind.Vendor), never its name, so any
// provider named in providers.toml works. Everything that is not a vendor-kind
// provider's latest name returns unchanged: the "anthropic/" marker (that is
// routing.ResolveModel's), an unknown provider (Set.Resolve reports it), a
// kind with no vendor, a name the provider declares itself, and any id that is
// not tier-shaped. A bracketed routing spec is carried over untouched.
//
// A vendor-kind provider's latest name that cannot resolve is an error naming
// the request and the tiers the catalog offers; there is no fallback id.
func ResolveProviderLatest(set *providers.Set, cat *routing.ModelCatalog, model string) (string, error) {
	if set == nil {
		return model, nil
	}
	base, spec := model, ""
	if i := strings.IndexByte(model, '['); i >= 0 {
		base, spec = model[:i], model[i:]
	}
	name, local := providers.SplitRaw(base)
	if name == "" || name+"/" == routing.NativeAnthropicPrefix {
		return model, nil
	}
	p, ok := set.Get(name)
	if !ok {
		return model, nil
	}
	vendor := routing.Vendor(p.Kind.Vendor())
	tier, isLatest := routing.LatestTier(vendor, local)
	if !isLatest {
		return model, nil
	}
	if _, declared := p.Models[local]; declared {
		return model, nil
	}
	if cat == nil {
		return "", fmt.Errorf("cannot resolve %q: no OpenRouter model catalog", model)
	}
	direct, _, ok := cat.ResolveTier(vendor, tier)
	if !ok {
		return "", fmt.Errorf("cannot resolve %q: no %s model for tier %q in the OpenRouter catalog (offline with a cold cache, or an unknown tier; known: %s)",
			model, vendor, tier, strings.Join(cat.LatestNames(vendor), ", "))
	}
	return name + "/" + direct + spec, nil
}
