// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/agentcli/local"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// reviewQueue is the daemon's bounded, per-conversation review-job queue.
// One item per conversation_id, never per request (design §4: a batch is N
// independent calls). capacity 8, drop-with-QUEUE_FULL beyond that.
//
// Everything on it is constructed once at startup and read-only after: pool,
// prov and catalog are set by newReviewQueue and never mutated. Only jobs,
// inFlight and the lazy LLM client below carry mutable state.
type reviewQueue struct {
	pool    *pgxpool.Pool
	prov    *providers.Set
	catalog *routing.ModelCatalog

	jobs     chan reviewJob
	mu       sync.Mutex
	inFlight map[string]bool // conversation_id -> true while queued or running

	// llmOnce/llmClient build the worker's LLM client on the first job that
	// needs one, not at startup: a daemon whose providers.toml carries no
	// usable credentials still serves every read verb, and an analyze run is
	// the only thing that would notice. Cached for the daemon's lifetime —
	// one client shares the catalog the proxy face warms.
	llmOnce   sync.Once
	llmClient *llm.Client
	llmErr    error
}

// reviewQueueCapacity bounds the queue. 8 is generous for the design's cost
// profile (§8: cents per conversation) — a full queue means the operator's
// operator ceiling (RAFIKI_REVIEW_MAX_BUDGET_USD) or single-flight is doing
// its job, and callers learn QUEUE_FULL rather than piling up unbounded work
// the worker can never get to.
const reviewQueueCapacity = 8

// reviewJob is one conversation's review. stage is always "detect" or
// "rank" — never "" (which would run the pipeline through draft; design §7).
// budgetUSD records the per-conversation spend ceiling the job was admitted
// under, already clamped against RAFIKI_REVIEW_MAX_BUDGET_USD: the analyze
// pipeline has no mid-run spend hook, so this bounds nothing at runtime —
// it is the admission decision, kept for the log line and for tests to pin
// the clamp's exact value. minTurns gates at DEQUEUE (design §6), not at
// accept: the count is read from the row when the job starts, so even an
// explicit request cheaply skips 1-turn probe children.
type reviewJob struct {
	conversationID string
	stage          string // "detect" | "rank"
	model          string
	profileName    string
	budgetUSD      float64
	minTurns       int32
	force          bool
}

func newReviewQueue(pool *pgxpool.Pool, prov *providers.Set, catalog *routing.ModelCatalog) *reviewQueue {
	return &reviewQueue{
		pool:     pool,
		prov:     prov,
		catalog:  catalog,
		jobs:     make(chan reviewJob, reviewQueueCapacity),
		inFlight: make(map[string]bool),
	}
}

// start launches the single worker goroutine. Call once, from Controller
// construction, threaded off the same baseCtx every other daemon-lifetime
// goroutine uses. A job pulled off the queue after ctx is cancelled still
// runs its cleanup (the single-flight entry is removed either way), but the
// select below makes no NEW pickup once the daemon is shutting down.
func (q *reviewQueue) start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-q.jobs:
				q.runJob(ctx, job)
			}
		}
	}()
}

// tryAccept attempts to admit one conversation_id. Exact order matters — see
// the steps. Never blocks: the send is select/default, so holding the lock
// across it cannot deadlock the worker (which takes the lock only in
// runJob's defer, after its receive).
func (q *reviewQueue) tryAccept(job reviewJob) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 1. Already queued or running for this conversation: visible conflict,
	// not a silent second job. The channel is untouched.
	if q.inFlight[job.conversationID] {
		return "already_running"
	}
	// 2. Attempt the send.
	select {
	case q.jobs <- job:
	default:
		// 3. Queue full. Do NOT set inFlight — a rejected job must never
		// leave the id looking in-flight, or every subsequent request for
		// that conversation gets a false "already_running" forever, with
		// nothing to ever clear it (there is no job running to eventually
		// remove it).
		return "queue_full"
	}
	// 4. Accepted: the single-flight entry is taken here, at accept time,
	// and leaves in runJob's defer. TestReviewQueueFullDoesNotLeaveTheID-
	// MarkedInFlight pins this ordering.
	q.inFlight[job.conversationID] = true
	return "enqueued"
}

