// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

const (
	// ReasonUnavailable: the pinned provider kept failing (or stopped serving
	// the model) for a conversation. Short-lived and memory-only: a capacity
	// problem clears on its own, and a restart gives the provider a fresh
	// chance.
	ReasonUnavailable EjectReason = "unavailable"
	// ReasonOutputCap: a max_tokens stop at no more than half the max_tokens
	// requested — the provider truncated the answer it was asked to produce.
	ReasonOutputCap EjectReason = "output_cap"
	// ReasonToolCorrupt: tool calls whose input is not an object or lacks a
	// schema-required key.
	ReasonToolCorrupt EjectReason = "tool_corrupt"
	// ReasonDegenerate: repetition-loop output.
	ReasonDegenerate EjectReason = "degenerate"
)

const (
	// pinnedFailuresToBan is the number of consecutive failed pinned attempts
	// in one conversation that ban the provider. Four is where the agent loop's
	// own backoff (1s, 2s, 4s — ~7s total) has had its chance to ride out a
	// network blip: a provider still failing after that is not a blip.
	pinnedFailuresToBan = 4
	// unavailableTTL is how long a capacity/outage ban holds. Short on purpose:
	// it is memory-only (Rehydrate skips it), so a restart clears it too.
	unavailableTTL = 4 * time.Hour
	// structuralTTL is how long an output_cap, tool_corrupt or degenerate ban
	// holds. Long and persisted: these are properties of the provider's serving
	// stack, not of the moment, and they cost a whole turn to discover.
	structuralTTL = 24 * time.Hour
	// structuralRepeatsToBan is how many tool_corrupt or degenerate turns in one
	// conversation ban the provider. One event is noise (a model's own bad turn);
	// two in the same conversation is the provider.
	structuralRepeatsToBan = 2
	// repetitionDegenerate is the RepetitionScore below which output is a loop.
	// Measured: production loops scored 0.006 and 0.148, while capped non-loops
	// scored at least 0.24.
	repetitionDegenerate = 0.2
	// failureDetailLimit bounds the excerpt carried into ban evidence, so a
	// provider's error page cannot bloat the durable log.
	failureDetailLimit = 300
)

// FailureKind classifies a failed attempt for the guard. The zero value is
// FailureNone — no evidence — so an Observation that sets Failed but forgets
// the kind can never ban anyone.
type FailureKind uint8

const (
	FailureNone        FailureKind = iota // not classified: never evidence
	FailureNotProvider                    // account limit, billing, request shape, our own routing spec: never evidence
	FailureTransient                      // 5xx, overloaded, transport error, stream timeout, a pinned provider's 429
	FailureNoEligible                     // OpenRouter: nothing eligible under this request's provider object
)

// String names the kind for logs and ban evidence.
func (k FailureKind) String() string {
	switch k {
	case FailureTransient:
		return "transient"
	case FailureNotProvider:
		return "not_provider"
	case FailureNoEligible:
		return "no_eligible"
	default:
		return "none"
	}
}

// convHealth is one conversation's running evidence against the provider it
// is currently on. Any observation naming a different slug resets it.
type convHealth struct {
	slug        string
	failures    int // consecutive failed pinned attempts on slug; reset by a success on slug
	toolCorrupt int // successful attempts on slug with ToolArgsInvalid > 0
	degenerate  int // successful attempts on slug with Repetition below repetitionDegenerate
	at          time.Time
}

// healthLocked returns the conversation's evidence against slug, creating it
// on first use and resetting it when the conversation moved to another
// provider. It returns nil for a conversation-less observation: with no
// conversation there is nothing to count, and the rules that need no
// conversation (output cap) do not call it. Caller holds g.mu.
func (g *ProviderGuard) healthLocked(now time.Time, conversation, slug string) *convHealth {
	if conversation == "" {
		return nil
	}
	if len(g.health) >= convMemoryLimit {
		for c, h := range g.health {
			if now.Sub(h.at) > convMemoryTTL {
				delete(g.health, c)
			}
		}
	}
	h, ok := g.health[conversation]
	if !ok || h.slug != slug {
		h = &convHealth{slug: slug}
		g.health[conversation] = h
	}
	h.at = now
	return h
}

