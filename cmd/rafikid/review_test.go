// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// fakeReviewReads stands in for *insights.Insights on the review verbs'
// accept path: FilterByScope answers with a fixed admitted set, so
// ConversationReview's scope, clamp and model-validation behavior is
// testable without a database. canonical is the parallel canonical list the
// real FilterByScope returns; when unset, the inputs are their own canonical
// (the uuid inputs the older tests use).
type fakeReviewReads struct {
	ids       []string
	canonical []string
	findings  []insights.Finding
	analyses  []insights.AnalysisRow
}

func (f fakeReviewReads) FilterByScope(_ context.Context, _ insights.Scope, _ []string) ([]string, []string, error) {
	if f.canonical == nil {
		return f.ids, f.ids, nil
	}
	return f.canonical, f.ids, nil
}

func (f fakeReviewReads) RecentAnalyses(_ context.Context, _ insights.Scope, _ []string, _ int) ([]insights.AnalysisRow, error) {
	return f.analyses, nil
}

func (f fakeReviewReads) Findings(_ context.Context, _ insights.Scope, _ insights.FindingsFilter) ([]insights.Finding, error) {
	return f.findings, nil
}

var _ reviewReads = fakeReviewReads{}

// reviewTestController builds a Controller for the review verbs with the
// given read surface and a queue over a nil pool (tests never start the
// worker; a job reaching the channel is the assertion). Every
// RAFIKI_REVIEW_* variable is blanked first: this package's TestMain
// isolates XDG_CONFIG_HOME but not these, and a developer's exported
// RAFIKI_REVIEW_MODEL must not decide what a defaulting test observes.
func reviewTestController(t *testing.T, reads reviewReads) *Controller {
	t.Helper()
	t.Setenv("RAFIKI_REVIEW_MODEL", "")
	t.Setenv("RAFIKI_REVIEW_BUDGET_USD", "")
	t.Setenv("RAFIKI_REVIEW_MIN_TURNS", "")
	t.Setenv("RAFIKI_REVIEW_PROFILE", "")
	t.Setenv("RAFIKI_REVIEW_MAX_BUDGET_USD", "")
	c := &Controller{}
	c.reviewQ = newReviewQueue(nil, providers.Default(), nil)
	c.reviewInsights = reads
	return c
}

func TestReviewQueueTryAcceptEnqueuesOncePerConversation(t *testing.T) {
	q := newReviewQueue(nil, nil, nil)
	for i := 0; i < 8; i++ {
		id := string(rune('a' + i))
		if st := q.tryAccept(reviewJob{conversationID: id, stage: "detect"}); st != "enqueued" {
			t.Fatalf("tryAccept(%q) = %q, want enqueued", id, st)
		}
	}
	if len(q.jobs) != 8 {
		t.Fatalf("queue holds %d jobs, want 8", len(q.jobs))
	}
	// Once per conversation: each id appears exactly once in the channel.
	seen := map[string]int{}
	for i := 0; i < 8; i++ {
		job := <-q.jobs
		seen[job.conversationID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("conversation %q enqueued %d times, want 1", id, n)
		}
	}
}

// Two near-simultaneous requests for the same conversation: the second is a
// visible "already happening", never a second job. The first job must still
// be the only one in the channel — the guard rejects without enqueueing.
func TestReviewQueueSecondRequestForSameConversationGetsAlreadyRunning(t *testing.T) {
	q := newReviewQueue(nil, nil, nil)
	if st := q.tryAccept(reviewJob{conversationID: "conv-1", stage: "detect"}); st != "enqueued" {
		t.Fatalf("first tryAccept = %q, want enqueued", st)
	}
	st := q.tryAccept(reviewJob{conversationID: "conv-1", stage: "detect"})
	if st != "already_running" {
		t.Fatalf("second tryAccept = %q, want already_running", st)
	}
	if len(q.jobs) != 1 {
		t.Fatalf("queue holds %d jobs after a rejected duplicate, want 1", len(q.jobs))
	}
}

