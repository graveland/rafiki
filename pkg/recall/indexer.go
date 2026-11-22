package recall

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// SummaryPass is the summarizer stage the indexer drives after extraction and
// embedding. It is defined here, not as the Summarizer type itself, so the
// indexer depends only on this one method.
type SummaryPass interface {
	Pass(ctx context.Context) error
}

const (
	extractCursorLimit  = 50
	embedBatchesPerTick = 10
	// maxDirty bounds the marked-conversation set. Past it the ids are
	// dropped and the next pass is a full one, which finds the same work.
	maxDirty = 10000
)

// Indexer keeps the recall index current in the background: it windows new
// conversation messages, embeds pending rows, and runs the summary pass.
//
// Work is event-driven. A message commit calls Mark, and once commits have
// paused for the debounce the indexer extracts exactly the marked
// conversations. A slow sweep runs the full pass for everything no event
// announces, and Nudge forces a full pass now. Passes take the store's index
// lock.
type Indexer struct {
	store       Store
	embedder    Embedder // nil disables the embed pass
	summaries   SummaryPass
	logger      *slog.Logger
	sweep       time.Duration
	debounce    time.Duration
	maxDebounce time.Duration
	nudge       chan struct{}
	marked      chan struct{}

	dirtyMu       sync.Mutex
	dirty         map[string]struct{}
	dirtyOverflow bool

	stats indexerCounters
}

// IndexerOptions configures an Indexer.
type IndexerOptions struct {
	Store     Store
	Embedder  Embedder     // nil = no embed pass
	Summaries SummaryPass  // nil = no summary pass
	Logger    *slog.Logger // nil = slog.Default
	// Sweep is the full-pass interval; 0 = IndexerSweep.
	Sweep time.Duration
	// Debounce is the commit pause before a marked pass; 0 = IndexerDebounce.
	Debounce time.Duration
	// MaxDebounce caps that wait under continuous commits; 0 = IndexerMaxDebounce.
	MaxDebounce time.Duration
}

// NewIndexer builds an indexer.
func NewIndexer(o IndexerOptions) *Indexer {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	sweep, debounce, maxDebounce := o.Sweep, o.Debounce, o.MaxDebounce
	if sweep <= 0 {
		sweep = IndexerSweep
	}
	if debounce <= 0 {
		debounce = IndexerDebounce
	}
	if maxDebounce <= 0 {
		maxDebounce = IndexerMaxDebounce
	}
	return &Indexer{
		store:       o.Store,
		embedder:    o.Embedder,
		summaries:   o.Summaries,
		logger:      logger,
		sweep:       sweep,
		debounce:    debounce,
		maxDebounce: maxDebounce,
		nudge:       make(chan struct{}, 1),
		marked:      make(chan struct{}, 1),
		dirty:       make(map[string]struct{}),
	}
}

// Mark records that conversationID has new messages and schedules a pass over
// it after the debounce. It is safe from any goroutine and never blocks, so a
// message commit can call it directly.
func (ix *Indexer) Mark(conversationID string) {
	ix.stats.marks.Add(1)
	ix.dirtyMu.Lock()
	if len(ix.dirty) < maxDirty {
		ix.dirty[conversationID] = struct{}{}
	} else {
		ix.dirtyOverflow = true
		ix.stats.markOverflows.Add(1)
	}
	ix.dirtyMu.Unlock()
	select {
	case ix.marked <- struct{}{}:
	default:
	}
}

// takeDirty returns the marked ids and clears the set; full reports that ids
// overflowed and a full pass is needed instead.
func (ix *Indexer) takeDirty() (ids []string, full bool) {
	ix.dirtyMu.Lock()
	defer ix.dirtyMu.Unlock()
	ids = make([]string, 0, len(ix.dirty))
	for id := range ix.dirty {
		ids = append(ids, id)
	}
	clear(ix.dirty)
	full = ix.dirtyOverflow
	ix.dirtyOverflow = false
	return ids, full
}