// ejectedLiveLocked reports whether key already holds a live ejection of any
// reason. A ban is skipped in that case: the provider is already out, and a
// second row would only be noise in the durable log. Caller holds g.mu.
func (g *ProviderGuard) ejectedLiveLocked(now time.Time, key providerKey) bool {
	e, ok := g.ejected[key]
	return ok && e.live(now)
}

// banLocked records a ban in memory and returns the record to persist.
// Reporting (log, callback, sink write) is the caller's job, after g.mu is
// released — see reportBan. Caller holds g.mu.
func (g *ProviderGuard) banLocked(now time.Time, key providerKey, reason EjectReason, ttl time.Duration, evidence map[string]any) EjectionRecord {
	expiresAt := now.Add(ttl)
	g.ejected[key] = ejection{reason: reason, at: now, expiresAt: expiresAt}
	ev, _ := json.Marshal(evidence)
	return EjectionRecord{
		Provider:  key.provider,
		ModelLine: key.modelLine,
		Reason:    reason,
		ExpiresAt: expiresAt,
		Evidence:  ev,
	}
}

// banEvidenceLocked is the evidence every detector rule carries: who was
// blamed, for which model, in which conversation, and how much of a track
// record the provider had on the line — so an operator can tell a provider
// that broke on its first turn from one that broke after a hundred good ones.
// extra adds the rule's own fields. Caller holds g.mu.
func (g *ProviderGuard) banEvidenceLocked(key providerKey, obs Observation, extra map[string]any) map[string]any {
	ev := map[string]any{
		"conversation":           obs.Conversation,
		"model":                  obs.Model,
		"pinned":                 obs.Pinned,
		"provider_turns_on_line": g.turns[key],
	}
	for k, v := range extra {
		ev[k] = v
	}
	return ev
}

// reportBan logs, notifies and persists one ban recorded by banLocked. It must
// be called WITHOUT g.mu held: the sink write is I/O and the callback is
// foreign code. extra carries any rule-specific log attribute (the no_cache
// path's streak). The in-memory ban stands whatever the sink says.
func (g *ProviderGuard) reportBan(rec EjectionRecord, conversation string, extra ...any) {
	g.mu.Lock()
	logger, sink, onEject, dir := g.logger, g.sink, g.onEject, g.dir
	g.mu.Unlock()
	attrs := []any{"provider", dir.Slug(rec.Provider), "model_line", rec.ModelLine, "reason", string(rec.Reason)}
	attrs = append(attrs, extra...)
	attrs = append(attrs, "expires_at", rec.ExpiresAt, "conversation", conversation)
	logger.Warn("routing: provider ejected", attrs...)
	if onEject != nil {
		onEject(rec.Provider, rec.ModelLine, rec.Reason)
	}
	if sink != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sink.Append(ctx, rec); err != nil {
			logger.Warn("routing: ejection log append failed", "provider", rec.Provider, "error", err)
		}
	}
}

// failureBanLocked judges a failed attempt. Failures are attributed ONLY
// through Pinned: an unpinned failure says nothing about which provider would
// have been at fault, and neither does a kind we cannot blame on the provider.
// A failure never touches lastSeen, streaks or no_cache — it is not evidence
// about caching. Caller holds g.mu.
func (g *ProviderGuard) failureBanLocked(now time.Time, line string, obs Observation) (EjectionRecord, bool) {
	if obs.Pinned == "" {
		return EjectionRecord{}, false
	}
	slug := obs.Pinned
	key := providerKey{provider: slug, modelLine: line}
	h := g.healthLocked(now, obs.Conversation, slug)
	failures := 1
	if h != nil {
		failures = h.failures
	}
	switch obs.FailureKind {
	case FailureNoEligible:
		// OpenRouter had nothing eligible under this request's provider object:
		// the pinned provider is not serving the model at all.
		if h != nil {
			h.failures++
			failures = h.failures
		}
		if g.ejectedLiveLocked(now, key) {
			return EjectionRecord{}, false
		}
		return g.banLocked(now, key, ReasonUnavailable, unavailableTTL, g.banEvidenceLocked(key, obs, map[string]any{
			"failures":     failures,
			"failure_kind": obs.FailureKind.String(),
			"detail":       clipDetail(obs.FailureDetail),
		})), true
	case FailureTransient:
		if h == nil {
			return EjectionRecord{}, false
		}
		h.failures++
		if h.failures < pinnedFailuresToBan {
			return EjectionRecord{}, false
		}
		failures = h.failures
		h.failures = 0
		if g.ejectedLiveLocked(now, key) {
			return EjectionRecord{}, false
		}
		return g.banLocked(now, key, ReasonUnavailable, unavailableTTL, g.banEvidenceLocked(key, obs, map[string]any{
			"failures":     failures,
			"failure_kind": obs.FailureKind.String(),
			"detail":       clipDetail(obs.FailureDetail),
		})), true
	default:
		// FailureNone and FailureNotProvider are never evidence.
		return EjectionRecord{}, false
	}
}