// runJob is the worker's per-job body: the min-turns gate, profile
// resolution, the analyze run, and the single-flight cleanup that must
// happen on every path out. Tests call it directly (no worker started) to
// drive a job to completion deterministically.
func (q *reviewQueue) runJob(ctx context.Context, job reviewJob) {
	// Step 6 first in source order only — defers run last, which is the
	// point: the id leaves the single-flight set when the job COMPLETES
	// (success or failure), never while it is merely draining.
	defer func() {
		q.mu.Lock()
		delete(q.inFlight, job.conversationID)
		q.mu.Unlock()
	}()

	// Step 1: min-turns gate from the row's own turn count. A nil pool is
	// test-only (main.go constructs the queue only under pool != nil); the
	// gate simply cannot be evaluated there, so it is skipped rather than
	// failed — no fake evidence, same fail-safe direction ProviderGuard
	// follows.
	if q.pool != nil {
		turns, err := q.turnCount(ctx, job.conversationID)
		if err != nil {
			slog.Warn("conversation review: turn count failed; skipping job",
				"conversation", job.conversationID, "error", err)
			return
		}
		if turns < int64(job.minTurns) {
			slog.Info("conversation review: conversation below min_turns; skipping",
				"conversation", job.conversationID, "turns", turns, "minTurns", job.minTurns)
			// No analysis row is written for a skip — nothing ran.
			return
		}
	}

	// Step 2: resolve the analyzer profile. The request/env tiers were
	// resolved at accept time; the profile tier (detector model, population
	// filters, compact policy) resolves here, lazily, per job.
	profile, err := resolveProfile(os.Getenv("RAFIKI_ANALYZER_DIR"), job.profileName, job.model, true)
	if err != nil {
		slog.Warn("conversation review: analyzer profile failed to resolve; skipping job",
			"conversation", job.conversationID, "profile", job.profileName, "error", err)
		return
	}

	// Step 3: the LLM client, built once lazily. Mirrors fundi's
	// Config.clientOptions minus the child-specific escape hatches
	// (FakeTurns/APIKeyOverride/ProviderSenders) this worker has no use for.
	client, err := q.client()
	if err != nil {
		slog.Warn("conversation review: LLM client failed to build; skipping job",
			"conversation", job.conversationID, "error", err)
		return
	}

	// Step 4: run the pipeline, stopping after the job's stage. AnalyzedSet/
	// force skip logic lives inside Backend.Analyze — not re-derived here.
	// The stage is re-normalized here even though ConversationReview already
	// did: this is the one point that feeds StopAfter, and the constraint
	// (design §7) is StopAfter is never "" — draft must never run from this
	// verb, however the job was constructed. SkillFiles stays nil
	// deliberately: it is consumed only by the draft stage's skill-matching.
	stage := job.stage
	if stage != "rank" {
		stage = "detect"
	}
	backend := local.New(local.Options{Pool: q.pool, LLM: client, Pricer: client.Catalog().Pricing})
	events, err := backend.Analyze(ctx, agentcli.AnalyzeRequest{
		ConversationIDs: []string{job.conversationID},
		Profile:         profile,
		StopAfter:       stage,
		Force:           job.force,
		SkillFiles:      nil,
	})
	if err != nil {
		slog.Warn("conversation review: analyze refused the job",
			"conversation", job.conversationID, "error", err)
		return
	}

	// Step 5: drain to completion. An EventError is logged, never propagated:
	// the analysis row backing it is already marked status='failed' by
	// Backend.Analyze's own write path, and a review job can never fail the
	// close that asked for it (design §4).
	for ev := range events {
		if ev.Kind == agentcli.EventError && ev.Err != nil {
			slog.Warn("conversation review: analyze run failed (analysis row marked failed)",
				"conversation", job.conversationID, "error", ev.Err)
		}
	}
}

// turnCount reads one conversation's turn count — the single-row query the
// min-turns gate dequeues against. pkg/insights has no reusable
// per-conversation count (its turn counts are lateral aggregates inside
// search/stats SQL), so this is the small dedicated query design §6 asks
// for. An unparseable id surfaces as a query error and skips the job.
func (q *reviewQueue) turnCount(ctx context.Context, conversationID string) (int64, error) {
	var n int64
	err := q.pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_turn WHERE conversation_id = $1::uuid`,
		conversationID).Scan(&n)
	return n, err
}

// client builds the worker's shared LLM client on first use. WithProviders
// is the whole option list except one: the daemon's own catalog is passed
// through WithCatalog, because a client without one builds a SECOND
// OpenRouter fetcher with its own cache and TTL — the exact regression
// CLAUDE.md documents for daemon paths (the provider registry itself resolves
// where each provider's credentials come from).
func (q *reviewQueue) client() (*llm.Client, error) {
	q.llmOnce.Do(func() {
		opts := []llm.ClientOption{llm.WithProviders(q.prov)}
		if q.catalog != nil {
			opts = append(opts, llm.WithCatalog(q.catalog))
		}
		q.llmClient, q.llmErr = llm.NewClient(opts...)
	})
	return q.llmClient, q.llmErr
}

// envFloat reads one float-valued env var. ("", false) means unset — the
// caller's "fall through to the next tier" (design §3). An unparseable or
// negative value reads as unset too: a typoed number must not silently
// become a clamp of 0 (which would zero every review's ceiling) and an
// unparseable minimum must not gate everything.
func envFloat(name string) (float64, bool) {
	s := os.Getenv(name)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// envInt32 reads one int32-valued env var, same unset semantics as envFloat.
func envInt32(name string) (int32, bool) {
	s := os.Getenv(name)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil || v < 0 {
		return 0, false
	}
	return int32(v), true
}