// A full queue must refuse without blocking: no worker is draining it, so a
// blocking send would hang the review verb (design §2: the verb never blocks
// regardless of outcome). The timeout is the assertion — no sleep-based
// "probably returned in time" check.
func TestReviewQueueFullReturnsQueueFullWithoutBlocking(t *testing.T) {
	q := newReviewQueue(nil, nil, nil)
	for i := 0; i < reviewQueueCapacity; i++ {
		id := string(rune('a' + i))
		if st := q.tryAccept(reviewJob{conversationID: id, stage: "detect"}); st != "enqueued" {
			t.Fatalf("tryAccept(%q) = %q, want enqueued", id, st)
		}
	}
	done := make(chan string, 1)
	go func() { done <- q.tryAccept(reviewJob{conversationID: "ninth", stage: "detect"}) }()
	select {
	case st := <-done:
		if st != "queue_full" {
			t.Fatalf("9th tryAccept = %q, want queue_full", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tryAccept blocked on a full queue")
	}
}

// The regression test for tryAccept's exact ordering: a queue_full response
// must never mark the id in-flight, or every later request for that id gets
// a false already_running forever (nothing running to clear it). Drain one
// slot to make room and prove the id is still admissible.
func TestReviewQueueFullDoesNotLeaveTheIDMarkedInFlight(t *testing.T) {
	q := newReviewQueue(nil, nil, nil)
	for i := 0; i < reviewQueueCapacity; i++ {
		id := string(rune('a' + i))
		q.tryAccept(reviewJob{conversationID: id, stage: "detect"})
	}
	if st := q.tryAccept(reviewJob{conversationID: "X", stage: "detect"}); st != "queue_full" {
		t.Fatalf("tryAccept(X) = %q, want queue_full", st)
	}
	<-q.jobs // make room
	if st := q.tryAccept(reviewJob{conversationID: "X", stage: "detect"}); st != "enqueued" {
		t.Fatalf("tryAccept(X) after a drain = %q, want enqueued (not already_running)", st)
	}
}

// The single-flight entry leaves when the job completes — success or
// failure — so a subsequent request for the same conversation is admissible
// again. The job here FAILS fast (no analyzer dir), which is the
// failure-shaped half of that promise; runJob is invoked directly, the same
// body the worker loop calls, so no goroutine timing is involved.
func TestReviewQueueInFlightClearsOnJobCompletion(t *testing.T) {
	// An absent analyzer dir fails the job at profile resolution, before
	// anything else runs. Setting it (rather than leaving the env alone)
	// also keeps resolveProfile's auto-seed path from writing into the
	// developer's real profile directory.
	t.Setenv("RAFIKI_ANALYZER_DIR", filepath.Join(t.TempDir(), "absent"))
	q := newReviewQueue(nil, nil, nil)
	job := reviewJob{conversationID: "conv-9", stage: "detect"}
	if st := q.tryAccept(job); st != "enqueued" {
		t.Fatalf("tryAccept = %q, want enqueued", st)
	}
	q.runJob(context.Background(), job)
	q.mu.Lock()
	stillIn := q.inFlight["conv-9"]
	q.mu.Unlock()
	if stillIn {
		t.Fatal("inFlight still holds the id after the job completed (failed)")
	}
	if st := q.tryAccept(job); st != "enqueued" {
		t.Fatalf("tryAccept after completion = %q, want enqueued (not already_running)", st)
	}
}

func TestConversationReviewClampsBudgetToMaxEnv(t *testing.T) {
	conv := "00000000-0000-0000-0000-0000000000aa"
	t.Run("request above max clamps down", func(t *testing.T) {
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		// After the helper: it blanks every RAFIKI_REVIEW_* var first, so a
		// value set before it would be erased.
		t.Setenv("RAFIKI_REVIEW_MAX_BUDGET_USD", "1.5")
		acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			BudgetUSD: 5, HasBudgetUSD: true,
		})
		if err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		if len(acc) != 1 || acc[0].Status != "enqueued" {
			t.Fatalf("accepts = %+v, want one enqueued", acc)
		}
		job := <-c.reviewQ.jobs
		if job.budgetUSD != 1.5 {
			t.Fatalf("job.budgetUSD = %v, want the clamp 1.5", job.budgetUSD)
		}
	})
	t.Run("unset budget takes the max", func(t *testing.T) {
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		t.Setenv("RAFIKI_REVIEW_MAX_BUDGET_USD", "0.25")
		if _, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
		}); err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		job := <-c.reviewQ.jobs
		if job.budgetUSD != 0.25 {
			t.Fatalf("job.budgetUSD = %v, want 0.25 (no request ceiling + max set)", job.budgetUSD)
		}
	})
	t.Run("request below max is untouched", func(t *testing.T) {
		// Downward only: the clamp must never raise a request's own ceiling.
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		t.Setenv("RAFIKI_REVIEW_MAX_BUDGET_USD", "1.5")
		if _, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			BudgetUSD: 0.1, HasBudgetUSD: true,
		}); err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		job := <-c.reviewQ.jobs
		if job.budgetUSD != 0.1 {
			t.Fatalf("job.budgetUSD = %v, want the request's own 0.1", job.budgetUSD)
		}
	})
}