// Nudge requests one extra pass soon. It is safe from any goroutine, never
// blocks, and coalesces nudges that arrive while a pass is still running.
func (ix *Indexer) Nudge() {
	select {
	case ix.nudge <- struct{}{}:
	default:
	}
}

// Run drives the indexer until ctx is done. A full pass runs on every sweep
// and nudge; marked conversations get a targeted pass once commits have paused
// for the debounce (or the max wait has passed). A pass is skipped when
// another holder has the index lock. Errors are logged and never stop the loop.
func (ix *Indexer) Run(ctx context.Context) {
	sweep := time.NewTicker(ix.sweep)
	defer sweep.Stop()
	var quiet, ceiling *time.Timer
	var quietC, ceilingC <-chan time.Time
	disarm := func() {
		if quiet != nil {
			quiet.Stop()
		}
		if ceiling != nil {
			ceiling.Stop()
		}
		quiet, ceiling, quietC, ceilingC = nil, nil, nil, nil
	}
	defer disarm()
	for {
		full := false
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			full = true
		case <-ix.nudge:
			full = true
		case <-ix.marked:
			// Each commit pushes the quiet deadline out; the ceiling is set
			// once, by the first commit of a burst.
			if quiet != nil {
				quiet.Stop()
			}
			quiet = time.NewTimer(ix.debounce)
			quietC = quiet.C
			if ceiling == nil {
				ceiling = time.NewTimer(ix.maxDebounce)
				ceilingC = ceiling.C
			}
			continue
		case <-quietC:
		case <-ceilingC:
		}
		disarm()
		if ctx.Err() != nil {
			return
		}
		ids, overflow := ix.takeDirty()
		if full || overflow {
			ix.tickOnce(ctx)
		} else {
			ix.tickMarked(ctx, ids)
		}
	}
}

// tickMarked runs one pass over the marked conversations alone: extract them,
// then embed and summarize. It does nothing when none of them has work, which
// is the case for the indexer's own machine conversations (their commits mark
// them too), so a pass cannot trigger itself.
func (ix *Indexer) tickMarked(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	release, ok := ix.takeLock(ctx)
	if !ok {
		return
	}
	defer release()
	ix.stats.markedPasses.Add(1)
	cursors, err := ix.store.ExtractCursorsFor(ctx, ExcludedEntrypoints, ids)
	if err != nil {
		_ = ix.stats.extract.observe(time.Now(), func() error { return err })
		if ctx.Err() == nil {
			ix.logger.Warn("recall indexer", "error", err)
		}
		return
	}
	if len(cursors) == 0 {
		return
	}
	if err := ix.runPasses(ctx, cursors, nil); err != nil && ctx.Err() == nil {
		ix.logger.Warn("recall indexer", "error", err)
	}
}

// tickOnce runs one tick under the index lock, skipping it when the lock is
// held elsewhere or cannot be probed.
func (ix *Indexer) tickOnce(ctx context.Context) {
	release, ok := ix.takeLock(ctx)
	if !ok {
		return
	}
	defer release()
	ix.stats.fullPasses.Add(1)
	if err := ix.Tick(ctx); err != nil && ctx.Err() == nil {
		ix.logger.Warn("recall indexer", "error", err)
	}
}

// Tick runs one full indexing pass: extract, then embed, then summarize. A
// per-conversation extract error is logged and the pass continues; an embed
// or summary error ends that pass for this tick without touching the others.
// The returned error joins the pass-level failures, for callers that log it.
//
// Tick does not take the advisory lock itself — Run holds it for each pass
// (tickOnce) — so Tick is not safe to call concurrently with Run. The
// daemon's caller is that Run loop, started once at startup and held for the
// daemon's life; the tests call Tick directly, single-threaded.
func (ix *Indexer) Tick(ctx context.Context) error {
	cursors, err := ix.store.ExtractCursors(ctx, ExcludedEntrypoints, extractCursorLimit)
	if err == nil {
		ix.stats.lastFullBacklog.Store(int64(len(cursors)))
	}
	return ix.runPasses(ctx, cursors, err)
}

