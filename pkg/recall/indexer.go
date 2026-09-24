package recall

import (
	"context"
	"errors"
	"log/slog"
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
)

// Indexer keeps the recall index current in the background: it windows new
// conversation messages, embeds pending rows, and runs the summary pass on a
// ticker, coordinating across daemons through the store's index lock.
type Indexer struct {
	store     Store
	embedder  Embedder // nil disables the embed pass
	summaries SummaryPass
	logger    *slog.Logger
	tick      time.Duration
	nudge     chan struct{}
}

// IndexerOptions configures an Indexer.
type IndexerOptions struct {
	Store     Store
	Embedder  Embedder      // nil = no embed pass
	Summaries SummaryPass   // nil = no summary pass
	Logger    *slog.Logger  // nil = slog.Default
	Tick      time.Duration // 0 = IndexerTick
}

// NewIndexer builds an indexer.
func NewIndexer(o IndexerOptions) *Indexer {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tick := o.Tick
	if tick <= 0 {
		tick = IndexerTick
	}
	return &Indexer{
		store:     o.Store,
		embedder:  o.Embedder,
		summaries: o.Summaries,
		logger:    logger,
		tick:      tick,
		nudge:     make(chan struct{}, 1),
	}
}

// Nudge requests one extra pass soon. It is safe from any goroutine, never
// blocks, and coalesces nudges that arrive while a pass is still running.
func (ix *Indexer) Nudge() {
	select {
	case ix.nudge <- struct{}{}:
	default:
	}
}

// Run drives the indexer until ctx is done: one pass per tick or nudge,
// skipped when another daemon holds the index lock. Errors are logged and
// never stop the loop.
func (ix *Indexer) Run(ctx context.Context) {
	ticker := time.NewTicker(ix.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-ix.nudge:
		}
		if ctx.Err() != nil {
			return
		}
		ix.tickOnce(ctx)
	}
}

// tickOnce runs one tick under the index lock, skipping it when the lock is
// held elsewhere or cannot be probed.
func (ix *Indexer) tickOnce(ctx context.Context) {
	release, ok, err := ix.store.TryLock(ctx)
	if err != nil {
		if ctx.Err() == nil {
			ix.logger.Warn("recall indexer", "error", err)
		}
		return
	}
	if !ok {
		return
	}
	defer release()
	if err := ix.Tick(ctx); err != nil && ctx.Err() == nil {
		ix.logger.Warn("recall indexer", "error", err)
	}
}

// Tick runs one full indexing pass: extract, then embed, then summarize. A
// per-conversation extract error is logged and the pass continues; an embed
// or summary error ends that pass for this tick without touching the others.
// The returned error joins the pass-level failures, for callers that log it.
func (ix *Indexer) Tick(ctx context.Context) error {
	var err error
	if e := ix.extractPass(ctx); e != nil {
		err = errors.Join(err, e)
	}
	if ix.embedder != nil {
		if e := ix.embedPass(ctx); e != nil {
			err = errors.Join(err, e)
		}
	}
	if ix.summaries != nil {
		if e := ix.summaries.Pass(ctx); e != nil {
			err = errors.Join(err, e)
		}
	}
	return err
}

// extractPass windows new messages for up to extractCursorLimit
// conversations, skipping one whose extract or write fails.
func (ix *Indexer) extractPass(ctx context.Context) error {
	cursors, err := ix.store.ExtractCursors(ctx, ExcludedEntrypoints, extractCursorLimit)
	if err != nil {
		return err
	}
	for _, c := range cursors {
		if ctx.Err() != nil {
			return nil
		}
		if err := ix.extractConversation(ctx, c); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			ix.logger.Warn("recall indexer", "error", err)
		}
	}
	return nil
}

// extractConversation windows one conversation from its cursor position: a
// nil tail starts at ordinal 0, an unsealed tail is re-read from its first
// message, a sealed tail is re-read from its last message (the overlap) and
// new windows continue at Seq+1. When the build yields no windows the
// existing tail stands and nothing is written.
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
	if len(ws) == 0 {
		return nil
	}
	return ix.store.WriteWindows(ctx, c.Conversation.ID, ws)
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
	}
	return nil
}
