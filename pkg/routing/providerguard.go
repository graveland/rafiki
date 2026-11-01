// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// EjectReason names why a provider was removed from routing. The guard's
// eject/persist/expire machinery is reason-agnostic: a new detector adds a
// constant and an Observe rule, not new plumbing.
type EjectReason string

// ReasonNoCache: the provider stopped serving prompt-cache hits on prompts
// large enough that it should have.
const ReasonNoCache EjectReason = "no_cache"

// ReasonOperator: a human banned the provider (Ban). May carry no expiry at
// all, and is never lifted by the guard's own sweep (LiftLine).
const ReasonOperator EjectReason = "operator"

// ReasonLift: a human lifted an operator ban (Lift). Log-only — it supersedes
// the ban row in the durable log and never lives in the in-memory map.
const ReasonLift EjectReason = "lift"

// AllModelLines is the model line an operator ban is recorded under: it
// applies to every model line.
const AllModelLines = "*"

// ErrNoBan is Lift's answer for a provider with no live operator ban.
var ErrNoBan = errors.New("provider has no active operator ban")

// ErrInvalidBan is Ban/Lift's answer for an unusable provider or duration.
var ErrInvalidBan = errors.New("invalid provider ban")

const (
	// missStreakToEject is the number of consecutive qualifying cache misses
	// that ejects a provider. Measured, not guessed: across ~1200 healthy
	// Novita turns the worst consecutive miss streak was 1, while a broken
	// CoreWeave produced streaks of 15-28. See the design doc.
	missStreakToEject = 5
	// minCacheableTokens is the prompt size below which a provider declining to
	// cache is not evidence of anything.
	minCacheableTokens = 4096
	// convMemoryLimit caps the per-conversation memory of the previous turn and
	// of each conversation's health evidence. Exceeding it prunes entries
	// untouched for convMemoryTTL.
	convMemoryLimit = 512
	convMemoryTTL   = time.Hour
)

// DefaultEjectTTL is how long an ejection holds before the provider becomes
// eligible again — long enough that a bad provider cannot re-burn the budget
// the same day, short enough that a repaired endpoint returns unattended.
const DefaultEjectTTL = 24 * time.Hour

type providerKey struct{ provider, modelLine string }

type ejection struct {
	reason EjectReason
	at     time.Time
	// expiresAt zero means "until lifted" — only an operator ban can have
	// that, since the guard's own ejections always expire after g.ttl.
	expiresAt time.Time
	note      string
}

func (e ejection) live(now time.Time) bool {
	return e.expiresAt.IsZero() || e.expiresAt.After(now)
}

// lastTurn is what the guard remembers about a conversation's previous turn:
// enough to tell whether the next turn is evidence about a provider, and which
// provider served it on which model line (PinFor).
type lastTurn struct {
	provider   string
	modelLine  string
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
	// Pinned is the provider slug this attempt was forced onto with only=[slug];
	// "" when OpenRouter chose. Failures are attributed ONLY through Pinned.
	Pinned string
	// Failed reports the attempt returned an error. FailureKind says what kind;
	// FailureDetail is a short excerpt (<= 300 bytes) for ban evidence.
	Failed        bool
	FailureKind   FailureKind
	FailureDetail string
	// StopReason is the response's stop reason spelling ("max_tokens", "tool_use", ...); "" unknown.
	StopReason   string
	OutputTokens int64
	// MaxTokens is the max_tokens the REQUEST asked for; 0 unknown.
	MaxTokens int64
	// Repetition is RepetitionScore of the turn's text+thinking; nil = not scored
	// (too short). Never use 0 for "not scored": 0 is maximal repetition.
	Repetition *float64
	// ToolArgsInvalid counts tool calls whose input was not a JSON object or lacked
	// a key the tool's schema marks required.
	ToolArgsInvalid int
}

// EjectionRecord is one ejection, as written to the durable log.
type EjectionRecord struct {
	Provider  string
	ModelLine string
	Reason    EjectReason
	CreatedAt time.Time // set on reads; the sink stamps its own on Append
	ExpiresAt time.Time // zero = until lifted (operator bans only)
	Evidence  []byte    // JSON; nil when there is nothing to say
	Note      string    // operator's reason; empty for the guard's own ejections
}

