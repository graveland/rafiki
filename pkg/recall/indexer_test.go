package recall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	return f.cursors, f.cursorsErr
}

func (f *indexerFakeStore) MessagesFrom(ctx context.Context, conversationID string, fromOrdinal int) ([]Message, error) {
	f.fromIDs = append(f.fromIDs, conversationID)
	f.fromOrdinals = append(f.fromOrdinals, fromOrdinal)
	return f.messages[conversationID], f.messagesErr
}

func (f *indexerFakeStore) WriteWindows(ctx context.Context, conversationID string, ws []Window) error {
	f.writes = append(f.writes, indexerWriteCall{conversationID: conversationID, ws: ws})
	return f.writesErr
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
	store := &indexerFakeStore{
		cursors: []ExtractCursor{{Conversation: indexerConv("c1")}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "first"), indexerMsg("c1", 1, "second")},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	if err := ix.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if store.cursorsCalls != 1 {
		t.Fatalf("ExtractCursors calls = %d, want 1", store.cursorsCalls)
	}
	if got := store.cursorsExcluded[0]; !slices.Equal(got, ExcludedEntrypoints) {
		t.Errorf("excluded = %v, want %v", got, ExcludedEntrypoints)
	}
	if got := store.cursorsLimits[0]; got != extractCursorLimit {
		t.Errorf("limit = %d, want %d", got, extractCursorLimit)
	}
	if len(store.fromIDs) != 1 || store.fromIDs[0] != "c1" || store.fromOrdinals[0] != 0 {
		t.Fatalf("MessagesFrom = %v from %v, want [c1] from [0]", store.fromIDs, store.fromOrdinals)
	}
	if len(store.writes) != 1 {
		t.Fatalf("WriteWindows calls = %d, want 1", len(store.writes))
	}
	ws := store.writes[0].ws
	if len(ws) != 1 {
		t.Fatalf("windows written = %d, want 1", len(ws))
	}
	w := ws[0]
	if w.ConversationID != "c1" || w.OwnerUserID != "u1" || w.Seq != 0 || w.OrdinalFrom != 0 || w.OrdinalTo != 1 {
		t.Errorf("window = %+v", w)
	}
	if !strings.Contains(w.Text, "user: first") || !strings.Contains(w.Text, "user: second") {
		t.Errorf("window text = %q", w.Text)
	}
	if w.Sealed {
		t.Error("a sole window must stay unsealed")
	}
}

func TestIndexerExtractRebuildsUnsealedTailFromItsStart(t *testing.T) {
	tail := &Window{ID: "w-tail", ConversationID: "c1", OwnerUserID: "u1",
		Seq: 2, OrdinalFrom: 5, OrdinalTo: 9, Text: "user: tail", ExtractorVersion: ExtractorVersion}
	store := &indexerFakeStore{
		cursors: []ExtractCursor{{Conversation: indexerConv("c1"), Tail: tail}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 5, "m5"), indexerMsg("c1", 6, "m6"), indexerMsg("c1", 7, "m7")},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	if err := ix.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(store.fromIDs) != 1 || store.fromIDs[0] != "c1" || store.fromOrdinals[0] != tail.OrdinalFrom {
		t.Fatalf("MessagesFrom = %v from %v, want [c1] from [%d]", store.fromIDs, store.fromOrdinals, tail.OrdinalFrom)
	}
	if len(store.writes) != 1 {
		t.Fatalf("WriteWindows calls = %d, want 1", len(store.writes))
	}
	ws := store.writes[0].ws
	if len(ws) == 0 {
		t.Fatal("no windows written")
	}
	first := ws[0]
	if first.Seq != tail.Seq || first.ID != tail.ID {
		t.Errorf("first window Seq/ID = %d/%q, want rebuilt tail %d/%q", first.Seq, first.ID, tail.Seq, tail.ID)
	}
	if first.OrdinalFrom != 5 || first.OrdinalTo != 7 {
		t.Errorf("first window ordinals = %d..%d, want 5..7", first.OrdinalFrom, first.OrdinalTo)
	}
	if first.Sealed {
		t.Error("rebuilt tail must stay unsealed")
	}
}

func TestIndexerExtractContinuesAfterSealedTailWithOverlap(t *testing.T) {
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
	if err := ix.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(store.fromIDs) != 1 || store.fromIDs[0] != "c1" || store.fromOrdinals[0] != tail.OrdinalTo {
		t.Fatalf("MessagesFrom = %v from %v, want [c1] from [%d]", store.fromIDs, store.fromOrdinals, tail.OrdinalTo)
	}
	if len(store.writes) != 1 {
		t.Fatalf("WriteWindows calls = %d, want 1", len(store.writes))
	}
	ws := store.writes[0].ws
	if len(ws) == 0 {
		t.Fatal("no windows written")
	}
	if ws[0].Seq != tail.Seq+1 {
		t.Errorf("first new window Seq = %d, want %d (tail.Seq+1)", ws[0].Seq, tail.Seq+1)
	}
	if ws[0].ID != "" {
		t.Errorf("first new window ID = %q, want empty (assigned by the store)", ws[0].ID)
	}
	if ws[0].OrdinalFrom != 10 || ws[0].OrdinalTo != 13 {
		t.Errorf("first new window ordinals = %d..%d, want 10..13", ws[0].OrdinalFrom, ws[0].OrdinalTo)
	}
}

