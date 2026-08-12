// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// EjectReason names why a provider was removed from routing. The guard's
// eject/persist/expire machinery is reason-agnostic: a future detector (a
// provider that mangles tool-call records, say) adds a constant and an Observe
// rule, not new plumbing.
type EjectReason string

// ReasonNoCache: the provider stopped serving prompt-cache hits on prompts
// large enough that it should have.
const ReasonNoCache EjectReason = "no_cache"

const (
	// missStreakToEject is the number of consecutive qualifying cache misses
	// that ejects a provider. Measured, not guessed: across ~1200 healthy
	// Novita turns the worst consecutive miss streak was 1, while a broken
	// CoreWeave produced streaks of 15-28. See the design doc.
	missStreakToEject = 5
	// minCacheableTokens is the prompt size below which a provider declining to
	// cache is not evidence of anything.
	minCacheableTokens = 4096
	// maxEjectedPerModelLine bounds a pathological cascade: however many
	// providers break, a model line never loses more than this many at once.
	maxEjectedPerModelLine = 3
	// convMemoryLimit caps the per-conversation memory of the previous turn.
	// Exceeding it prunes entries untouched for convMemoryTTL.
	convMemoryLimit = 512
	convMemoryTTL   = time.Hour
)

// DefaultEjectTTL is how long an ejection holds before the provider becomes
// eligible again — long enough that a bad provider cannot re-burn the budget
// the same day, short enough that a repaired endpoint returns unattended.
const DefaultEjectTTL = 24 * time.Hour

type providerKey struct{ provider, modelLine string }

type ejection struct {
	reason    EjectReason
	at        time.Time
	expiresAt time.Time
}

// lastTurn is what the guard remembers about a conversation's previous turn:
// enough to tell whether the next turn is evidence about a provider.
type lastTurn struct {
	provider   string
	prefixHash string
	at         time.Time
}

// Observation is one completed OpenRouter turn as seen by the guard.
type Observation struct {
	Provider        string // response provider name, e.g. "CoreWeave"; empty for native Anthropic
	Model           string // OpenRouter model id, e.g. "deepseek/deepseek-v4-pro"
	Conversation    string
	PrefixHash      string
	InputTokens     int64
	CacheReadTokens int64
}

// EjectionRecord is one ejection, as written to the durable log.
type EjectionRecord struct {
	Provider  string
	ModelLine string
	Reason    EjectReason
	ExpiresAt time.Time
	Evidence  []byte // JSON; nil when there is nothing to say
}

// EjectionSink is the durable, append-only ejection log. Implementations must
// be safe for concurrent use. A nil sink makes the guard memory-only.
type EjectionSink interface {
	Append(ctx context.Context, e EjectionRecord) error
	Active(ctx context.Context, now time.Time) ([]EjectionRecord, error)
}

// ProviderGuard watches completed OpenRouter turns for a provider that has
// stopped serving prompt-cache hits, and ejects it from routing. In-memory
// state is authoritative; the sink is history that reseeds it at startup.
//
// A nil *ProviderGuard is valid and inert — that is what RAFIKI_PROVIDER_GUARD=off
// leaves at every call site.
type ProviderGuard struct {
	mu       sync.Mutex
	ejected  map[providerKey]ejection
	streaks  map[providerKey]int
	lastSeen map[string]lastTurn // by conversation id
	ttl      time.Duration
	sink     EjectionSink
	logger   *slog.Logger
	onEject  func(provider, modelLine string, reason EjectReason)
}

func NewProviderGuard(ttl time.Duration, logger *slog.Logger) *ProviderGuard {
	if ttl <= 0 {
		ttl = DefaultEjectTTL
	}
	return &ProviderGuard{
		ejected:  map[providerKey]ejection{},
		streaks:  map[providerKey]int{},
		lastSeen: map[string]lastTurn{},
		ttl:      ttl,
		logger:   logger,
	}
}

// SetSink attaches the durable ejection log. Safe to leave unset.
func (g *ProviderGuard) SetSink(s EjectionSink) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sink = s
}