// EjectionSink is the durable, append-only ejection log. Implementations must
// be safe for concurrent use. A nil sink makes the guard memory-only.
type EjectionSink interface {
	Append(ctx context.Context, e EjectionRecord) error
	Active(ctx context.Context, now time.Time) ([]EjectionRecord, error)
}

// ProviderGuard judges provider health turn by turn and ejects a provider from
// routing when a verdict lands: the cache rule (a provider that stopped serving
// prompt-cache hits) plus the per-conversation rules in providerhealth.go
// (unavailable, output_cap, tool_corrupt, degenerate). It also holds operator
// bans (Ban/Lift) and remembers which provider each conversation is on, for
// pinning (PinFor). In-memory state is authoritative; the sink is history that
// reseeds it at startup.
//
// A nil *ProviderGuard is valid and inert. RAFIKI_PROVIDER_GUARD=off does NOT
// produce one: it calls SetObserve(false), which stops automatic ejection
// while operator bans keep applying.
type ProviderGuard struct {
	// opMu serialises Ban/Lift end to end (sink write, then map update), so
	// concurrent operator calls land in the log and the map in the same
	// order. Held across the sink write; mu is not.
	opMu     sync.Mutex
	mu       sync.Mutex
	observe  bool
	ejected  map[providerKey]ejection
	streaks  map[providerKey]int
	lastSeen map[string]lastTurn    // by conversation id
	health   map[string]*convHealth // by conversation id
	// turns counts successful attempts per (slug, line) this process has seen.
	// Evidence only: it never decides a ban on its own.
	turns   map[providerKey]int64
	ttl     time.Duration
	sink    EjectionSink
	logger  *slog.Logger
	onEject func(provider, modelLine string, reason EjectReason)
	dir     *ProviderDirectory
}

func NewProviderGuard(ttl time.Duration, logger *slog.Logger) *ProviderGuard {
	if ttl <= 0 {
		ttl = DefaultEjectTTL
	}
	return &ProviderGuard{
		observe:  true,
		ejected:  map[providerKey]ejection{},
		streaks:  map[providerKey]int{},
		lastSeen: map[string]lastTurn{},
		health:   map[string]*convHealth{},
		turns:    map[providerKey]int64{},
		ttl:      ttl,
		logger:   logger,
	}
}

// SetDirectory attaches the provider directory the guard resolves slugs
// through. Unset, every slug is guessSlug's guess. Call before serving.
func (g *ProviderGuard) SetDirectory(d *ProviderDirectory) {
	g.mu.Lock()
	g.dir = d
	g.mu.Unlock()
}

// Slug returns provider's OpenRouter routing slug, provider being a display
// name as responses report it or a slug. Nil-safe: a nil guard guesses.
func (g *ProviderGuard) Slug(provider string) string {
	if g == nil {
		return guessSlug(provider)
	}
	g.mu.Lock()
	d := g.dir
	g.mu.Unlock()
	return d.Slug(provider)
}

