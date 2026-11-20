package recall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// indexerFakeStore implements only the Store methods the indexer calls; the
// embedded nil interface makes any other method panic.
type indexerFakeStore struct {
	Store

	tryLockOK    bool
	tryLockErr   error
	tryLockCalls int

	cursors         []ExtractCursor
	cursorsErr      error
	cursorsCalls    int
	cursorsExcluded [][]string
	cursorsLimits   []int

	messages     map[string][]Message
	messagesErr  error
	fromIDs      []string
	fromOrdinals []int

	writes    []indexerWriteCall
	writesErr error

	cursorsForCalls int
	cursorsForIDs   [][]string
	// forSeen and fullSeen, when set, receive one value per targeted and per
	// full cursor listing, so a test can wait for a pass without sleeping.
	forSeen  chan struct{}
	fullSeen chan struct{}

	pending       []EmbedItem
	pendingErr    error
	pendingCalls  int
	pendingModels []string
	pendingLimits []int

	setEmbedCalls  int
	setEmbedModels []string
	setEmbedItems  [][]EmbedItem
	setEmbedVecs   [][][]float32
	setEmbedErr    error
}

type indexerWriteCall struct {
	conversationID string
	ws             []Window
	through        int
}

func (f *indexerFakeStore) TryLock(ctx context.Context) (release func(), ok bool, err error) {
	f.tryLockCalls++
	if f.tryLockErr != nil {
		return nil, false, f.tryLockErr
	}
	return func() {}, f.tryLockOK, nil
}

func (f *indexerFakeStore) ExtractCursors(ctx context.Context, excluded []string, limit int) ([]ExtractCursor, error) {
	f.cursorsCalls++
	f.cursorsExcluded = append(f.cursorsExcluded, excluded)
	f.cursorsLimits = append(f.cursorsLimits, limit)
	if f.fullSeen != nil {
		f.fullSeen <- struct{}{}
	}
	return f.cursors, f.cursorsErr
}

func (f *indexerFakeStore) MessagesFrom(ctx context.Context, conversationID string, fromOrdinal int) ([]Message, error) {
	f.fromIDs = append(f.fromIDs, conversationID)
	f.fromOrdinals = append(f.fromOrdinals, fromOrdinal)
	return f.messages[conversationID], f.messagesErr
}

func (f *indexerFakeStore) WriteWindows(ctx context.Context, conversationID string, ws []Window, through int) error {
	f.writes = append(f.writes, indexerWriteCall{conversationID: conversationID, ws: ws, through: through})
	return f.writesErr
}

func (f *indexerFakeStore) ExtractCursorsFor(ctx context.Context, excluded []string, ids []string) ([]ExtractCursor, error) {
	f.cursorsForCalls++
	f.cursorsForIDs = append(f.cursorsForIDs, ids)
	if f.forSeen != nil {
		defer func() { f.forSeen <- struct{}{} }()
	}
	var out []ExtractCursor
	for _, c := range f.cursors {
		for _, id := range ids {
			if c.Conversation.ID == id {
				out = append(out, c)
			}
		}
	}
	return out, f.cursorsErr
}

func (f *indexerFakeStore) PendingEmbeds(ctx context.Context, model string, limit int) ([]EmbedItem, error) {
	f.pendingCalls++
	f.pendingModels = append(f.pendingModels, model)
	f.pendingLimits = append(f.pendingLimits, limit)
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	if len(f.pending) > limit {
		return f.pending[:limit], nil
	}
	return f.pending, nil
}

func (f *indexerFakeStore) SetEmbeddings(ctx context.Context, model string, items []EmbedItem, vecs [][]float32) error {
	f.setEmbedCalls++
	f.setEmbedModels = append(f.setEmbedModels, model)
	f.setEmbedItems = append(f.setEmbedItems, items)
	f.setEmbedVecs = append(f.setEmbedVecs, vecs)
	if f.setEmbedErr != nil {
		return f.setEmbedErr
	}
	f.pending = f.pending[len(items):]
	return nil
}

// indexerFakeEmbedder implements only Model and Embed, what the embed pass
// calls; Dimensions panics via the embedded nil interface.
type indexerFakeEmbedder struct {
	Embedder
	model string
	calls [][]string
	err   error
}

func (f *indexerFakeEmbedder) Model() string { return f.model }

func (f *indexerFakeEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	f.calls = append(f.calls, inputs)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(inputs))
	for i := range out {
		out[i] = []float32{0.5}
	}
	return out, nil
}

