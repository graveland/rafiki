// Package local implements agentcli.Backend directly against a Postgres
// pool: no gRPC, no auth. It is the DSN-backed backend the CLI runs against
// today; a gRPC one lives in savannah-client later.
package local

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timescale/rafiki/agentcli"
	"github.com/timescale/rafiki/analyze"
	"github.com/timescale/rafiki/insights"
	"github.com/timescale/rafiki/llm"
	"github.com/timescale/rafiki/store"
)

// ErrNoLLM is returned by Analyze when no *llm.Client was configured and the
// run needs one (every stage past compact). The
// read paths work without one, but analysis needs a model to call.
var ErrNoLLM = errors.New("agentcli/local: no LLM client configured")

// ErrNoPool is returned by read/findings methods when no *pgxpool.Pool was
// configured (corpus-only runs). Corpus Analyze does not require a pool.
var ErrNoPool = errors.New("agentcli/local: no database pool configured (corpus-only backend)")

// defaultOwner attributes conversations run through this backend when
// Options.Owner is empty.
const defaultOwner = "rafiki-cli"

// Options configures a Backend.
type Options struct {
	Pool   *pgxpool.Pool   // nil ⇒ corpus-only Analyze runs only; read methods and findings persistence return errors
	LLM    *llm.Client     // nil ⇒ Analyze returns ErrNoLLM unless StopAfter=="compact"
	Pricer insights.Pricer // nil-safe
	Owner  string          // conversation attribution; default "rafiki-cli"
}

// Backend implements agentcli.Backend directly against a Postgres pool: no
// gRPC, no auth. Analyze needs an *llm.Client; the read paths do not.
type Backend struct {
	pool  *pgxpool.Pool
	ins   *insights.Insights
	llm   *llm.Client
	owner string

	// pricer is kept directly on Backend (in addition to being handed to
	// insights.Insights) because analyze.Detect and analyze.Draft take an
	// insights.Pricer argument straight from their caller — there is no
	// public accessor on *insights.Insights to recover it from there.
	pricer insights.Pricer
}

var _ agentcli.Backend = (*Backend)(nil)

// New returns a Backend backed by o.Pool. o.Pool may be nil for corpus-only
// Analyze runs; read paths then return ErrNoPool. o.LLM may be nil: read
// paths work without one, but Analyze then returns ErrNoLLM.
func New(o Options) *Backend {
	owner := o.Owner
	if owner == "" {
		owner = defaultOwner
	}
	var ins *insights.Insights
	if o.Pool != nil {
		ins = insights.New(o.Pool).WithPricer(o.Pricer)
	}
	return &Backend{
		pool:   o.Pool,
		ins:    ins,
		llm:    o.LLM,
		owner:  owner,
		pricer: o.Pricer,
	}
}

// Stats delegates to insights.GlobalStats.
func (b *Backend) Stats(ctx context.Context, f insights.StatsFilter) (*insights.Stats, error) {
	if b.pool == nil {
		return nil, ErrNoPool
	}
	return b.ins.GlobalStats(ctx, f)
}

// ConversationStats delegates to insights.ConversationStats.
func (b *Backend) ConversationStats(ctx context.Context, id string) (*insights.Stats, error) {
	if b.pool == nil {
		return nil, ErrNoPool
	}
	return b.ins.ConversationStats(ctx, id)
}

// Search delegates to insights.Search.
func (b *Backend) Search(ctx context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error) {
	if b.pool == nil {
		return nil, ErrNoPool
	}
	return b.ins.Search(ctx, f)
}

// Export delegates to insights.Export.
func (b *Backend) Export(ctx context.Context, id string) (*insights.Transcript, error) {
	if b.pool == nil {
		return nil, ErrNoPool
	}
	return b.ins.Export(ctx, id)
}

// Findings delegates to store.ListFindings.
func (b *Backend) Findings(ctx context.Context, f store.FindingFilter) ([]store.FindingRow, error) {
	if b.pool == nil {
		return nil, ErrNoPool
	}
	return store.ListFindings(ctx, b.pool, f)
}

// SetFindingStatus delegates to store.SetFindingStatus.
func (b *Backend) SetFindingStatus(ctx context.Context, id, status string) (store.FindingRow, error) {
	if b.pool == nil {
		return store.FindingRow{}, ErrNoPool
	}
	return store.SetFindingStatus(ctx, b.pool, id, status)
}

// analyzeChanCap bounds the Analyze event channel: large enough that a
// normal-sized batch never blocks the producer goroutine on a slow consumer,
// small enough it isn't a real memory concern.
const analyzeChanCap = 64

// Analyze runs the analyze pipeline: resolve a population of conversations,
// skip ones already analyzed under this exact detector configuration (unless
// Force), run Export/Compact/Detect per conversation, then Rank findings
// across the batch and (unless StopAfter says otherwise) Draft skill edits
// for the top candidates. Events stream on the returned channel; the last
// event is either a Summary or, on a fatal error, an EventError — either way
// the channel is then closed.
//
// The ErrNoLLM check runs before anything else, including validating
// req.ConversationIDs: a caller that never configured an LLM should get a
// single unambiguous error regardless of what else is wrong with the
// request.
func (b *Backend) Analyze(ctx context.Context, req agentcli.AnalyzeRequest) (<-chan agentcli.AnalyzeEvent, error) {
	// The compact stage is a pure local transform (analyze.Compact), so it
	// needs no upstream; every later stage calls the LLM.
	if b.llm == nil && req.StopAfter != "compact" {
		return nil, ErrNoLLM
	}

	// Canonicalize every id to uuid's lowercase, unbraced string form —
	// uuid.Parse accepts uppercase/braced/urn forms too, but Postgres's own
	// ::text cast always produces the canonical lowercase form, and every
	// id-equality check downstream (AnalyzedSet's ANY($1::uuid[]) aside, which
	// compares as uuid not text) that compares against a stored id as a plain
	// string — e.g. slices.Contains-style dedup — silently fails across a
	// case mismatch. Discarding uuid.Parse's result (as this used to) validated
	// the id but never normalized it, so an uppercase-cased request id would
	// never match its own lowercase form once round-tripped through the store.
	if len(req.ConversationIDs) > 0 {
		canonical := make([]string, len(req.ConversationIDs))
		for i, id := range req.ConversationIDs {
			u, err := uuid.Parse(id)
			if err != nil {
				return nil, fmt.Errorf("agentcli/local: invalid conversation id %q: %w", id, err)
			}
			canonical[i] = u.String()
		}
		req.ConversationIDs = canonical
	}

	// A DB-backed population (explicit ids, a search Filter, or the default
	// unfiltered search) needs a pool to resolve — unlike every other Backend
	// method, Analyze doesn't check b.pool == nil up front, because a
	// corpus-only run legitimately has none. Keying this guard on
	// req.CorpusDir == "" (rather than len(ConversationIDs) > 0 || Filter != nil)
	// is deliberate: population()'s own dispatch falls through to a DB search
	// whenever CorpusDir is empty, even with no ConversationIDs and no Filter
	// set (a plain AnalyzeRequest{}) — that default branch was the actual gap,
	// since it panics deep inside runAnalyze on a nil *insights.Insights
	// instead of returning ErrNoPool like every other pool-dependent method.
	if b.pool == nil && req.CorpusDir == "" {
		return nil, ErrNoPool
	}

	profile := analyze.Profile{}
	if req.Profile != nil {
		profile = *req.Profile
	}
	profile.Defaults()

	ch := make(chan agentcli.AnalyzeEvent, analyzeChanCap)
	go b.runAnalyze(ctx, req, &profile, ch)
	return ch, nil
}