// SetOnEject registers a callback fired once per ejection, outside the guard's
// lock. Consumed by metrics.
func (g *ProviderGuard) SetOnEject(fn func(provider, modelLine string, reason EjectReason)) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onEject = fn
}

// Observe records one completed OpenRouter turn.
//
// A turn is evidence against its provider only when every qualification rule
// holds (see qualifies). Notably this requires a conversation id and a prefix
// hash, both of which come from the capture path: with capture disabled the
// guard receives empty values, nothing qualifies, and the guard is inert. That
// is the intended fail-safe — no evidence beats false evidence.
func (g *ProviderGuard) Observe(now time.Time, obs Observation) {
	if g == nil {
		return
	}
	line := ModelLine(obs.Model)

	g.mu.Lock()
	prev, hadPrev := g.lastSeen[obs.Conversation]
	if obs.Conversation != "" {
		g.rememberLocked(now, obs)
	}
	if !qualifies(obs, prev, hadPrev) {
		g.mu.Unlock()
		return
	}
	key := providerKey{provider: obs.Provider, modelLine: line}
	if obs.CacheReadTokens > 0 {
		delete(g.streaks, key)
		g.mu.Unlock()
		return
	}
	g.streaks[key]++
	streak := g.streaks[key]
	if streak < missStreakToEject {
		g.mu.Unlock()
		return
	}
	if _, already := g.ejected[key]; already {
		g.mu.Unlock()
		return
	}
	rec := g.ejectLocked(now, key, ReasonNoCache, streak, obs)
	sink, onEject, logger := g.sink, g.onEject, g.logger
	g.mu.Unlock()

	logger.Warn("routing: provider ejected",
		"provider", rec.Provider, "model_line", rec.ModelLine, "reason", string(rec.Reason),
		"streak", streak, "expires_at", rec.ExpiresAt, "conversation", obs.Conversation)
	if onEject != nil {
		onEject(rec.Provider, rec.ModelLine, rec.Reason)
	}
	if sink != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sink.Append(ctx, rec); err != nil {
			// The in-memory ejection stands regardless; the log is history.
			logger.Warn("routing: ejection log append failed", "provider", rec.Provider, "error", err)
		}
	}
}

// ejectLocked records the ejection in memory and returns the record to persist.
// Caller holds g.mu.
func (g *ProviderGuard) ejectLocked(now time.Time, key providerKey, reason EjectReason, streak int, obs Observation) EjectionRecord {
	g.ejected[key] = ejection{reason: reason, at: now, expiresAt: now.Add(g.ttl)}
	delete(g.streaks, key)
	g.enforceCapLocked(now, key.modelLine)
	evidence, _ := json.Marshal(map[string]any{
		"streak":            streak,
		"conversation":      obs.Conversation,
		"input_tokens":      obs.InputTokens,
		"cache_read_tokens": obs.CacheReadTokens,
		"model":             obs.Model,
	})
	return EjectionRecord{
		Provider:  key.provider,
		ModelLine: key.modelLine,
		Reason:    reason,
		ExpiresAt: now.Add(g.ttl),
		Evidence:  evidence,
	}
}

// enforceCapLocked drops the oldest ejections for a model line until at most
// maxEjectedPerModelLine remain. Caller holds g.mu.
func (g *ProviderGuard) enforceCapLocked(now time.Time, line string) {
	var keys []providerKey
	for k, e := range g.ejected {
		if k.modelLine == line && e.expiresAt.After(now) {
			keys = append(keys, k)
		}
	}
	if len(keys) <= maxEjectedPerModelLine {
		return
	}
	sort.Slice(keys, func(i, j int) bool { return g.ejected[keys[i]].at.Before(g.ejected[keys[j]].at) })
	for _, k := range keys[:len(keys)-maxEjectedPerModelLine] {
		delete(g.ejected, k)
	}
}

// rememberLocked stores this turn as the conversation's previous turn, pruning
// stale conversations when the map grows past its limit. Caller holds g.mu.
func (g *ProviderGuard) rememberLocked(now time.Time, obs Observation) {
	if len(g.lastSeen) >= convMemoryLimit {
		for c, lt := range g.lastSeen {
			if now.Sub(lt.at) > convMemoryTTL {
				delete(g.lastSeen, c)
			}
		}
	}
	g.lastSeen[obs.Conversation] = lastTurn{provider: obs.Provider, prefixHash: obs.PrefixHash, at: now}
}