// indexerFakeSummaries records the summary stage's invocations.
type indexerFakeSummaries struct {
	SummaryPass
	calls int
	err   error
}

func (f *indexerFakeSummaries) Pass(ctx context.Context) error {
	f.calls++
	return f.err
}

func indexerTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func indexerConv(id string) ConversationMeta {
	return ConversationMeta{ID: id, OwnerUserID: "u1", Name: "conv " + id, Kind: "claude"}
}

// indexerMsg builds a user message whose content is a JSON string, so Extract
// yields "user: <text>".
func indexerMsg(conv string, ordinal int, text string) Message {
	return Message{
		ConversationID: conv,
		Ordinal:        ordinal,
		Role:           "user",
		Content:        json.RawMessage(strconv.Quote(text)),
	}
}

// indexerToolMsg builds a message with no extractable text, so it is skipped.
func indexerToolMsg(conv string, ordinal int) Message {
	return Message{
		ConversationID: conv,
		Ordinal:        ordinal,
		Role:           "user",
		Content:        json.RawMessage(`[]`),
	}
}

func TestIndexerExtractCreatesWindowsFromZero(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &indexerFakeStore{
		cursors: []ExtractCursor{{Conversation: indexerConv("c1")}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "first"), indexerMsg("c1", 1, "second")},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.Require().NoError(ix.Tick(context.Background()), "Tick")
	c.Require().Eq(1, store.cursorsCalls, "ExtractCursors calls")
	c.EqDiff(ExcludedEntrypoints, store.cursorsExcluded[0], "excluded")
	c.Eq(extractCursorLimit, store.cursorsLimits[0], "limit")
	if len(store.fromIDs) != 1 || store.fromIDs[0] != "c1" || store.fromOrdinals[0] != 0 {
		t.Fatalf("MessagesFrom = %v from %v, want [c1] from [0]", store.fromIDs, store.fromOrdinals)
	}
	c.Require().Len(store.writes, 1, "WriteWindows calls = %d, want 1", len(store.writes))
	ws := store.writes[0].ws
	c.Require().Len(ws, 1, "windows written = %d, want 1", len(ws))
	w := ws[0]
	c.False(w.ConversationID != "c1" || w.OwnerUserID != "u1" || w.Seq != 0 || w.OrdinalFrom != 0 || w.OrdinalTo != 1, "window = %+v", w)
	c.False(!strings.Contains(w.Text, "user: first") || !strings.Contains(w.Text, "user: second"), "window text = %q", w.Text)
	c.False(w.Sealed, "a sole window must stay unsealed")
}

func TestIndexerExtractRebuildsUnsealedTailFromItsStart(t *testing.T) {
	c := assert.NewCollecting(t)
	tail := &Window{ID: "w-tail", ConversationID: "c1", OwnerUserID: "u1",
		Seq: 2, OrdinalFrom: 5, OrdinalTo: 9, Text: "user: tail", ExtractorVersion: ExtractorVersion}
	store := &indexerFakeStore{
		cursors: []ExtractCursor{{Conversation: indexerConv("c1"), Tail: tail}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 5, "m5"), indexerMsg("c1", 6, "m6"), indexerMsg("c1", 7, "m7")},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.Require().NoError(ix.Tick(context.Background()), "Tick")
	if len(store.fromIDs) != 1 || store.fromIDs[0] != "c1" || store.fromOrdinals[0] != tail.OrdinalFrom {
		t.Fatalf("MessagesFrom = %v from %v, want [c1] from [%d]", store.fromIDs, store.fromOrdinals, tail.OrdinalFrom)
	}
	c.Require().Len(store.writes, 1, "WriteWindows calls = %d, want 1", len(store.writes))
	ws := store.writes[0].ws
	c.Require().NotEmpty(ws, "no windows written")
	first := ws[0]
	if first.Seq != tail.Seq || first.ID != tail.ID {
		t.Errorf("first window Seq/ID = %d/%q, want rebuilt tail %d/%q", first.Seq, first.ID, tail.Seq, tail.ID)
	}
	c.False(first.OrdinalFrom != 5 || first.OrdinalTo != 7, "first window ordinals = %d..%d, want 5..7", first.OrdinalFrom, first.OrdinalTo)
	c.False(first.Sealed, "rebuilt tail must stay unsealed")
}