func TestIndexerExtractKeepsTailWhenAllWindowsNil(t *testing.T) {
	t.Run("unsealed tail", func(t *testing.T) {
		tail := &Window{ID: "w-tail", ConversationID: "c1", OwnerUserID: "u1",
			Seq: 2, OrdinalFrom: 5, OrdinalTo: 6, ExtractorVersion: ExtractorVersion}
		store := &indexerFakeStore{
			cursors: []ExtractCursor{{Conversation: indexerConv("c1"), Tail: tail}},
			messages: map[string][]Message{
				"c1": {indexerToolMsg("c1", 5), indexerToolMsg("c1", 6)},
			},
		}
		ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
		if err := ix.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if len(store.writes) != 0 {
			t.Errorf("WriteWindows calls = %d, want 0 (nil build keeps the existing tail)", len(store.writes))
		}
	})
	t.Run("sealed tail with only the overlap left", func(t *testing.T) {
		tail := &Window{ID: "w3", ConversationID: "c1", OwnerUserID: "u1",
			Seq: 3, OrdinalFrom: 7, OrdinalTo: 10, Sealed: true, ExtractorVersion: ExtractorVersion}
		store := &indexerFakeStore{
			cursors: []ExtractCursor{{Conversation: indexerConv("c1"), Tail: tail}},
			messages: map[string][]Message{
				"c1": {indexerToolMsg("c1", 10)},
			},
		}
		ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
		if err := ix.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if len(store.writes) != 0 {
			t.Errorf("WriteWindows calls = %d, want 0 (nothing past the overlap)", len(store.writes))
		}
	})
}

func TestIndexerEmbedPassBatchesAndCaps(t *testing.T) {
	store := &indexerFakeStore{pending: make([]EmbedItem, 1000)}
	for i := range store.pending {
		store.pending[i] = EmbedItem{Source: SourceWindow, ID: strconv.Itoa(i), Text: fmt.Sprintf("text %d", i)}
	}
	emb := &indexerFakeEmbedder{model: "embed-model"}
	ix := NewIndexer(IndexerOptions{Store: store, Embedder: emb, Logger: indexerTestLogger()})
	if err := ix.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(emb.calls) != embedBatchesPerTick {
		t.Fatalf("Embed calls = %d, want %d", len(emb.calls), embedBatchesPerTick)
	}
	for i, batch := range emb.calls {
		if len(batch) != EmbedBatchSize {
			t.Errorf("batch %d has %d inputs, want %d", i, len(batch), EmbedBatchSize)
		}
	}
	if store.pendingCalls != embedBatchesPerTick {
		t.Errorf("PendingEmbeds calls = %d, want %d", store.pendingCalls, embedBatchesPerTick)
	}
	if store.pendingModels[0] != emb.model {
		t.Errorf("PendingEmbeds model = %q, want %q", store.pendingModels[0], emb.model)
	}
	if store.pendingLimits[0] != EmbedBatchSize {
		t.Errorf("PendingEmbeds limit = %d, want %d", store.pendingLimits[0], EmbedBatchSize)
	}
	if store.setEmbedCalls != embedBatchesPerTick {
		t.Errorf("SetEmbeddings calls = %d, want %d", store.setEmbedCalls, embedBatchesPerTick)
	}
	if len(store.setEmbedVecs[0]) != EmbedBatchSize {
		t.Errorf("SetEmbeddings vectors = %d, want %d", len(store.setEmbedVecs[0]), EmbedBatchSize)
	}
	if left := len(store.pending); left != 1000-embedBatchesPerTick*EmbedBatchSize {
		t.Errorf("pending left = %d, want %d (embed pass resumed next tick)", left, 1000-embedBatchesPerTick*EmbedBatchSize)
	}
}

func TestIndexerNoEmbedderSkipsEmbedPass(t *testing.T) {
	store := &indexerFakeStore{
		cursors: []ExtractCursor{{Conversation: indexerConv("c1")}},
		messages: map[string][]Message{
			"c1": {indexerMsg("c1", 0, "hello")},
		},
	}
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger()})
	if err := ix.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if store.pendingCalls != 0 {
		t.Errorf("PendingEmbeds calls = %d, want 0", store.pendingCalls)
	}
	if store.cursorsCalls != 1 {
		t.Errorf("ExtractCursors calls = %d, want 1 (extract pass still runs)", store.cursorsCalls)
	}
}

func TestIndexerSummaryPassRuns(t *testing.T) {
	sum := &indexerFakeSummaries{err: errors.New("summarizer down")}
	store := &indexerFakeStore{}
	ix := NewIndexer(IndexerOptions{Store: store, Summaries: sum, Logger: indexerTestLogger()})
	err := ix.Tick(context.Background())
	if sum.calls != 1 {
		t.Errorf("Pass calls = %d, want 1", sum.calls)
	}
	if err == nil {
		t.Error("Tick must surface the summary pass error")
	}
}

func TestIndexerRunSkipsTickWhenLockHeld(t *testing.T) {
	store := &indexerFakeStore{} // tryLockOK false: another daemon holds the lock
	ix := NewIndexer(IndexerOptions{Store: store, Logger: indexerTestLogger(), Tick: 5 * time.Millisecond})
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
	if store.cursorsCalls != 0 {
		t.Errorf("ExtractCursors calls = %d, want 0 while the lock is held elsewhere", store.cursorsCalls)
	}
	if store.tryLockCalls == 0 {
		t.Error("no tick was attempted; the skip path was never exercised")
	}
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
	ix := NewIndexer(IndexerOptions{Store: store, Embedder: emb, Logger: indexerTestLogger(), Tick: 5 * time.Millisecond})
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