// successBanLocked judges a successful attempt for a structural fault, in the
// order the rules are declared: output cap, then tool corruption, then
// degeneracy. At most one ban per observation — the first rule that fires wins
// — and a key that already holds a live ejection is left alone.
//
// It also keeps the evidence counters: the successful-turn tally, the
// conversation's failure count (a success on the slug clears it), and the
// per-conversation structural counts. Caller holds g.mu.
func (g *ProviderGuard) successBanLocked(now time.Time, line string, obs Observation) (EjectionRecord, bool) {
	slug := g.dir.Slug(obs.Provider)
	key := providerKey{provider: slug, modelLine: line}
	g.turns[key]++
	h := g.healthLocked(now, obs.Conversation, slug)
	if h != nil {
		h.failures = 0
	}
	if obs.StopReason == "max_tokens" && obs.MaxTokens > 0 && obs.OutputTokens > 0 && 2*obs.OutputTokens <= obs.MaxTokens {
		// The provider stopped at no more than half the room it was given: it
		// truncated the answer, which no retry against the same provider fixes.
		if g.ejectedLiveLocked(now, key) {
			return EjectionRecord{}, false
		}
		return g.banLocked(now, key, ReasonOutputCap, structuralTTL, g.banEvidenceLocked(key, obs, map[string]any{
			"stop_reason":   obs.StopReason,
			"output_tokens": obs.OutputTokens,
			"max_tokens":    obs.MaxTokens,
		})), true
	}
	if h == nil {
		return EjectionRecord{}, false
	}
	if obs.ToolArgsInvalid > 0 {
		h.toolCorrupt++
		if h.toolCorrupt < structuralRepeatsToBan || g.ejectedLiveLocked(now, key) {
			return EjectionRecord{}, false
		}
		return g.banLocked(now, key, ReasonToolCorrupt, structuralTTL, g.banEvidenceLocked(key, obs, map[string]any{
			"tool_args_invalid": obs.ToolArgsInvalid,
			"count":             h.toolCorrupt,
		})), true
	}
	if obs.Repetition != nil && *obs.Repetition < repetitionDegenerate {
		// A nil Repetition means "not scored"; 0 would mean maximal repetition,
		// so the pointer is what separates the two.
		h.degenerate++
		if h.degenerate < structuralRepeatsToBan || g.ejectedLiveLocked(now, key) {
			return EjectionRecord{}, false
		}
		return g.banLocked(now, key, ReasonDegenerate, structuralTTL, g.banEvidenceLocked(key, obs, map[string]any{
			"repetition": *obs.Repetition,
			"count":      h.degenerate,
		})), true
	}
	return EjectionRecord{}, false
}

// clipDetail bounds a failure excerpt to failureDetailLimit bytes.
func clipDetail(s string) string {
	if len(s) <= failureDetailLimit {
		return s
	}
	return s[:failureDetailLimit]
}