// runPasses extracts cursors, then embeds and summarizes. cursorErr is the
// failure to list them, joined into the result: the later passes still run.
func (ix *Indexer) runPasses(ctx context.Context, cursors []ExtractCursor, cursorErr error) error {
	err := cursorErr
	// The stage fails when the cursor listing failed, when any conversation's
	// extraction failed, or when the pass was cancelled part-way: its
	// last-success time must not advance through a database outage. Its
	// failures are not part of the pass's returned error, which has only ever
	// carried the listing, embed and summary failures.
	_ = ix.stats.extract.observe(time.Now(), func() error {
		return errors.Join(cursorErr, ix.extractCursors(ctx, cursors))
	})
	if ix.embedder != nil {
		if e := ix.stats.embed.observe(time.Now(), func() error { return ix.embedPass(ctx) }); e != nil {
			err = errors.Join(err, e)
		}
	}
	if ix.summaries != nil {
		if e := ix.stats.summarize.observe(time.Now(), func() error { return ix.summaries.Pass(ctx) }); e != nil {
			err = errors.Join(err, e)
		}
	}
	return err
}

// extractCursors windows each conversation, skipping one whose extract or
// write fails.
func (ix *Indexer) extractCursors(ctx context.Context, cursors []ExtractCursor) error {
	var failed error
	for _, c := range cursors {
		if ctx.Err() != nil {
			return errors.Join(failed, ctx.Err())
		}
		if err := ix.extractConversation(ctx, c); err != nil {
			if ctx.Err() != nil {
				return errors.Join(failed, ctx.Err())
			}
			ix.stats.extractErrors.Add(1)
			ix.logger.Warn("recall indexer", "error", err)
			failed = errors.Join(failed, err)
			continue
		}
		ix.stats.conversationsExtracted.Add(1)
	}
	return failed
}

// extractConversation windows one conversation from its cursor position: a
// nil tail starts at ordinal 0, an unsealed tail is re-read from its first
// message, a sealed tail is re-read from its last message (the overlap) and
// new windows continue at Seq+1. When the build yields no windows the
// existing tail stands and no window rows are written, but the conversation is
// still handed to WriteWindows so it is marked (see WriteWindows).
func (ix *Indexer) extractConversation(ctx context.Context, c ExtractCursor) error {
	from := 0
	switch {
	case c.Tail == nil:
	case c.Tail.Sealed:
		from = c.Tail.OrdinalTo
	default:
		from = c.Tail.OrdinalFrom
	}
	msgs, err := ix.store.MessagesFrom(ctx, c.Conversation.ID, from)
	if err != nil {
		return err
	}
	ws := BuildWindows(c.Conversation.ID, c.Conversation.OwnerUserID, c.Tail, ExtractAll(msgs))
	// through is the highest ordinal actually read: the conversation is
	// pending again only when something lands past it, even if every message
	// read here extracted to no text and built no window.
	through := -1
	if len(msgs) > 0 {
		through = msgs[len(msgs)-1].Ordinal
	}
	// Route the empty case through WriteWindows too: it writes no rows but
	// still stamps derived_skip, so a flagged conversation whose messages
	// build no windows is never summarised.
	return ix.store.WriteWindows(ctx, c.Conversation.ID, ws, through)
}

// embedPass embeds pending rows in EmbedBatchSize chunks, at most
// embedBatchesPerTick chunks per pass so extraction and summaries are not
// starved. The first error stops the pass for this tick.
func (ix *Indexer) embedPass(ctx context.Context) error {
	model := ix.embedder.Model()
	for i := 0; i < embedBatchesPerTick; i++ {
		if ctx.Err() != nil {
			return nil
		}
		items, err := ix.store.PendingEmbeds(ctx, model, EmbedBatchSize)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		texts := make([]string, len(items))
		for j, it := range items {
			texts[j] = it.Text
		}
		vecs, err := ix.embedder.Embed(ctx, texts)
		if err != nil {
			return err
		}
		if err := ix.store.SetEmbeddings(ctx, model, items, vecs); err != nil {
			return err
		}
		ix.stats.rowsEmbedded.Add(uint64(len(items)))
	}
	return nil
}