func TestIndexerExtractContinuesAfterSealedTailWithOverlap(t *testing.T) {
	c := assert.NewCollecting(t)
	tail := &Window{ID: "w3", ConversationID: "c1", OwnerUserID: "u1",
		Seq: 3, OrdinalFrom: 7, OrdinalTo: 10, Text: "user: m10", Sealed: true, ExtractorVersion: ExtractorVersion}
	store := &indexerFakeStore{
		cursors: []ExtractCursor{{Conversation: indexerConv("c1"), Tail: tail}},
		messages: map[string][]Message{
			"c1": {
				indexerMsg("c1", 10, "overlap"), indexerMsg("c1", 11, "m11"),
				indexerMsg("c1", 12, "m12"), indexerMsg("c1", 13, "m13"),
			},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.Require().NoError(ix.Tick(context.Background()), "Tick")
	if len(store.fromIDs) != 1 || store.fromIDs[0] != "c1" || store.fromOrdinals[0] != tail.OrdinalTo {
		t.Fatalf("MessagesFrom = %v from %v, want [c1] from [%d]", store.fromIDs, store.fromOrdinals, tail.OrdinalTo)
	}
	c.Require().Len(store.writes, 1, "WriteWindows calls = %d, want 1", len(store.writes))
	ws := store.writes[0].ws
	c.Require().NotEmpty(ws, "no windows written")
	c.Eq(tail.Seq+1, ws[0].Seq, "first new window Seq")
	c.Eq("", ws[0].ID, "first new window ID")
	if ws[0].OrdinalFrom != 10 || ws[0].OrdinalTo != 13 {
		t.Errorf("first new window ordinals = %d..%d, want 10..13", ws[0].OrdinalFrom, ws[0].OrdinalTo)
	}
}

func TestIndexerExtractKeepsTailWhenAllWindowsNil(t *testing.T) {
	t.Run("unsealed tail", func(t *testing.T) {
		c := assert.NewCollecting(t)
		tail := &Window{ID: "w-tail", ConversationID: "c1", OwnerUserID: "u1",
			Seq: 2, OrdinalFrom: 5, OrdinalTo: 6, ExtractorVersion: ExtractorVersion}
		store := &indexerFakeStore{
			cursors: []ExtractCursor{{Conversation: indexerConv("c1"), Tail: tail}},
			messages: map[string][]Message{
				"c1": {indexerToolMsg("c1", 5), indexerToolMsg("c1", 6)},
			},
		}
		ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
		c.Require().NoError(ix.Tick(context.Background()), "Tick")
		c.Require().Len(store.writes, 1, "WriteWindows calls = %d, want 1 (empty build still marks)", len(store.writes))
		c.Empty(store.writes[0].ws, "no window rows written; the existing tail stands")
		c.Eq("c1", store.writes[0].conversationID, "marked conversation")
	})
	t.Run("sealed tail with only the overlap left", func(t *testing.T) {
		c := assert.NewCollecting(t)
		tail := &Window{ID: "w3", ConversationID: "c1", OwnerUserID: "u1",
			Seq: 3, OrdinalFrom: 7, OrdinalTo: 10, Sealed: true, ExtractorVersion: ExtractorVersion}
		store := &indexerFakeStore{
			cursors: []ExtractCursor{{Conversation: indexerConv("c1"), Tail: tail}},
			messages: map[string][]Message{
				"c1": {indexerToolMsg("c1", 10)},
			},
		}
		ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
		c.Require().NoError(ix.Tick(context.Background()), "Tick")
		c.Require().Len(store.writes, 1, "WriteWindows calls = %d, want 1 (nothing past the overlap, still marks)", len(store.writes))
		c.Empty(store.writes[0].ws, "no window rows written past the overlap")
	})
}

func TestIndexerEmbedPassBatchesAndCaps(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &indexerFakeStore{pending: make([]EmbedItem, 1000)}
	for i := range store.pending {
		store.pending[i] = EmbedItem{Source: SourceWindow, ID: strconv.Itoa(i), Text: fmt.Sprintf("text %d", i)}
	}
	emb := &indexerFakeEmbedder{model: "embed-model"}
	ix := NewIndexer(IndexerOptions{Store: store, Embedder: emb, Logger: indexerTestLogger()})
	c.Require().NoError(ix.Tick(context.Background()), "Tick")
	c.Require().Len(emb.calls, embedBatchesPerTick, "Embed calls = %d, want", len(emb.calls))
	for i, batch := range emb.calls {
		c.Len(batch, EmbedBatchSize, "batch %d has %d inputs, want", i, len(batch))
	}
	c.Eq(embedBatchesPerTick, store.pendingCalls, "PendingEmbeds calls")
	c.Eq(emb.model, store.pendingModels[0], "PendingEmbeds model")
	c.Eq(EmbedBatchSize, store.pendingLimits[0], "PendingEmbeds limit")
	c.Eq(embedBatchesPerTick, store.setEmbedCalls, "SetEmbeddings calls")
	c.Len(store.setEmbedVecs[0], EmbedBatchSize, "SetEmbeddings vectors = %d, want", len(store.setEmbedVecs[0]))
	c.Eq(1000-embedBatchesPerTick*EmbedBatchSize, len(store.pending), "pending left")
}

func TestIndexerNoEmbedderSkipsEmbedPass(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &indexerFakeStore{
		cursors: []ExtractCursor{{Conversation: indexerConv("c1")}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "hello")},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.Require().NoError(ix.Tick(context.Background()), "Tick")
	c.Eq(0, store.pendingCalls, "PendingEmbeds calls")
	c.Eq(1, store.cursorsCalls, "ExtractCursors calls")
}

func TestIndexerSummaryPassRuns(t *testing.T) {
	c := assert.NewCollecting(t)
	sum := &indexerFakeSummaries{err: errors.New("summarizer down")}
	store := &indexerFakeStore{}
	ix := NewIndexer(IndexerOptions{Store: store, Summaries: sum, Logger: indexerTestLogger()})
	err := ix.Tick(context.Background())
	c.Eq(1, sum.calls, "Pass calls")
	c.Error(err, "Tick must surface the summary pass error")
}

func TestIndexerRunSkipsTickWhenLockHeld(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &indexerFakeStore{} // tryLockOK false: another daemon holds the lock
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger(), Sweep: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		ix.Run(ctx)
		close(done)
	}()
	ix.Nudge()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	c.Eq(0, store.cursorsCalls, "ExtractCursors calls")
	c.NotEq(0, store.tryLockCalls, "no tick was attempted; the skip path was never exercised")
}

func TestIndexerRunStopsOnCancel(t *testing.T) {
	store := &indexerFakeStore{
		tryLockOK: true,
		cursors:   []ExtractCursor{{Conversation: indexerConv("c1")}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "first"), indexerMsg("c1", 1, "second")},
		},
	}
	emb := &indexerFakeEmbedder{model: "embed-model"}
	ix := NewIndexer(IndexerOptions{Store: store, Embedder: emb, Logger: indexerTestLogger(), Sweep: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		ix.Run(ctx)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of cancel")
	}
}