// exclusionsLocked splits the live ejections that apply to line into operator
// bans (a human's choice, every model line) and automatic ejections (the
// guard's own, that line only). Each is sorted and deduplicated, a slug in both
// appears in operator only, and expired entries are pruned in passing. Caller
// holds g.mu.
func (g *ProviderGuard) exclusionsLocked(now time.Time, line string) (operator, automatic []string) {
	opSet := map[string]struct{}{}
	autoSet := map[string]struct{}{}
	for k, e := range g.ejected {
		if !e.live(now) {
			delete(g.ejected, k)
			continue
		}
		slug := g.dir.Slug(k.provider)
		if e.reason == ReasonOperator || k.modelLine == AllModelLines {
			opSet[slug] = struct{}{}
			continue
		}
		if k.modelLine == line {
			autoSet[slug] = struct{}{}
		}
	}
	operator = sortedSlugs(opSet)
	for _, slug := range operator {
		delete(autoSet, slug)
	}
	return operator, sortedSlugs(autoSet)
}

func sortedSlugs(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for slug := range set {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// ExclusionsFor splits IgnoredFor: operator holds slugs of ReasonOperator
// entries (every model line), automatic holds slugs of every other live entry
// on the model's line. Each is sorted and deduplicated; a slug present in both
// appears in operator only. Expired entries are pruned in passing. Nil-safe.
func (g *ProviderGuard) ExclusionsFor(now time.Time, model string) (operator, automatic []string) {
	if g == nil {
		return nil, nil
	}
	line := ModelLine(model)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exclusionsLocked(now, line)
}

// PinFor returns the provider slug that served the conversation's last
// successful turn on this model's line, when it is still routable: not in
// ExclusionsFor's operator or automatic lists. ok is false for a nil guard, an
// empty conversation, no remembered turn, a turn on another model line, or an
// excluded provider. The caller still checks the spec (AllowsPin) and any
// model-declaration pin.
//
// With observation off (RAFIKI_PROVIDER_GUARD=off) nothing is remembered, so
// this never pins and routing is exactly what it was before the guard.
func (g *ProviderGuard) PinFor(now time.Time, conversation, model string) (slug string, ok bool) {
	if g == nil || conversation == "" {
		return "", false
	}
	line := ModelLine(model)
	g.mu.Lock()
	defer g.mu.Unlock()
	lt, had := g.lastSeen[conversation]
	if !had || lt.provider == "" || lt.modelLine != line {
		return "", false
	}
	slug = g.dir.Slug(lt.provider)
	operator, automatic := g.exclusionsLocked(now, line)
	for _, s := range operator {
		if s == slug {
			return "", false
		}
	}
	for _, s := range automatic {
		if s == slug {
			return "", false
		}
	}
	return slug, true
}

// LiftLine removes every live automatic ejection (any reason except
// ReasonOperator) on the model's line — never an AllModelLines entry — and
// appends one ReasonLift record per removed key to the sink (5s timeout, WARN
// on error, the in-memory lift stands). Logs WARN with model_line, count and
// note. Returns the number lifted. Nil-safe (returns 0).
//
// This is the operator's answer to a line that has run out of providers: the
// guard's own ejections go, a human's bans stay.
func (g *ProviderGuard) LiftLine(ctx context.Context, now time.Time, model, note string) int {
	if g == nil {
		return 0
	}
	line := ModelLine(model)
	g.opMu.Lock()
	defer g.opMu.Unlock()
	g.mu.Lock()
	var keys []providerKey
	for k, e := range g.ejected {
		if k.modelLine != line || k.modelLine == AllModelLines {
			continue
		}
		if e.reason == ReasonOperator || !e.live(now) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].provider < keys[j].provider })
	for _, k := range keys {
		delete(g.ejected, k)
	}
	sink, logger := g.sink, g.logger
	g.mu.Unlock()
	for _, k := range keys {
		if sink == nil {
			break
		}
		rec := EjectionRecord{Provider: k.provider, ModelLine: line, Reason: ReasonLift, ExpiresAt: now, Note: note}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := sink.Append(wctx, rec)
		cancel()
		if err != nil {
			logger.Warn("routing: lift log append failed", "provider", k.provider, "error", err)
		}
	}
	logger.Warn("routing: automatic ejections lifted", "model_line", line, "count", len(keys), "note", note)
	return len(keys)
}