// Resolve returns provider's routing slug for an operator action, loading the
// directory on demand — the same call Ban makes. degraded is true when the
// directory could not be loaded and slug is merely a guess, so an OpenRouter
// outage never blocks the action; err is ErrUnknownProvider for a name the
// loaded directory does not list. Nil-safe: a nil guard (or no directory)
// degrades every name.
func (g *ProviderGuard) Resolve(ctx context.Context, provider string) (slug string, degraded bool, err error) {
	if g == nil {
		return (*ProviderDirectory)(nil).Resolve(ctx, provider)
	}
	g.mu.Lock()
	d := g.dir
	g.mu.Unlock()
	return d.Resolve(ctx, provider)
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

// SetObserve enables or disables automatic ejection. Disabled, Observe is a
// no-op; operator bans and rehydrated ejections still apply.
func (g *ProviderGuard) SetObserve(on bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observe = on
}

// Persistent reports whether bans survive a restart (a sink is attached).
func (g *ProviderGuard) Persistent() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sink != nil
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
//
// Beyond the cache rule, an observation can ban a provider outright: a run of
// failed pinned attempts (unavailable), a truncated answer (output_cap),
// mangled tool arguments (tool_corrupt) or a repetition loop (degenerate). At
// most one ban per observation, in that order of precedence.
func (g *ProviderGuard) Observe(now time.Time, obs Observation) {
	if g == nil {
		return
	}
	line := ModelLine(obs.Model)

	g.mu.Lock()
	if !g.observe {
		g.mu.Unlock()
		return
	}
	if obs.Failed {
		rec, banned := g.failureBanLocked(now, line, obs)
		g.mu.Unlock()
		if banned {
			g.reportBan(rec, obs.Conversation, "failure_kind", obs.FailureKind.String())
		}
		return
	}
	if obs.Provider != "" {
		rec, banned := g.successBanLocked(now, line, obs)
		if banned {
			// Remember the turn before leaving: the ban's key is the slug, the
			// no_cache path's key is the display name, so the already-ejected
			// check below would NOT stop a second ban for the same provider.
			if obs.Conversation != "" {
				g.rememberLocked(now, obs)
			}
			g.mu.Unlock()
			g.reportBan(rec, obs.Conversation)
			return
		}
	}
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
	// The no_cache evidence keys are the ones this rule has always written; the
	// newer rules carry banEvidenceLocked's common set instead.
	rec := g.banLocked(now, key, ReasonNoCache, g.ttl, map[string]any{
		"streak":            streak,
		"conversation":      obs.Conversation,
		"input_tokens":      obs.InputTokens,
		"cache_read_tokens": obs.CacheReadTokens,
		"model":             obs.Model,
	})
	delete(g.streaks, key)
	g.mu.Unlock()

	g.reportBan(rec, obs.Conversation, "streak", streak)
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
	g.lastSeen[obs.Conversation] = lastTurn{
		provider:   obs.Provider,
		modelLine:  ModelLine(obs.Model),
		prefixHash: obs.PrefixHash,
		at:         now,
	}
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
// OpenRouter's provider.ignore list: the guard's ejections for the model's
// line plus every operator ban. Expired ejections are pruned in passing. The
// result is sorted and deduplicated, so callers and tests see a stable order.
func (g *ProviderGuard) IgnoredFor(now time.Time, model string) []string {
	if g == nil {
		return nil
	}
	operator, automatic := g.ExclusionsFor(now, model)
	if len(operator) == 0 && len(automatic) == 0 {
		return nil
	}
	out := make([]string, 0, len(operator)+len(automatic))
	out = append(out, operator...)
	out = append(out, automatic...)
	sort.Strings(out)
	return out
}

// Ban excludes provider from routing for every model line, for ttl (zero =
// until lifted). The provider is resolved to its OpenRouter slug through the
// directory, so "OpenInference" and "open-inference" are one ban, and a name
// the directory does not list is refused (ErrInvalidBan wrapping
// ErrUnknownProvider) — OpenRouter silently ignores an unknown slug, so such
// a ban would exclude nothing. With the directory unreachable the slug is
// guessed and the ban accepted. Banning an already-banned provider replaces
// the ban's expiry and note.
//
// The durable log is written FIRST and its failure returned: unlike the
// guard's own ejections, a ban is a request whose caller must learn whether
// it stuck. With no sink the ban is memory-only (see Persistent).
func (g *ProviderGuard) Ban(ctx context.Context, now time.Time, provider string, ttl time.Duration, note string) (EjectionRecord, error) {
	if g == nil {
		return EjectionRecord{}, errors.New("provider guard is not configured")
	}
	if ttl < 0 {
		return EjectionRecord{}, fmt.Errorf("%w: duration must not be negative, got %s", ErrInvalidBan, ttl)
	}
	g.mu.Lock()
	dir := g.dir
	g.mu.Unlock()
	resolved, degraded, err := dir.Resolve(ctx, strings.TrimSpace(provider))
	if err != nil {
		return EjectionRecord{}, fmt.Errorf("%w: %w", ErrInvalidBan, err)
	}
	slug, err := banSlug(resolved)
	if err != nil {
		return EjectionRecord{}, err
	}
	if degraded && dir != nil {
		g.logger.Warn("routing: provider directory unavailable; ban slug is a guess and may match nothing",
			"provider", provider, "slug", slug)
	}
	rec := EjectionRecord{Provider: slug, ModelLine: AllModelLines, Reason: ReasonOperator, CreatedAt: now, Note: note}
	if ttl > 0 {
		rec.ExpiresAt = now.Add(ttl)
	}
	g.opMu.Lock()
	defer g.opMu.Unlock()
	g.mu.Lock()
	sink, onEject, logger := g.sink, g.onEject, g.logger
	g.mu.Unlock()
	if sink != nil {
		if err := sink.Append(ctx, rec); err != nil {
			return EjectionRecord{}, fmt.Errorf("record ban: %w", err)
		}
	}
	g.mu.Lock()
	g.ejected[providerKey{provider: slug, modelLine: AllModelLines}] = ejection{
		reason: ReasonOperator, at: now, expiresAt: rec.ExpiresAt, note: note,
	}
	g.mu.Unlock()
	logger.Warn("routing: provider banned by operator",
		"provider", slug, "expires_at", rec.ExpiresAt, "note", note)
	if onEject != nil {
		onEject(slug, AllModelLines, ReasonOperator)
	}
	return rec, nil
}

// Lift removes provider's operator ban, recording the lift in the durable log
// first (append-only: the ban row stays as history). It does not touch the
// guard's own ejections of that provider on specific model lines — those have
// their own cause and expiry. ErrNoBan when no operator ban is live.
//
// provider is matched against the stored ban key exactly first, then as its
// directory slug, then as the guessed slug — so a ban stored under a slug
// OpenRouter never had ("openinference") can still be lifted by that name
// without the directory redirecting the lift onto the real one.
func (g *ProviderGuard) Lift(ctx context.Context, now time.Time, provider string) error {
	if g == nil {
		return errors.New("provider guard is not configured")
	}
	provider = strings.TrimSpace(provider)
	if _, err := banSlug(provider); err != nil {
		return err
	}
	g.opMu.Lock()
	defer g.opMu.Unlock()
	g.mu.Lock()
	sink, logger, dir := g.sink, g.logger, g.dir
	g.mu.Unlock()
	var slug string
	for _, cand := range []string{provider, dir.Slug(provider), guessSlug(provider)} {
		g.mu.Lock()
		e, ok := g.ejected[providerKey{provider: cand, modelLine: AllModelLines}]
		g.mu.Unlock()
		if ok && e.reason == ReasonOperator && e.live(now) {
			slug = cand
			break
		}
	}
	if slug == "" {
		return fmt.Errorf("%w: %s", ErrNoBan, provider)
	}
	key := providerKey{provider: slug, modelLine: AllModelLines}
	if sink != nil {
		rec := EjectionRecord{Provider: slug, ModelLine: AllModelLines, Reason: ReasonLift, ExpiresAt: now}
		if err := sink.Append(ctx, rec); err != nil {
			return fmt.Errorf("record lift: %w", err)
		}
	}
	g.mu.Lock()
	delete(g.ejected, key)
	g.mu.Unlock()
	logger.Info("routing: operator ban lifted", "provider", slug)
	return nil
}

func banSlug(provider string) (string, error) {
	slug := guessSlug(provider)
	if slug == "" || slug == AllModelLines || strings.ContainsAny(slug, "\t\r\n\x00") {
		return "", fmt.Errorf("%w: provider slug %q", ErrInvalidBan, provider)
	}
	return slug, nil
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
		if !e.live(now) {
			continue
		}
		out = append(out, EjectionRecord{
			Provider: k.provider, ModelLine: k.modelLine, Reason: e.reason,
			CreatedAt: e.at, ExpiresAt: e.expiresAt, Note: e.note,
		})
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
// ReasonUnavailable record is skipped: those bans are memory-only, so a restart
// gives the provider a fresh chance at a capacity problem. A failure is logged
// and swallowed: ejection state is a cost optimisation, not a correctness
// requirement, and a daemon must not fail to start over it.
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
		if r.Reason == ReasonUnavailable {
			// Short bans are memory-only: a restart gives the provider a fresh
			// chance at a capacity problem that has very likely cleared.
			continue
		}
		at := r.CreatedAt
		if at.IsZero() {
			at = now
		}
		g.ejected[providerKey{provider: r.Provider, modelLine: r.ModelLine}] = ejection{
			reason: r.Reason, at: at, expiresAt: r.ExpiresAt, note: r.Note,
		}
	}
	n := len(g.ejected)
	g.mu.Unlock()
	logger.Info("routing: rehydrated provider ejections", "count", n)
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