// The single-flight key and the queued job use the CANONICAL conversation
// uuid FilterByScope returns, while the response echoes the CALLER's
// spelling: the worker's turn count and analysis write run against the
// conversation row (a child id would query nothing), and the caller sees its
// own spelling back.
func TestConversationReviewQueuesCanonicalEchoesCallerSpelling(t *testing.T) {
	conv := "00000000-0000-0000-0000-0000000000dd"
	child := "c_reviewchild"
	c := reviewTestController(t, fakeReviewReads{ids: []string{child}, canonical: []string{conv}})
	acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
		ConversationIDs: []string{child}, Stage: "detect",
	})
	if err != nil {
		t.Fatalf("ConversationReview: %v", err)
	}
	if len(acc) != 1 || acc[0].Status != "enqueued" {
		t.Fatalf("accepts = %+v, want one enqueued", acc)
	}
	if acc[0].ConversationID != child {
		t.Fatalf("accept echoes %q, want the CALLER's spelling %q", acc[0].ConversationID, child)
	}
	job := <-c.reviewQ.jobs
	if job.conversationID != conv {
		t.Fatalf("job.conversationID = %q, want the canonical %q", job.conversationID, conv)
	}
}

// Two spellings of one conversation -- its child id and its uuid --
// canonicalize to the same value, so the second spelling collides in the
// single-flight map: already_running, never a second job.
func TestConversationReviewTwoSpellingsCollideInFlight(t *testing.T) {
	conv := "00000000-0000-0000-0000-0000000000ee"
	child := "c_reviewchild2"
	c := reviewTestController(t, fakeReviewReads{
		ids:       []string{child, conv},
		canonical: []string{conv, conv},
	})
	acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
		ConversationIDs: []string{child, conv}, Stage: "detect",
	})
	if err != nil {
		t.Fatalf("ConversationReview: %v", err)
	}
	if len(acc) != 2 {
		t.Fatalf("accepts = %+v, want two", acc)
	}
	if acc[0].ConversationID != child || acc[0].Status != "enqueued" {
		t.Fatalf("accept[0] = %+v, want %q enqueued", acc[0], child)
	}
	if acc[1].ConversationID != conv || acc[1].Status != "already_running" {
		t.Fatalf("accept[1] = %+v, want %q already_running", acc[1], conv)
	}
	if len(c.reviewQ.jobs) != 1 {
		t.Fatalf("queue holds %d jobs, want 1 (the canonical key collided)", len(c.reviewQ.jobs))
	}
}

// A negative budget_usd is malformed, not "spend nothing": 0 is the
// no-ceiling value, so the negative must fold to 0 at the clamp point
// instead of landing on the job. The max-env logic then applies to the
// folded value as for any unrequested ceiling.
func TestConversationReviewNegativeBudgetIsNoCeiling(t *testing.T) {
	conv := "00000000-0000-0000-0000-0000000000ff"
	t.Run("without max env stays 0", func(t *testing.T) {
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			BudgetUSD: -0.5, HasBudgetUSD: true,
		})
		if err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		if len(acc) != 1 || acc[0].Status != "enqueued" {
			t.Fatalf("accepts = %+v, want one enqueued", acc)
		}
		if job := <-c.reviewQ.jobs; job.budgetUSD != 0 {
			t.Fatalf("job.budgetUSD = %v, want 0 (negative folded to no ceiling)", job.budgetUSD)
		}
	})
	t.Run("with max env takes the max", func(t *testing.T) {
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		t.Setenv("RAFIKI_REVIEW_MAX_BUDGET_USD", "0.25")
		if _, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			BudgetUSD: -0.5, HasBudgetUSD: true,
		}); err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		if job := <-c.reviewQ.jobs; job.budgetUSD != 0.25 {
			t.Fatalf("job.budgetUSD = %v, want 0.25 (folded to 0, then the max)", job.budgetUSD)
		}
	})
}

// A mixed batch folds the out-of-scope id out of the response entirely —
// never a distinct status naming it (constraints: a scope miss reads exactly
// like not-found, and a status would leak that the id exists).
func TestConversationReviewDropsOutOfScopeIDSilently(t *testing.T) {
	inScope := "00000000-0000-0000-0000-000000000001"
	outScope := "00000000-0000-0000-0000-000000000002"
	c := reviewTestController(t, fakeReviewReads{ids: []string{inScope}})
	acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
		ConversationIDs: []string{outScope, inScope},
		Stage:           "detect",
	})
	if err != nil {
		t.Fatalf("ConversationReview: %v", err)
	}
	if len(acc) != 1 {
		t.Fatalf("accepts = %+v, want exactly one", acc)
	}
	if acc[0].ConversationID != inScope || acc[0].Status != "enqueued" {
		t.Fatalf("accept[0] = %+v, want %q enqueued", acc[0], inScope)
	}
}

