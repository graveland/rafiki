// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// healthModel is the model line every test in this file judges, matching the
// one miss/hit build.
const healthModel = "deepseek/deepseek-v4-pro"

// failed builds a failed attempt that was forced onto one provider. The
// response provider is left empty on purpose: failures are attributed ONLY
// through Pinned.
func failed(conv, pinned string, kind FailureKind) Observation {
	return Observation{
		Model:         healthModel,
		Conversation:  conv,
		Pinned:        pinned,
		Failed:        true,
		FailureKind:   kind,
		FailureDetail: "upstream 502",
	}
}

// truncated builds a successful turn the provider cut short at output tokens
// against a much larger request.
func truncated(conv, provider string, output, max int64) Observation {
	o := hit(conv, provider)
	o.StopReason = "max_tokens"
	o.OutputTokens = output
	o.MaxTokens = max
	return o
}

// score is a RepetitionScore pointer; nil means "not scored", and 0 would mean
// maximal repetition.
func score(v float64) *float64 { return &v }

// convID names the nth conversation of a loop.
func convID(i int) string { return string(rune('a' + i)) }

// TestPinnedTransientFailuresBanAtK proves the pinned-failure threshold: three
// consecutive failed pinned attempts in one conversation are a blip the agent
// loop's own backoff rides out, the fourth bans the provider for
// unavailableTTL.
func TestPinnedTransientFailuresBanAtK(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	for i := range pinnedFailuresToBan - 1 {
		g.Observe(now, failed("c1", "deepinfra", FailureTransient))
		c.Empty(g.IgnoredFor(now, healthModel), "banned after %d failure(s), want none yet", i+1)
	}
	g.Observe(now, failed("c1", "deepinfra", FailureTransient))
	c.EqDeep([]string{"deepinfra"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
	c.Require().Len(sink.recs, 1, "sink rows")
	rec := sink.recs[0]
	c.Eq(ReasonUnavailable, rec.Reason, "reason")
	c.True(rec.ExpiresAt.Equal(now.Add(unavailableTTL)), "ExpiresAt = %s, want %s", rec.ExpiresAt, now.Add(unavailableTTL))
}

// TestPinnedSuccessResetsFailures proves the count is CONSECUTIVE: a success on
// the same provider clears it, so an intermittently failing provider never
// accumulates its way to a ban.
func TestPinnedSuccessResetsFailures(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	for range pinnedFailuresToBan - 1 {
		g.Observe(now, failed("c1", "deepinfra", FailureTransient))
	}
	g.Observe(now, hit("c1", "DeepInfra"))
	for range pinnedFailuresToBan - 1 {
		g.Observe(now, failed("c1", "deepinfra", FailureTransient))
	}
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestFailureCountIsPerConversation proves two failures in each of two
// conversations are two failures, not four: the evidence is about one
// conversation's experience of one provider.
func TestFailureCountIsPerConversation(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	for _, conv := range []string{"c1", "c2"} {
		for range 2 {
			g.Observe(now, failed(conv, "deepinfra", FailureTransient))
		}
	}
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestUnpinnedFailureIsNoEvidence proves a failure with no forced provider
// blames nobody: OpenRouter chose, so the guard has no provider to judge.
func TestUnpinnedFailureIsNoEvidence(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	for range 10 {
		g.Observe(now, failed("c1", "", FailureTransient))
	}
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
	c.Empty(sink.recs, "sink rows")
}

// TestNotProviderAndNoneFailuresNeverBan proves the two kinds that are not
// evidence: our own fault (FailureNotProvider) and unclassified (the zero
// value), however many times they repeat.
func TestNotProviderAndNoneFailuresNeverBan(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	for range 10 {
		g.Observe(now, failed("c1", "deepinfra", FailureNotProvider))
	}
	unclassified := failed("c1", "deepinfra", FailureNone)
	unclassified.FailureKind = 0
	for range 10 {
		g.Observe(now, unclassified)
	}
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestPinnedNoEligibleBansAtOnce proves "nothing eligible" is a ban on the
// first attempt: the pinned provider is not serving the model at all, so no
// number of retries changes the answer.
func TestPinnedNoEligibleBansAtOnce(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	g.Observe(now, failed("c1", "deepinfra", FailureNoEligible))
	c.EqDeep([]string{"deepinfra"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
	c.Require().Len(sink.recs, 1, "sink rows")
	c.Eq(ReasonUnavailable, sink.recs[0].Reason, "reason")
	c.True(sink.recs[0].ExpiresAt.Equal(now.Add(unavailableTTL)), "ExpiresAt = %s", sink.recs[0].ExpiresAt)
}

// TestSlugChangeResetsConversationHealth proves the evidence is against the
// provider the conversation is CURRENTLY on: a turn on another provider starts
// the count again rather than adding to it.
func TestSlugChangeResetsConversationHealth(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	for range pinnedFailuresToBan - 1 {
		g.Observe(now, failed("c1", "deepinfra", FailureTransient))
	}
	g.Observe(now, failed("c1", "novita", FailureTransient))
	for range pinnedFailuresToBan - 1 {
		g.Observe(now, failed("c1", "deepinfra", FailureTransient))
	}
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestOutputCapBansAtOnce proves a truncated answer is a ban on the first
// turn, and that it needs no conversation: the provider was given 16384 tokens
// of room and stopped at 2048.
func TestOutputCapBansAtOnce(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	g.Observe(now, truncated("", "DeepInfra", 2048, 16384))
	c.EqDeep([]string{"deepinfra"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
	c.Require().Len(sink.recs, 1, "sink rows")
	rec := sink.recs[0]
	c.Eq(ReasonOutputCap, rec.Reason, "reason")
	c.True(rec.ExpiresAt.Equal(now.Add(structuralTTL)), "ExpiresAt = %s, want %s", rec.ExpiresAt, now.Add(structuralTTL))
}

// TestOutputCapNeedsTheMargin proves the rule's four guards: the stop must be
// max_tokens, both token counts must be known, and the output must be no more
// than half of what was asked for.
func TestOutputCapNeedsTheMargin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stop   string
		output int64
		max    int64
	}{
		{"more than half the room", "max_tokens", 9000, 16384},
		{"no output tokens reported", "max_tokens", 0, 16384},
		{"no max_tokens asked for", "max_tokens", 2048, 0},
		{"not a max_tokens stop", "tool_use", 2048, 16384},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			g := testGuard()
			now := time.Now()
			o := truncated("c1", "DeepInfra", tc.output, tc.max)
			o.StopReason = tc.stop
			g.Observe(now, o)
			c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
		})
	}
}

// TestToolCorruptionBansOnSecond proves one mangled tool call is noise and two
// in the same conversation is the provider — other turns between them do not
// reset the count.
func TestToolCorruptionBansOnSecond(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	bad := hit("c1", "DeepInfra")
	bad.ToolArgsInvalid = 2
	g.Observe(now, bad)
	c.Empty(g.IgnoredFor(now, healthModel), "banned on the first corrupt turn, want none yet")
	g.Observe(now, hit("c1", "DeepInfra"))
	g.Observe(now, bad)
	c.EqDeep([]string{"deepinfra"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
	c.Require().Len(sink.recs, 1, "sink rows")
	c.Eq(ReasonToolCorrupt, sink.recs[0].Reason, "reason")
}

// TestDegenerateBansOnSecond proves the same shape for repetition loops, and
// that an unscored turn (nil Repetition) never counts — 0 would mean maximal
// repetition, so the nil is what separates "not scored" from "a loop".
func TestDegenerateBansOnSecond(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	loop := hit("c1", "DeepInfra")
	loop.Repetition = score(0.01)
	g.Observe(now, loop)
	c.Empty(g.IgnoredFor(now, healthModel), "banned on the first loop, want none yet")
	clean := hit("c1", "DeepInfra")
	clean.Repetition = score(0.5)
	g.Observe(now, clean)
	loop.Repetition = score(0.15)
	g.Observe(now, loop)
	c.EqDeep([]string{"deepinfra"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
	c.Require().Len(sink.recs, 1, "sink rows")
	c.Eq(ReasonDegenerate, sink.recs[0].Reason, "reason")

	// Unscored turns are not evidence, however many there are.
	unscored := testGuard()
	for range 5 {
		unscored.Observe(now, hit("c2", "DeepInfra"))
	}
	c.Empty(unscored.IgnoredFor(now, healthModel), "unscored IgnoredFor")
}

// TestDegenerateNeedsTheSameConversation proves a loop in one conversation and
// a loop in another are two unrelated events, not the second repeat.
func TestDegenerateNeedsTheSameConversation(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	for _, conv := range []string{"c1", "c2"} {
		o := hit(conv, "DeepInfra")
		o.Repetition = score(0.01)
		g.Observe(now, o)
	}
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestBanEvidenceCarriesProviderTurns proves the evidence names the provider's
// track record on the line, so an operator can tell a provider that broke on
// its first turn from one that broke after ten good ones.
func TestBanEvidenceCarriesProviderTurns(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	for range 10 {
		g.Observe(now, hit("c1", "DeepInfra"))
	}
	g.Observe(now, truncated("c1", "DeepInfra", 2048, 16384))
	c.Require().Len(sink.recs, 1, "sink rows")
	rec := sink.recs[0]
	c.Eq(ReasonOutputCap, rec.Reason, "reason")
	var ev map[string]any
	c.Require().NoError(json.Unmarshal(rec.Evidence, &ev), "decode evidence")
	turns, isNum := ev["provider_turns_on_line"].(float64)
	c.True(isNum, "provider_turns_on_line is not a number in %s", rec.Evidence)
	c.Eq(float64(11), turns, "provider_turns_on_line in %s", rec.Evidence)
	conv, isStr := ev["conversation"].(string)
	c.True(isStr, "conversation is not a string in %s", rec.Evidence)
	c.Eq("c1", conv, "conversation in %s", rec.Evidence)
}

// TestBanSkippedWhenAlreadyEjected proves a provider already out for a live
// ban is not banned again: the second fault changes nothing, in memory or in
// the durable log.
func TestBanSkippedWhenAlreadyEjected(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	o := truncated("c1", "DeepInfra", 2048, 16384)
	g.Observe(now, o)
	g.Observe(now, o)
	c.Len(sink.recs, 1, "sink rows")
	c.EqDeep([]string{"deepinfra"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestObserveOffJudgesNothing proves RAFIKI_PROVIDER_GUARD=off restores today's
// routing: no automatic ban, and nothing remembered, so nothing is pinned
// either.
func TestObserveOffJudgesNothing(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	g.SetObserve(false)
	now := time.Now()
	for range pinnedFailuresToBan {
		g.Observe(now, failed("c1", "deepinfra", FailureTransient))
	}
	g.Observe(now, truncated("c1", "DeepInfra", 2048, 16384))
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor")
	slug, ok := g.PinFor(now, "c1", healthModel)
	c.False(ok, "PinFor = (%q, true), want no pin with observation off", slug)
}

// TestOneBanPerObservation proves a single observation records at most one
// ban, and that the structural rules are judged BEFORE the cache streak: the
// turn that trips both must be reported as the output cap, not as no_cache,
// and must not write two rows for one provider.
func TestOneBanPerObservation(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	// The first miss of a conversation is never evidence; the next four are a
	// streak of four, one short of the cache ban.
	for range missStreakToEject {
		g.Observe(now, miss("c1", "DeepInfra"))
	}
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor with a streak of four")
	both := miss("c1", "DeepInfra")
	both.StopReason = "max_tokens"
	both.OutputTokens = 2048
	both.MaxTokens = 16384
	g.Observe(now, both)
	c.Require().Len(sink.recs, 1, "sink rows")
	c.Eq(ReasonOutputCap, sink.recs[0].Reason, "reason")
	c.EqDeep([]string{"deepinfra"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestExclusionsForSplitsOperatorFromAutomatic proves the split callers need to
// tell "a human said no" from "the guard said not now": operator holds every
// line's bans, automatic holds the model's own line only.
func TestExclusionsForSplitsOperatorFromAutomatic(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "zulu", 0, "")
	c.Require().NoError(err, "Ban")
	g.Observe(now, truncated("c1", "DeepInfra", 2048, 16384))

	operator, automatic := g.ExclusionsFor(now, healthModel)
	c.EqDeep([]string{"zulu"}, operator, "operator")
	c.EqDeep([]string{"deepinfra"}, automatic, "automatic")

	operator, automatic = g.ExclusionsFor(now, "z-ai/glm-5.3-flash")
	c.EqDeep([]string{"zulu"}, operator, "operator on another line")
	c.Empty(automatic, "automatic on another line")
}

// TestPinForReturnsLastServedSlug proves pinning follows the provider that
// actually served the conversation's last turn, and only on the same model
// line.
func TestPinForReturnsLastServedSlug(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	now := time.Now()
	g.Observe(now, hit("c1", "DeepInfra"))
	slug, ok := g.PinFor(now, "c1", healthModel)
	c.True(ok, "PinFor(c1) = (%q, false)", slug)
	c.Eq("deepinfra", slug, "slug")
	_, ok = g.PinFor(now, "c2", healthModel)
	c.False(ok, "PinFor of a conversation with no turn succeeded")
	_, ok = g.PinFor(now, "c1", "z-ai/glm-5.3-flash")
	c.False(ok, "PinFor on another model line succeeded")
	_, ok = g.PinFor(now, "", healthModel)
	c.False(ok, "PinFor with no conversation succeeded")
	var nilGuard *ProviderGuard
	_, ok = nilGuard.PinFor(now, "c1", healthModel)
	c.False(ok, "PinFor on a nil guard succeeded")
}

// TestPinForRefusesExcludedProvider proves pinning never routes back to a
// provider that is out, whether a human took it out or the guard did.
func TestPinForRefusesExcludedProvider(t *testing.T) {
	c := assert.NewAborting(t)
	now := time.Now()

	operatorBanned := testGuard()
	operatorBanned.Observe(now, hit("c1", "DeepInfra"))
	_, err := operatorBanned.Ban(context.Background(), now, "deepinfra", 0, "")
	c.Require().NoError(err, "Ban")
	_, ok := operatorBanned.PinFor(now, "c1", healthModel)
	c.False(ok, "PinFor of an operator-banned provider succeeded")

	autoBanned := testGuard()
	autoBanned.Observe(now, truncated("c1", "DeepInfra", 2048, 16384))
	_, ok = autoBanned.PinFor(now, "c1", healthModel)
	c.False(ok, "PinFor of an automatically ejected provider succeeded")
}

// TestLiftLineRemovesOnlyAutomaticOnThatLine proves LiftLine's scope: the
// guard's own ejections on the named line go, a human's ban stays, and other
// lines are untouched.
func TestLiftLineRemovesOnlyAutomaticOnThatLine(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "zulu", 0, "")
	c.Require().NoError(err, "Ban")
	g.Observe(now, truncated("c1", "DeepInfra", 2048, 16384))
	const otherLine = "z-ai/glm-5.3-flash"
	for range missStreakToEject + 1 {
		o := miss("c2", "CoreWeave")
		o.Model = otherLine
		g.Observe(now, o)
	}
	c.EqDeep([]string{"deepinfra", "zulu"}, g.IgnoredFor(now, healthModel), "line A before the lift")
	c.EqDeep([]string{"coreweave", "zulu"}, g.IgnoredFor(now, otherLine), "line B before the lift")

	n := g.LiftLine(context.Background(), now, healthModel, "line exhausted")
	c.Eq(1, n, "LiftLine lifted %d ejections, want 1", n)
	c.EqDeep([]string{"zulu"}, g.IgnoredFor(now, healthModel), "line A after the lift")
	c.EqDeep([]string{"coreweave", "zulu"}, g.IgnoredFor(now, otherLine), "line B after the lift")

	var lifts []EjectionRecord
	for _, r := range sink.recs {
		if r.Reason == ReasonLift {
			lifts = append(lifts, r)
		}
	}
	c.Require().Len(lifts, 1, "lift rows")
	c.Eq("deepinfra", lifts[0].Provider, "lift provider")
	c.Eq(healthModel, lifts[0].ModelLine, "lift model line")
	c.Eq("line exhausted", lifts[0].Note, "lift note")
}

// TestLiftLineWritesOneRowPerKey proves the lift is recorded per provider, so
// the durable log shows exactly what came back into routing.
func TestLiftLineWritesOneRowPerKey(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	for i, p := range []string{"Alpha", "Bravo"} {
		for range missStreakToEject + 1 {
			g.Observe(now, miss(convID(i), p))
		}
	}
	c.Require().Len(g.IgnoredFor(now, healthModel), 2, "ejections before the lift")
	c.Eq(2, g.LiftLine(context.Background(), now, healthModel, "both gone"), "lifted")
	c.Empty(g.IgnoredFor(now, healthModel), "IgnoredFor after the lift")
	var lifts []EjectionRecord
	for _, r := range sink.recs {
		if r.Reason == ReasonLift {
			lifts = append(lifts, r)
		}
	}
	c.Len(lifts, 2, "lift rows")
	c.EqDeep([]string{"Alpha", "Bravo"}, []string{lifts[0].Provider, lifts[1].Provider}, "lifted providers")
}

// TestRehydrateSkipsUnavailable proves the short outage ban is memory-only: a
// restart gives a provider a fresh chance at a capacity problem, while the
// structural and operator bans come back.
func TestRehydrateSkipsUnavailable(t *testing.T) {
	c := assert.NewAborting(t)
	now := time.Now()
	sink := &fakeSink{active: []EjectionRecord{
		{Provider: "deepinfra", ModelLine: healthModel, Reason: ReasonUnavailable, ExpiresAt: now.Add(unavailableTTL)},
		{Provider: "novita", ModelLine: healthModel, Reason: ReasonDegenerate, ExpiresAt: now.Add(structuralTTL)},
		{Provider: "zulu", ModelLine: AllModelLines, Reason: ReasonOperator},
	}}
	g := testGuard()
	g.SetSink(sink)
	g.Rehydrate(context.Background(), now)
	got := g.Ejected(now)
	c.Require().Len(got, 2, "rehydrated ejections")
	// Ejected sorts by model line, and an operator ban's line is AllModelLines,
	// which sorts before every real one.
	c.Eq("zulu", got[0].Provider, "first rehydrated provider")
	c.Eq(ReasonOperator, got[0].Reason, "first rehydrated reason")
	c.Eq("novita", got[1].Provider, "second rehydrated provider")
	c.Eq(ReasonDegenerate, got[1].Reason, "second rehydrated reason")
}

// TestNoCapOnAutomaticEjections proves a line can lose every provider it has:
// the guard no longer declines an ejection to keep a line routable.
func TestNoCapOnAutomaticEjections(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	providers := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}
	for i, p := range providers {
		for range missStreakToEject + 1 {
			g.Observe(now, miss(convID(i), p))
		}
	}
	c.EqDeep([]string{"alpha", "bravo", "charlie", "delta", "echo"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestBanSurvivesLineLift proves lifting a line's automatic ejections is not a
// way around a human's ban: only the guard's own ejections go.
func TestBanSurvivesLineLift(t *testing.T) {
	c := assert.NewAborting(t)
	g := testGuard()
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "zulu", 0, "")
	c.Require().NoError(err, "Ban")
	for i, p := range []string{"Alpha", "Bravo", "Charlie"} {
		for range missStreakToEject + 1 {
			g.Observe(now, miss(convID(i), p))
		}
	}
	c.Eq(3, g.LiftLine(context.Background(), now, healthModel, "clean slate"), "lifted")
	c.EqDeep([]string{"zulu"}, g.IgnoredFor(now, healthModel), "IgnoredFor")
}

// TestLiftedBanIsNotPersisted proves the durable log never records a ban the
// guard has already lifted. The log is latest-row-wins per (provider, model
// line) and seeds Rehydrate, so a ban row appended after LiftLine's lift row
// would resurrect — for a structural ban, for 24h — a ban the exhaustion
// breaker had deliberately cleared. The interleaving is reproduced without
// concurrency: the ban is recorded in memory, the line is lifted, and only then
// is the ban reported.
func TestLiftedBanIsNotPersisted(t *testing.T) {
	c := assert.NewAborting(t)
	now := time.Now()

	// Positive control: an unreported ban that still stands IS persisted, so the
	// assertions below cannot pass by never writing anything.
	standing := testGuard()
	standingSink := &fakeSink{}
	standing.SetSink(standingSink)
	standing.mu.Lock()
	rec := standing.banLocked(now, providerKey{provider: "deepinfra", modelLine: healthModel}, ReasonOutputCap, structuralTTL, nil)
	standing.mu.Unlock()
	standing.reportBan(rec, "c1")
	c.Require().Len(standingSink.recs, 1, "a standing ban must reach the sink")
	c.Eq(ReasonOutputCap, standingSink.recs[0].Reason, "reason")

	// The interleaving: recorded in memory, lifted, then reported.
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	g.mu.Lock()
	rec = g.banLocked(now, providerKey{provider: "deepinfra", modelLine: healthModel}, ReasonOutputCap, structuralTTL, nil)
	g.mu.Unlock()
	c.Eq(1, g.LiftLine(context.Background(), now, healthModel, "line exhausted"), "LiftLine count")
	g.reportBan(rec, "c1")

	c.Require().Len(sink.recs, 1, "sink rows: the lift only")
	c.Eq(ReasonLift, sink.recs[0].Reason, "a lifted ban must not be persisted after the lift")
}

// blockingSink parks its first Append until released, so a test can drive a
// second guard operation while a ban is mid-append.
type blockingSink struct {
	entered chan struct{}
	release chan struct{}
	n       atomic.Int32
	mu      sync.Mutex
	recs    []EjectionRecord
}

func (s *blockingSink) Append(_ context.Context, e EjectionRecord) error {
	// Only the FIRST append parks; later ones must not queue behind it, or the
	// test would serialise the guard's two appends by itself.
	if s.n.Add(1) == 1 {
		close(s.entered)
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, e)
	return nil
}

func (s *blockingSink) Active(context.Context, time.Time) ([]EjectionRecord, error) {
	return nil, nil
}

// TestBanReportAndLiftAreSerialised proves the durable log and the map cannot
// disagree: reporting a ban and lifting a line take the same lock (opMu), so a
// lift can never slip between a ban's "still standing" check and its append.
// Without that, a lift during the append leaves the ban row LAST — and the log
// is latest-row-wins, so Rehydrate would restore a ban the guard had lifted.
func TestBanReportAndLiftAreSerialised(t *testing.T) {
	c := assert.NewAborting(t)
	now := time.Now()
	key := providerKey{provider: "deepinfra", modelLine: healthModel}

	g := testGuard()
	sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	g.SetSink(sink)

	g.mu.Lock()
	rec := g.banLocked(now, key, ReasonOutputCap, structuralTTL, nil)
	g.mu.Unlock()

	reported := make(chan struct{})
	go func() {
		g.reportBan(rec, "c1")
		close(reported)
	}()
	<-sink.entered

	lifted := make(chan struct{})
	var liftedCount int
	go func() {
		liftedCount = g.LiftLine(context.Background(), now, healthModel, "line exhausted")
		close(lifted)
	}()

	// The lift either waits for the in-flight append (correct) or runs during it
	// (wrong, and caught by the ordering assertion below). Waiting here rather
	// than failing keeps the broken case from deadlocking the test.
	select {
	case <-lifted:
	case <-time.After(100 * time.Millisecond):
	}
	close(sink.release)
	<-reported
	<-lifted

	sink.mu.Lock()
	recs := append([]EjectionRecord(nil), sink.recs...)
	sink.mu.Unlock()

	c.Require().Len(recs, 2, "sink rows")
	c.Eq(ReasonOutputCap, recs[0].Reason, "the ban is appended before the lift")
	c.Eq(ReasonLift, recs[1].Reason, "the lift is the last row for the key, so Rehydrate stays lifted")
	c.Eq(1, liftedCount, "lifted count")
	_, still := g.ejected[key]
	c.False(still, "the lift removed the ban from memory")
}

// TestPinForRefusesAnUnloadedDirectory pins the design's "the slug resolves"
// rule: a configured directory that has not finished its first load answers no
// pin, because only= silently drops a slug OpenRouter does not know — a guess
// would buy one request against nothing and then ban a provider that does not
// exist.
func TestPinForRefusesAnUnloadedDirectory(t *testing.T) {
	c := assert.NewAborting(t)
	now := time.Now()

	g := testGuard()
	dir := NewProviderDirectory(http.DefaultClient, slog.New(slog.DiscardHandler))
	dir.mu.Lock()
	dir.lastFail = now // inside the failure backoff: no fetch is attempted
	dir.mu.Unlock()
	g.SetDirectory(dir)

	g.Observe(now, hit("c1", "DeepInfra"))
	_, ok := g.PinFor(now, "c1", healthModel)
	c.False(ok, "an unloaded directory must not pin a guessed slug")

	// Once the directory has loaded and lists the provider, the pin resumes.
	dir.mu.Lock()
	dir.fetched = now
	dir.slugs = map[string]string{"deepinfra": "deepinfra"}
	dir.mu.Unlock()

	slug, ok := g.PinFor(now, "c1", healthModel)
	c.True(ok, "a loaded directory pins")
	c.Eq("deepinfra", slug, "slug")
}