func TestIndexerNudgeNonBlocking(t *testing.T) {
	ix := NewIndexer(IndexerOptions{Store: &indexerFakeStore{}, Logger: indexerTestLogger()})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			ix.Nudge()
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Nudge blocked")
	}
}

// runIndexer starts ix.Run and returns a stop that cancels it and waits, after
// which the fake store's fields are safe to read.
func runIndexer(t *testing.T, ix *Indexer) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ix.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func markedStore() *indexerFakeStore {
	return &indexerFakeStore{
		tryLockOK: true,
		forSeen:   make(chan struct{}, 64),
		fullSeen:  make(chan struct{}, 64),
		cursors: []ExtractCursor{
			{Conversation: indexerConv("c1")},
			{Conversation: indexerConv("c2")},
			{Conversation: indexerConv("c3")},
		},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "first"), indexerMsg("c1", 1, "second")},
			"c2": {indexerMsg("c2", 0, "other")},
			"c3": {indexerMsg("c3", 0, "never marked")},
		},
	}
}

// A burst of commits becomes one targeted pass over just the marked
// conversations, not a full pass over everything pending.
func TestIndexerMarkedConversationsExtractOnceAfterTheDebounce(t *testing.T) {
	c := assert.NewCollecting(t)
	store := markedStore()
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger(),
		Sweep: time.Hour, Debounce: 40 * time.Millisecond, MaxDebounce: time.Second})
	stop := runIndexer(t, ix)
	for i := 0; i < 20; i++ {
		ix.Mark("c1")
		ix.Mark("c2")
		time.Sleep(5 * time.Millisecond)
	}
	awaitSignal(t, store.forSeen, "the targeted pass")
	time.Sleep(120 * time.Millisecond) // a second pass, if one were wrongly scheduled, would show here
	stop()

	c.Eq(1, store.cursorsForCalls, "one targeted pass for the whole burst")
	c.Eq(0, store.cursorsCalls, "no full pass")
	c.Require().Eq(1, len(store.cursorsForIDs), "ids of the pass")
	c.ElementsMatch([]string{"c1", "c2"}, store.cursorsForIDs[0], "only the marked conversations")
	var written []string
	for _, w := range store.writes {
		written = append(written, w.conversationID)
	}
	c.ElementsMatch([]string{"c1", "c2"}, written, "c3 was never marked and stays untouched")
}