// A model id that resolves nowhere fails the WHOLE request before anything
// is enqueued (design §6) — not a per-id status, and not a mid-run
// per-conversation error. The controller here has providers set (the queue's
// default registry) and an empty catalog: "openrouter/…" splits fine but
// resolves to no catalog entry.
func TestConversationReviewUnknownModelFailsWholeRequest(t *testing.T) {
	conv := "00000000-0000-0000-0000-0000000000bb"
	c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
	c.SetCatalog(seedTestCatalog(t, nil))
	acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
		ConversationIDs: []string{conv}, Stage: "detect",
		Model: "openrouter/definitely/not-a-model",
	})
	if err == nil {
		t.Fatalf("ConversationReview accepted an unresolvable model; accepts = %+v", acc)
	}
	if len(c.reviewQ.jobs) != 0 {
		t.Fatalf("a whole-request failure must enqueue nothing; queue holds %d", len(c.reviewQ.jobs))
	}
}

// The positive control for reviewModelResolves. Real catalog keys are bare
// OpenRouter ids — "z-ai/glm-5.3-flash", "anthropic/claude-sonnet-5" — so
// the seeds use that shape, never a qualified "openrouter/<id>" key (a shape
// real catalogs never carry; seeding it once masked the resolver bug where
// the qualified request string was resolved against the catalog and always
// missed). Both spellings a caller can actually send are pinned: the
// qualified spelling (what ListModelRows/agent_models/the picker offer) and
// the bare Anthropic spelling (resolves against DefaultProvider inside
// Split, then OpenRouterModel maps it onto its "anthropic/" entry).
func TestConversationReviewKnownModelPassesValidation(t *testing.T) {
	conv := "00000000-0000-0000-0000-0000000000cc"
	catalog := func(t *testing.T) *routing.ModelCatalog {
		return seedTestCatalog(t, map[string]int{
			"z-ai/glm-5.3-flash":        200000,
			"anthropic/claude-sonnet-5": 200000,
		})
	}
	t.Run("catalogued id, qualified spelling", func(t *testing.T) {
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		c.SetCatalog(catalog(t))
		acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			Model: "openrouter/z-ai/glm-5.3-flash",
		})
		if err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		if len(acc) != 1 || acc[0].Status != "enqueued" {
			t.Fatalf("accepts = %+v, want one enqueued", acc)
		}
	})
	t.Run("bare anthropic id", func(t *testing.T) {
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		c.SetCatalog(catalog(t))
		acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			Model: "claude-sonnet-5",
		})
		if err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		if len(acc) != 1 || acc[0].Status != "enqueued" {
			t.Fatalf("accepts = %+v, want one enqueued", acc)
		}
	})
	t.Run("bare openrouter id is refused", func(t *testing.T) {
		// The addressing rule: a first segment that names no configured
		// provider is an error, never a fallthrough to DefaultProvider —
		// so the catalog key's own spelling cannot ride in unqualified.
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		c.SetCatalog(catalog(t))
		_, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			Model: "z-ai/glm-5.3-flash",
		})
		if err == nil {
			t.Fatal("an unqualified openrouter id must fail the whole request")
		}
	})
	t.Run("registry alias without a catalog", func(t *testing.T) {
		c := reviewTestController(t, fakeReviewReads{ids: []string{conv}})
		c.providers = mustParseProviders(t, `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B"
context_window = 16384
`)
		acc, err := c.ConversationReview(context.Background(), insights.ScopeAll(), connectapi.ReviewRequest{
			ConversationIDs: []string{conv}, Stage: "detect",
			Model: "vmlx/qwen",
		})
		if err != nil {
			t.Fatalf("ConversationReview: %v", err)
		}
		if len(acc) != 1 || acc[0].Status != "enqueued" {
			t.Fatalf("accepts = %+v, want one enqueued", acc)
		}
	})
}

// TestReviewAcceptancePathsMatchTheVerifyPattern exists because the plan's
// verify command selects with -run TestReview, an unanchored substring
// match, and the four acceptance-path tests above carry pinned
// TestConversationReview* names the pattern does not match. It runs the same
// bodies so the pinned verify exercises them anyway; under a full -run .
// suite each body then runs twice (here and under its pinned name), which is
// inert — the bodies build fresh controllers and t.Setenv scopes env to the
// caller. Without this shim the clamp, scope-fold and model fast-fail paths
// ship with a verify command that never runs their tests.
func TestReviewAcceptancePathsMatchTheVerifyPattern(t *testing.T) {
	t.Run("clamps budget to max env", TestConversationReviewClampsBudgetToMaxEnv)
	t.Run("drops out-of-scope ids silently", TestConversationReviewDropsOutOfScopeIDSilently)
	t.Run("unknown model fails whole request", TestConversationReviewUnknownModelFailsWholeRequest)
	t.Run("known model passes validation", TestConversationReviewKnownModelPassesValidation)
}
