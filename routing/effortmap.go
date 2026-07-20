package routing

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// EffortLevels are the reasoning-effort values OpenRouter accepts, lowest to
// highest. The ordering lets a clamp pick the nearest allowed value.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// EffortCache is a per-process record of the effort values each model accepts,
// learned at runtime from the provider's own rejections (never from a static
// snapshot). A model absent from it is unconstrained — its effort passes
// through untouched. Concurrency-safe: the proxy reads it on every request and
// writes it when a rejection teaches a new constraint.
type EffortCache struct {
	mu     sync.RWMutex
	models map[string][]string
}

func NewEffortCache() *EffortCache { return &EffortCache{models: map[string][]string{}} }

// Learn records the allowed effort set for a model, parsed from a rejection.
func (c *EffortCache) Learn(model string, allowed []string) {
	c.mu.Lock()
	c.models[model] = allowed
	c.mu.Unlock()
}

// Clamp decides how to treat a requested effort for a model given what the cache
// has learned. action is:
//   - "keep":  model not yet known to constrain effort, or the value is allowed.
//   - "clamp": value not allowed; effort is the nearest allowed value.
//   - "strip": the model allows no effort value at all; effort should be removed.
func (c *EffortCache) Clamp(model, requested string) (effort string, action string) {
	c.mu.RLock()
	allowed, ok := c.models[model]
	c.mu.RUnlock()
	if !ok {
		return requested, "keep"
	}
	if len(allowed) == 0 {
		return "", "strip"
	}
	if slices.Contains(allowed, requested) {
		return requested, "keep"
	}
	return nearestEffort(allowed, requested), "clamp"
}

// nearestEffort picks the highest allowed value <= requested; if none is <=, the
// lowest allowed value. allowed is non-empty.
func nearestEffort(allowed []string, requested string) string {
	rr := slices.Index(EffortLevels, requested)
	if rr < 0 {
		rr = len(EffortLevels) // unknown requested value: treat as the highest
	}
	best, bestRank := "", -1
	for _, a := range allowed {
		ar := slices.Index(EffortLevels, a)
		if ar <= rr && ar > bestRank {
			best, bestRank = a, ar
		}
	}
	if best != "" {
		return best
	}
	lowest, lr := allowed[0], slices.Index(EffortLevels, allowed[0])
	for _, a := range allowed[1:] {
		if r := slices.Index(EffortLevels, a); r < lr {
			lowest, lr = a, r
		}
	}
	return lowest
}

var (
	supportedRe = regexp.MustCompile(`(?i)supported values are:\s*((?:'[^']*'\s*,?\s*)+)`)
	quotedRe    = regexp.MustCompile(`'([^']*)'`)
)

// ParseSupportedEfforts extracts the enumerated allowed effort values from a
// provider rejection body. It searches the raw bytes, so it works whether the
// "Supported values are: ..." message is top-level or nested in OpenRouter's
// error.metadata.raw. Only EffortLevels members are kept, de-duplicated, in
// low->high order. ok is false when nothing effort-shaped is enumerated.
func ParseSupportedEfforts(body []byte) (allowed []string, ok bool) {
	m := supportedRe.FindSubmatch(body)
	if m == nil {
		return nil, false
	}
	seen := map[string]bool{}
	var out []string
	for _, q := range quotedRe.FindAllSubmatch(m[1], -1) {
		v := strings.ToLower(string(q[1]))
		if slices.Contains(EffortLevels, v) && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	slices.SortFunc(out, func(a, b string) int {
		return slices.Index(EffortLevels, a) - slices.Index(EffortLevels, b)
	})
	return out, true
}