// qualifies reports whether obs is evidence about its provider's cache. All
// five rules must hold; see the design doc for why each exists.
func qualifies(obs Observation, prev lastTurn, hadPrev bool) bool {
	if obs.Provider == "" || obs.Conversation == "" || obs.PrefixHash == "" {
		return false // can't attribute it
	}
	if !hadPrev {
		return false // first turn of a conversation: nothing was cached yet
	}
	if prev.prefixHash != obs.PrefixHash {
		return false // we invalidated the cache, not the provider
	}
	if prev.provider != obs.Provider {
		return false // routed elsewhere last turn: a cold cache is expected
	}
	return obs.InputTokens+obs.CacheReadTokens > minCacheableTokens
}

// IgnoredFor returns the provider slugs to exclude for a model, as
// OpenRouter's provider.ignore list. Expired ejections are pruned in passing.
// The result is sorted, so callers and tests see a stable order.
func (g *ProviderGuard) IgnoredFor(now time.Time, model string) []string {
	if g == nil {
		return nil
	}
	line := ModelLine(model)
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for k, e := range g.ejected {
		if !e.expiresAt.After(now) {
			delete(g.ejected, k)
			continue
		}
		if k.modelLine == line {
			out = append(out, providerSlug(k.provider))
		}
	}
	sort.Strings(out)
	return out
}

// Ejected reports the currently-ejected (provider, model line) pairs, for
// gauges and diagnostics.
func (g *ProviderGuard) Ejected(now time.Time) []EjectionRecord {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []EjectionRecord
	for k, e := range g.ejected {
		if !e.expiresAt.After(now) {
			continue
		}
		out = append(out, EjectionRecord{Provider: k.provider, ModelLine: k.modelLine, Reason: e.reason, ExpiresAt: e.expiresAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelLine != out[j].ModelLine {
			return out[i].ModelLine < out[j].ModelLine
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// Rehydrate seeds in-memory state from the sink's unexpired ejections. A
// failure is logged and swallowed: ejection state is a cost optimisation, not a
// correctness requirement, and a daemon must not fail to start over it.
func (g *ProviderGuard) Rehydrate(ctx context.Context, now time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	sink, logger := g.sink, g.logger
	g.mu.Unlock()
	if sink == nil {
		return
	}
	recs, err := sink.Active(ctx, now)
	if err != nil {
		logger.Warn("routing: ejection rehydrate failed", "error", err)
		return
	}
	g.mu.Lock()
	for _, r := range recs {
		g.ejected[providerKey{provider: r.Provider, modelLine: r.ModelLine}] = ejection{
			reason: r.Reason, at: now, expiresAt: r.ExpiresAt,
		}
	}
	for line := range linesOf(recs) {
		g.enforceCapLocked(now, line)
	}
	n := len(g.ejected)
	g.mu.Unlock()
	logger.Info("routing: rehydrated provider ejections", "count", n)
}

func linesOf(recs []EjectionRecord) map[string]struct{} {
	out := map[string]struct{}{}
	for _, r := range recs {
		out[r.ModelLine] = struct{}{}
	}
	return out
}

// ModelLine folds a stamped point release into its model line, so an ejection
// recorded against one stamp still applies after OpenRouter bumps it:
// "deepseek/deepseek-v4-pro-20260423" -> "deepseek/deepseek-v4-pro". Only a
// trailing "-" plus four or more digits is a stamp; "openai/gpt-4" is a model
// name, not a stamped release, and passes through unchanged.
func ModelLine(id string) string {
	i := strings.LastIndexByte(id, '-')
	if i < 0 || len(id)-i-1 < 4 {
		return id
	}
	for _, c := range id[i+1:] {
		if c < '0' || c > '9' {
			return id
		}
	}
	return id[:i]
}

// providerSlug converts OpenRouter's display name to the slug its routing
// object expects: "CoreWeave" -> "coreweave", "Amazon Bedrock" ->
// "amazon-bedrock".
func providerSlug(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "-")
}
