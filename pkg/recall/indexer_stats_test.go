// SPDX-License-Identifier: Apache-2.0

package recall

import (
	"context"
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestIndexerStatsCountMarksAndOverflow(t *testing.T) {
	c := assert.NewAborting(t)
	ix := NewIndexer(IndexerOptions{Store: &indexerFakeStore{}, Logger: indexerTestLogger()})
	ix.Mark("a")
	ix.Mark("a")
	ix.Mark("b")
	st := ix.Stats()
	c.Eq(2, st.Dirty, "two distinct conversations marked")
	c.Eq(uint64(3), st.Marks, "three Mark calls")
	c.False(st.DirtyOverflow, "no overflow")

	for i := range maxDirty {
		ix.Mark(string(rune('A'+i%26)) + string(rune(i)))
	}
	st = ix.Stats()
	c.True(st.DirtyOverflow, "past maxDirty the set overflows")
	c.True(st.MarkOverflows > 0, "overflowing marks counted")

	ix.takeDirty()
	st = ix.Stats()
	c.Eq(0, st.Dirty, "taking the set empties it")
	c.False(st.DirtyOverflow, "and clears the overflow flag")
}

func TestIndexerStatsFullPassRecordsStagesAndBacklog(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{
		tryLockOK: true,
		cursors:   []ExtractCursor{{Conversation: indexerConv("c1")}, {Conversation: indexerConv("c2")}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "x")},
			"c2": {indexerMsg("c2", 0, "y")},
		},
		pending: make([]EmbedItem, 3),
	}
	sumErr := errors.New("summaries down")
	ix := NewIndexer(IndexerOptions{
		Store:     store,
		Embedder:  &indexerFakeEmbedder{model: "m"},
		Summaries: &indexerFakeSummaries{err: sumErr},
		Logger:    indexerTestLogger(),
	})
	ix.tickOnce(context.Background())

	st := ix.Stats()
	c.Eq(uint64(1), st.FullPasses, "one full pass")
	c.Eq(int64(2), st.LastFullBacklog, "backlog is the cursors the full pass found")
	c.Eq(uint64(2), st.ConversationsExtracted, "both conversations extracted")
	c.Eq(uint64(1), st.Extract.Runs, "extract stage ran once")
	c.True(!st.Extract.LastSuccess.IsZero(), "extract succeeded")
	c.Eq(uint64(1), st.Embed.Runs, "embed stage ran once")
	c.Eq(uint64(3), st.RowsEmbedded, "rows embedded")
	c.Eq(uint64(1), st.Summarize.Errors, "the failing summary stage is counted")
	c.True(st.Summarize.LastSuccess.IsZero(), "a failed stage is not a success")
}

func TestIndexerStatsCountExtractErrorsWithoutCountingThemExtracted(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{
		tryLockOK:   true,
		cursors:     []ExtractCursor{{Conversation: indexerConv("c1")}},
		messagesErr: errors.New("boom"),
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	ix.tickOnce(context.Background())
	st := ix.Stats()
	c.Eq(uint64(1), st.ExtractErrors, "extract error counted")
	c.Eq(uint64(0), st.ConversationsExtracted, "a failed conversation is not extracted")
}

func TestIndexerStatsCountLockSkipsAndErrors(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{tryLockOK: false}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	ix.tickOnce(context.Background())
	ix.tickMarked(context.Background(), []string{"c1"})
	st := ix.Stats()
	c.Eq(uint64(2), st.LockSkips, "both passes skipped on a held lock")
	c.Eq(uint64(0), st.FullPasses+st.MarkedPasses, "a skipped pass is not counted as run")

	store.tryLockErr = errors.New("db down")
	ix.tickOnce(context.Background())
	c.Eq(uint64(1), ix.Stats().LockErrors, "probe failure counted")
}

func TestIndexerStatsMarkedPassIsCounted(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{tryLockOK: true}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	ix.tickMarked(context.Background(), []string{"c1"})
	c.Eq(uint64(1), ix.Stats().MarkedPasses, "marked pass counted")
}

// The extract stage's last-success time is how an operator sees the indexer
// stalled: it must not advance through a database outage.
func TestIndexerExtractStageDoesNotSucceedWhenTheCursorListingFails(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{cursorsErr: errors.New("db down")}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.Error(ix.Tick(context.Background()), "the listing failure is still the pass's error")
	st := ix.Stats().Extract
	c.Eq(uint64(1), st.Runs, "ran")
	c.Eq(uint64(1), st.Errors, "a failed listing is a stage error")
	c.True(st.LastSuccess.IsZero(), "and not a success")
}

func TestIndexerExtractStageDoesNotSucceedWhenAConversationFails(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{
		cursors:     []ExtractCursor{{Conversation: indexerConv("c1")}},
		messagesErr: errors.New("boom"),
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.NoError(ix.Tick(context.Background()), "per-conversation extract failures never fail the pass")
	st := ix.Stats().Extract
	c.Eq(uint64(1), st.Errors, "but they fail the stage")
	c.True(st.LastSuccess.IsZero(), "and do not advance its last success")
}

func TestIndexerExtractStageSucceedsWhenEverythingExtracts(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{
		cursors:  []ExtractCursor{{Conversation: indexerConv("c1")}},
		messages: map[string][]Message{"c1": {indexerMsg("c1", 0, "x")}},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.NoError(ix.Tick(context.Background()), "Tick")
	st := ix.Stats().Extract
	c.Eq(uint64(0), st.Errors, "no error")
	c.False(st.LastSuccess.IsZero(), "success recorded")
}

func TestIndexerMarkedPassCursorListingFailureIsAnExtractStageError(t *testing.T) {
	c := assert.NewAborting(t)
	store := &indexerFakeStore{tryLockOK: true, cursorsErr: errors.New("db down")}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	ix.tickMarked(context.Background(), []string{"c1"})
	st := ix.Stats().Extract
	c.Eq(uint64(1), st.Errors, "the marked listing failure is counted")
	c.True(st.LastSuccess.IsZero(), "and is not a success")
}