// Under a continuous stream of commits the pass is not postponed forever: the
// max wait forces it.
func TestIndexerMaxDebounceForcesAPassUnderSteadyCommits(t *testing.T) {
	store := markedStore()
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger(),
		Sweep: time.Hour, Debounce: 200 * time.Millisecond, MaxDebounce: 80 * time.Millisecond})
	stop := runIndexer(t, ix)
	defer stop()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-store.forSeen:
			return
		case <-tick.C:
			ix.Mark("c1") // never quiet for the 200ms debounce
		case <-deadline:
			t.Fatal("no pass ran under continuous commits; the max wait did not fire")
		}
	}
}

// Marks for conversations with no work (the indexer's own machine
// conversations among them) cost one cursor lookup: no embed or summary pass
// runs, so a pass cannot cause itself.
func TestIndexerMarkWithNothingPendingRunsNoLaterPass(t *testing.T) {
	c := assert.NewCollecting(t)
	store := markedStore()
	store.cursors = nil
	emb := &indexerFakeEmbedder{model: "embed-model"}
	sum := &indexerFakeSummaries{}
	ix := NewIndexer(IndexerOptions{Store: store, Embedder: emb, Summaries: sum, Logger: indexerTestLogger(),
		Sweep: time.Hour, Debounce: 20 * time.Millisecond})
	stop := runIndexer(t, ix)
	ix.Mark("machine-conversation")
	awaitSignal(t, store.forSeen, "the targeted lookup")
	time.Sleep(50 * time.Millisecond)
	stop()

	c.Eq(1, store.cursorsForCalls, "the lookup ran")
	c.Eq(0, store.pendingCalls, "no embed pass")
	c.Eq(0, sum.calls, "no summary pass")
}

// The sweep runs the full pass with no event at all.
func TestIndexerSweepRunsAFullPass(t *testing.T) {
	c := assert.NewCollecting(t)
	store := markedStore()
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger(), Sweep: 20 * time.Millisecond})
	stop := runIndexer(t, ix)
	awaitSignal(t, store.fullSeen, "the sweep")
	stop()
	c.NotEq(0, store.cursorsCalls, "the sweep ran ExtractCursors")
	c.Eq(0, store.cursorsForCalls, "and no targeted pass")
}

// More marks than the dirty set holds fall back to a full pass, which finds
// the same work.
func TestIndexerDirtyOverflowFallsBackToAFullPass(t *testing.T) {
	c := assert.NewCollecting(t)
	store := markedStore()
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger(),
		Sweep: time.Hour, Debounce: 20 * time.Millisecond})
	for i := 0; i <= maxDirty; i++ {
		ix.Mark(fmt.Sprintf("conv-%d", i))
	}
	stop := runIndexer(t, ix)
	awaitSignal(t, store.fullSeen, "the full pass")
	stop()
	c.Eq(0, store.cursorsForCalls, "overflow skips the targeted lookup")
}

// A marked pass honours the index lock like any other.
func TestIndexerMarkedPassSkipsWhenLockHeld(t *testing.T) {
	c := assert.NewCollecting(t)
	store := markedStore()
	store.tryLockOK = false
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger(),
		Sweep: time.Hour, Debounce: 10 * time.Millisecond})
	stop := runIndexer(t, ix)
	ix.Mark("c1")
	time.Sleep(80 * time.Millisecond)
	stop()
	c.NotEq(0, store.tryLockCalls, "the lock was probed")
	c.Eq(0, store.cursorsForCalls, "nothing ran without the lock")
}

// A conversation whose messages all extract to no text still records how far
// it was read, so it is not pending again until something lands past it.
func TestIndexerStampsHowFarItReadEvenWhenNothingExtracts(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &indexerFakeStore{
		tryLockOK: true,
		cursors:   []ExtractCursor{{Conversation: indexerConv("c1")}, {Conversation: indexerConv("c2")}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "hello"), indexerToolMsg("c1", 1), indexerToolMsg("c1", 2)},
			"c2": {},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	c.Require().NoError(ix.Tick(context.Background()), "Tick")
	c.Require().Eq(2, len(store.writes), "both conversations were written")
	c.Eq(2, store.writes[0].through, "read through the trailing tool-result messages")
	c.Eq(-1, store.writes[1].through, "nothing read stamps nothing")
}
