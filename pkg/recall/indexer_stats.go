// SPDX-License-Identifier: Apache-2.0

package recall

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.graveland.dev/rafiki/pkg/errclass"
)

// stageCounters accumulate one pass stage's runs, failures, wall time and last
// success.
type stageCounters struct {
	runs        atomic.Uint64
	errors      atomic.Uint64
	nanos       atomic.Int64
	lastSuccess atomic.Int64 // unix nanoseconds; 0 until the first success
}

// observe runs fn and records it as one run of the stage. A stage that
// returned an error is not a success, so lastSuccess only moves when the stage
// finished cleanly.
func (s *stageCounters) observe(start time.Time, fn func() error) error {
	err := fn()
	s.runs.Add(1)
	s.nanos.Add(int64(time.Since(start)))
	if err != nil {
		s.errors.Add(1)
		return err
	}
	s.lastSuccess.Store(time.Now().UnixNano())
	return nil
}

func (s *stageCounters) snapshot() StageStats {
	st := StageStats{
		Runs:     s.runs.Load(),
		Errors:   s.errors.Load(),
		Duration: time.Duration(s.nanos.Load()),
	}
	if ns := s.lastSuccess.Load(); ns != 0 {
		st.LastSuccess = time.Unix(0, ns)
	}
	return st
}

type indexerCounters struct {
	marks, markOverflows   atomic.Uint64
	fullPasses             atomic.Uint64
	markedPasses           atomic.Uint64
	lockSkips, lockErrors  atomic.Uint64
	conversationsExtracted atomic.Uint64
	extractErrors          atomic.Uint64
	rowsEmbedded           atomic.Uint64
	lastFullBacklog        atomic.Int64

	embedErrMu sync.Mutex
	embedErrs  map[errclass.Class]uint64

	extract, embed, summarize stageCounters
}

// countEmbedError records one failed embedding call under its class. The map is
// bounded by the class vocabulary, so it cannot grow with the error text.
func (c *indexerCounters) countEmbedError(err error) {
	class := errclass.FromError(err)
	if class == "" {
		return
	}
	c.embedErrMu.Lock()
	defer c.embedErrMu.Unlock()
	if c.embedErrs == nil {
		c.embedErrs = make(map[errclass.Class]uint64)
	}
	c.embedErrs[class]++
}

func (c *indexerCounters) embedErrorSnapshot() map[string]uint64 {
	c.embedErrMu.Lock()
	defer c.embedErrMu.Unlock()
	out := make(map[string]uint64, len(c.embedErrs))
	for k, v := range c.embedErrs {
		out[string(k)] = v
	}
	return out
}

// StageStats is one pass stage's cumulative figures.
type StageStats struct {
	Runs     uint64
	Errors   uint64
	Duration time.Duration
	// LastSuccess is zero until the stage has finished cleanly once.
	LastSuccess time.Time
}

// IndexerStats is a point-in-time view of the indexer for monitoring. It reads
// only the indexer's own memory, never the database, so a scrape costs no
// query.
type IndexerStats struct {
	// Dirty is the number of conversations marked and awaiting a pass;
	// DirtyOverflow reports that the marked set overflowed and the next pass is
	// a full one.
	Dirty         int
	DirtyOverflow bool
	// LastFullBacklog is how many conversations the most recent full pass found
	// with work, capped at the cursor limit; it is the nearest thing to a
	// pending count that costs no query.
	LastFullBacklog int64

	Marks         uint64
	MarkOverflows uint64
	FullPasses    uint64
	MarkedPasses  uint64
	// LockSkips counts passes skipped because another holder had the index
	// lock; LockErrors counts lock probes that failed.
	LockSkips  uint64
	LockErrors uint64

	ConversationsExtracted uint64
	ExtractErrors          uint64
	RowsEmbedded           uint64
	// EmbedErrors counts failed embedding calls by errclass.Class spelling
	// (usage_limit, auth, rate_limit, ...). A class that has not occurred is
	// absent.
	EmbedErrors map[string]uint64

	Extract, Embed, Summarize StageStats
}

// Stats returns the indexer's current gauges and cumulative counters.
func (ix *Indexer) Stats() IndexerStats {
	ix.dirtyMu.Lock()
	dirty, overflow := len(ix.dirty), ix.dirtyOverflow
	ix.dirtyMu.Unlock()
	c := &ix.stats
	return IndexerStats{
		Dirty:                  dirty,
		DirtyOverflow:          overflow,
		LastFullBacklog:        c.lastFullBacklog.Load(),
		Marks:                  c.marks.Load(),
		MarkOverflows:          c.markOverflows.Load(),
		FullPasses:             c.fullPasses.Load(),
		MarkedPasses:           c.markedPasses.Load(),
		LockSkips:              c.lockSkips.Load(),
		LockErrors:             c.lockErrors.Load(),
		ConversationsExtracted: c.conversationsExtracted.Load(),
		ExtractErrors:          c.extractErrors.Load(),
		RowsEmbedded:           c.rowsEmbedded.Load(),
		EmbedErrors:            c.embedErrorSnapshot(),
		Extract:                c.extract.snapshot(),
		Embed:                  c.embed.snapshot(),
		Summarize:              c.summarize.snapshot(),
	}
}

// takeLock probes the index lock. ok is false when the pass must be skipped:
// another holder has the lock, or the probe failed (logged unless the context
// is done).
func (ix *Indexer) takeLock(ctx context.Context) (release func(), ok bool) {
	release, ok, err := ix.store.TryLock(ctx)
	if err != nil {
		ix.stats.lockErrors.Add(1)
		if ctx.Err() == nil {
			ix.logger.Warn("recall indexer", "error", err)
		}
		return nil, false
	}
	if !ok {
		ix.stats.lockSkips.Add(1)
		return nil, false
	}
	return release, true
}
