package recall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

type summarizerCompleteCall struct {
	owner, system, user string
	maxTokens           int
}

// summarizerFakeCompleter records Complete calls and returns scripted
// responses; ContextWindow is configurable.
type summarizerFakeCompleter struct {
	model    string
	window   int
	windowOK bool
	texts    []string // response per call; the last one repeats
	failAt   int      // call index that errors; -1 = none

	calls []summarizerCompleteCall
}

// summarizerCompleter builds a completer that never fails.
func summarizerCompleter(model string, texts ...string) *summarizerFakeCompleter {
	return &summarizerFakeCompleter{model: model, texts: texts, failAt: -1}
}

func (f *summarizerFakeCompleter) Model() string { return f.model }

func (f *summarizerFakeCompleter) ContextWindow() (int, bool) { return f.window, f.windowOK }

func (f *summarizerFakeCompleter) Complete(_ context.Context, owner, system, user string, maxTokens int) (Completion, error) {
	i := len(f.calls)
	f.calls = append(f.calls, summarizerCompleteCall{owner: owner, system: system, user: user, maxTokens: maxTokens})
	if f.failAt == i {
		return Completion{}, errors.New("completer exploded")
	}
	text := f.texts[len(f.texts)-1]
	if i < len(f.texts) {
		text = f.texts[i]
	}
	return Completion{Text: text, Model: f.model, InputTokens: 100, OutputTokens: 50, CostUSD: 0.05}, nil
}

type summarizerDeleteCall struct {
	conversationID string
	fromSeq        int
}

type summarizerMsgsFromCall struct {
	conversationID string
	from           int
}

type summarizerFailureCall struct {
	conversationID string
	promptVersion  int
	model, errText string
}

// summarizerFakeStore records the summary-related Store calls; interface
// methods the Pass never touches panic.
type summarizerFakeStore struct {
	state     map[string]string
	eligible  []ConversationMeta
	summaries map[string][]Summary
	msgs      map[string][]Message

	upserts  []Summary
	deleted  []summarizerDeleteCall
	failures []summarizerFailureCall
	msgsFrom []summarizerMsgsFromCall
}

func (f *summarizerFakeStore) GetState(_ context.Context, key string) (string, bool, error) {
	v, ok := f.state[key]
	return v, ok, nil
}

func (f *summarizerFakeStore) SetState(_ context.Context, key, value string) error {
	if f.state == nil {
		f.state = map[string]string{}
	}
	f.state[key] = value
	return nil
}

func (f *summarizerFakeStore) AddState(_ context.Context, key string, delta float64) error {
	if f.state == nil {
		f.state = map[string]string{}
	}
	n, _ := strconv.ParseFloat(f.state[key], 64)
	f.state[key] = strconv.FormatFloat(n+delta, 'f', -1, 64)
	return nil
}

func (f *summarizerFakeStore) EligibleForSummary(_ context.Context, _ []string, _ EligibleOpts) ([]ConversationMeta, error) {
	return f.eligible, nil
}

func (f *summarizerFakeStore) Summaries(_ context.Context, conversationID string) ([]Summary, error) {
	return f.summaries[conversationID], nil
}

func (f *summarizerFakeStore) MessagesFrom(_ context.Context, conversationID string, from int) ([]Message, error) {
	f.msgsFrom = append(f.msgsFrom, summarizerMsgsFromCall{conversationID: conversationID, from: from})
	return f.msgs[conversationID], nil
}

func (f *summarizerFakeStore) UpsertSummary(_ context.Context, s Summary) error {
	f.upserts = append(f.upserts, s)
	return nil
}

func (f *summarizerFakeStore) DeleteSegmentsFrom(_ context.Context, conversationID string, fromSeq int) error {
	f.deleted = append(f.deleted, summarizerDeleteCall{conversationID: conversationID, fromSeq: fromSeq})
	return nil
}

func (f *summarizerFakeStore) RecordSummaryFailure(_ context.Context, conversationID string, promptVersion int, model, errText string) error {
	f.failures = append(f.failures, summarizerFailureCall{
		conversationID: conversationID, promptVersion: promptVersion, model: model, errText: errText,
	})
	return nil
}

func (f *summarizerFakeStore) TryLock(context.Context) (func(), bool, error) {
	return func() {}, true, nil
}

func (f *summarizerFakeStore) unused(method string) {
	panic(method + ": unexpected call from summarizer test")
}

func (f *summarizerFakeStore) PutMemory(context.Context, string, Memory) (Memory, error) {
	f.unused("PutMemory")
	return Memory{}, nil
}

func (f *summarizerFakeStore) GetMemory(context.Context, string, string, string) (Memory, error) {
	f.unused("GetMemory")
	return Memory{}, nil
}

func (f *summarizerFakeStore) MemoryTree(context.Context, string, string, int) ([]Memory, error) {
	f.unused("MemoryTree")
	return nil, nil
}

func (f *summarizerFakeStore) DeleteMemory(context.Context, string, string, string) error {
	f.unused("DeleteMemory")
	return nil
}

func (f *summarizerFakeStore) SearchBM25(context.Context, SearchQuery, Source) ([]Hit, error) {
	f.unused("SearchBM25")
	return nil, nil
}

func (f *summarizerFakeStore) SearchVector(context.Context, SearchQuery, Source, string, []float32) ([]Hit, error) {
	f.unused("SearchVector")
	return nil, nil
}

func (f *summarizerFakeStore) Conversation(context.Context, Scope, string) (ConversationMeta, error) {
	f.unused("Conversation")
	return ConversationMeta{}, nil
}

func (f *summarizerFakeStore) Messages(context.Context, Scope, string, int, int) ([]Message, error) {
	f.unused("Messages")
	return nil, nil
}

func (f *summarizerFakeStore) Window(context.Context, Scope, string) (Window, error) {
	f.unused("Window")
	return Window{}, nil
}

func (f *summarizerFakeStore) Summary(context.Context, Scope, string) (Summary, error) {
	f.unused("Summary")
	return Summary{}, nil
}

func (f *summarizerFakeStore) MemoryByID(context.Context, string, string) (Memory, error) {
	f.unused("MemoryByID")
	return Memory{}, nil
}

func (f *summarizerFakeStore) ExtractCursors(context.Context, []string, int) ([]ExtractCursor, error) {
	f.unused("ExtractCursors")
	return nil, nil
}

func (f *summarizerFakeStore) WriteWindows(context.Context, string, []Window) error {
	f.unused("WriteWindows")
	return nil
}

func (f *summarizerFakeStore) PendingEmbeds(context.Context, string, int) ([]EmbedItem, error) {
	f.unused("PendingEmbeds")
	return nil, nil
}

func (f *summarizerFakeStore) SetEmbeddings(context.Context, string, []EmbedItem, [][]float32) error {
	f.unused("SetEmbeddings")
	return nil
}

func (f *summarizerFakeStore) EnsureVectorIndexes(context.Context, string, int) error {
	f.unused("EnsureVectorIndexes")
	return nil
}

func (f *summarizerFakeStore) Status(context.Context) (Status, error) {
	f.unused("Status")
	return Status{}, nil
}

func summarizerMsg(conv string, ordinal int, role, text string) Message {
	return Message{ConversationID: conv, Ordinal: ordinal, Role: role, Content: json.RawMessage(strconv.Quote(text))}
}

func summarizerCompaction(conv string, ordinal int) Message {
	return Message{ConversationID: conv, Ordinal: ordinal, Role: "user", Kind: "compaction_summary",
		Content: json.RawMessage(strconv.Quote("earlier conversation collapsed here"))}
}

// summarizerToolResult is a message whose only content is a tool_result
// block; Extract drops those, so a post-summary tail of only these builds no
// segments.
func summarizerToolResult(conv string, ordinal int) Message {
	return Message{ConversationID: conv, Ordinal: ordinal, Role: "tool", Kind: "tool_result",
		Content: json.RawMessage(`[{"type":"tool_result","content":"done"}]`)}
}

func summarizerConv(id string) ConversationMeta {
	return summarizerConvAt(id, utcDate(2026, 1, 2))
}

func summarizerConvAt(id string, lastAt time.Time) ConversationMeta {
	return ConversationMeta{ID: id, OwnerUserID: "u1", Name: "conv " + id, MaxOrdinal: 1000, LastAt: lastAt}
}

// summarizerEnabledAt seeds every store with a fixed summaries_enabled_at.
var summarizerEnabledAt = utcDate(2026, 1, 1)

func summarizerStore() *summarizerFakeStore {
	return &summarizerFakeStore{state: map[string]string{"summaries_enabled_at": summarizerEnabledAt.Format(time.RFC3339)}}
}

// summarizerWith fills in a quiet logger so fallback warnings stay out of
// test output.
func summarizerWith(o SummarizerOptions) *Summarizer {
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewSummarizer(o)
}

func TestSummarizerFirstPassOnlyRecordsEnabledAt(t *testing.T) {
	store := &summarizerFakeStore{eligible: []ConversationMeta{summarizerConv("c1")}}
	comp := summarizerCompleter("m", "TITLE: t\n\nbody")
	s := summarizerWith(SummarizerOptions{Store: store, Completer: comp, Now: func() time.Time { return utcDate(2026, 3, 4) }})
	if err := s.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	want := utcDate(2026, 3, 4).UTC().Format(time.RFC3339)
	if got := store.state["summaries_enabled_at"]; got != want {
		t.Fatalf("summaries_enabled_at = %q, want %q", got, want)
	}
	if len(comp.calls) != 0 {
		t.Fatalf("Complete calls = %d, want 0", len(comp.calls))
	}
	if len(store.upserts) != 0 {
		t.Fatalf("upserts = %d, want 0", len(store.upserts))
	}
}

func TestSummarizerSegmentBudgetDerivedFromContextWindow(t *testing.T) {
	window, fallback := 1_000_000, UnknownContextTokens
	s := summarizerWith(SummarizerOptions{Completer: &summarizerFakeCompleter{model: "m", window: window, windowOK: true, failAt: -1}})
	want := int(float64(window-SummaryPromptOverheadTokens-SummaryMaxOutputTokens) * 0.9)
	if got := s.SegmentBudgetTokens(); got != want {
		t.Fatalf("budget = %d, want %d", got, want)
	}
	s = summarizerWith(SummarizerOptions{
		Completer:        &summarizerFakeCompleter{model: "m", window: 1_000_000, windowOK: true, failAt: -1},
		MaxSegmentTokens: 200_000,
	})
	if got := s.SegmentBudgetTokens(); got != 200_000 {
		t.Fatalf("capped budget = %d, want 200000", got)
	}
	s = summarizerWith(SummarizerOptions{Completer: &summarizerFakeCompleter{model: "m", failAt: -1}})
	if got := s.SegmentBudgetTokens(); got != int(float64(fallback-SummaryPromptOverheadTokens-SummaryMaxOutputTokens)*0.9) {
		t.Fatalf("unknown-window budget = %d", got)
	}
	s = summarizerWith(SummarizerOptions{Completer: &summarizerFakeCompleter{model: "m", window: 5000, windowOK: true, failAt: -1}})
	if got := s.SegmentBudgetTokens(); got != 4000 {
		t.Fatalf("floored budget = %d, want 4000", got)
	}
}

func TestSummarizerSingleSegmentCopiesToConversationLevel(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	store.msgs = map[string][]Message{"c1": {
		summarizerMsg("c1", 0, "user", "please fix the login flow"),
		summarizerMsg("c1", 1, "assistant", "resetting the OAuth gate fixed the login flow"),
	}}
	comp := summarizerCompleter("m", "TITLE: Login fix\n\nFixed the login flow by resetting the OAuth gate.")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(comp.calls) != 1 {
		t.Fatalf("Complete calls = %d, want 1", len(comp.calls))
	}
	if len(store.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2", len(store.upserts))
	}
	seg, conv := store.upserts[0], store.upserts[1]
	if seg.Level != "segment" || seg.Seq != 0 || seg.OrdinalFrom != 0 || seg.OrdinalTo != 1 {
		t.Fatalf("segment row = %+v", seg)
	}
	if seg.Title != "Login fix" || seg.Summary != "Fixed the login flow by resetting the OAuth gate." {
		t.Fatalf("segment text = %q / %q", seg.Title, seg.Summary)
	}
	if seg.Model != "m" || seg.PromptVersion != SummaryPromptVersion || seg.CostUSD != 0.05 {
		t.Fatalf("segment meta = model %q version %d cost %v", seg.Model, seg.PromptVersion, seg.CostUSD)
	}
	if seg.InputTokens != 100 || seg.OutputTokens != 50 {
		t.Fatalf("segment tokens = %d/%d", seg.InputTokens, seg.OutputTokens)
	}
	if conv.Level != "conversation" || conv.Seq != 0 || conv.CostUSD != 0 {
		t.Fatalf("conversation row = %+v", conv)
	}
	if conv.Title != seg.Title || conv.Summary != seg.Summary || conv.OrdinalFrom != 0 || conv.OrdinalTo != 1 {
		t.Fatalf("conversation row not a copy: %+v", conv)
	}
}

func TestSummarizerRollingPassesStorySoFar(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	store.msgs = map[string][]Message{"c1": {
		summarizerMsg("c1", 0, "user", strings.Repeat("a", 9000)),
		summarizerMsg("c1", 1, "assistant", strings.Repeat("b", 9000)),
		summarizerMsg("c1", 2, "user", strings.Repeat("c", 9000)),
	}}
	comp := summarizerCompleter("m",
		"TITLE: one\n\none body",
		"TITLE: two\n\ntwo body",
		"TITLE: three\n\nthree body",
		"TITLE: all\n\nfull story",
	)
	// MaxSegmentTokens 4100 -> budget 16400 chars (over the 4000 floor); each
	// ~9000-char message fills a segment, so three segments roll plus one reduce.
	s := summarizerWith(SummarizerOptions{Store: store, Completer: comp, MaxSegmentTokens: 4100})
	if err := s.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(comp.calls) != 4 {
		t.Fatalf("Complete calls = %d, want 4 (3 rolling + reduce)", len(comp.calls))
	}
	for _, c := range comp.calls {
		if c.system != summarySegmentPrompt && c != comp.calls[3] {
			t.Fatalf("rolling call used the wrong system prompt: %q", c.system)
		}
		if c.maxTokens != SummaryMaxOutputTokens || c.owner != "u1" {
			t.Fatalf("call meta = %+v", c)
		}
	}
	if comp.calls[3].system != summaryReducePrompt {
		t.Fatalf("reduce system prompt = %q", comp.calls[3].system)
	}
	seg0 := "user: " + strings.Repeat("a", 9000)
	if comp.calls[0].user != "Next part of the conversation:\n"+seg0 {
		t.Fatalf("first segment prompt = %q", comp.calls[0].user)
	}
	want2 := "Story so far:\none body\n\n---\n\nNext part of the conversation:\nassistant: " + strings.Repeat("b", 9000)
	if comp.calls[1].user != want2 {
		t.Fatalf("second segment prompt = %q", comp.calls[1].user)
	}
	if !strings.Contains(comp.calls[1].user, "one body") {
		t.Fatalf("story so far missing: %q", comp.calls[1].user)
	}
	if !strings.Contains(comp.calls[3].user, "Part 1: one body") || !strings.Contains(comp.calls[3].user, "Part 3: three body") {
		t.Fatalf("reduce prompt missing parts: %q", comp.calls[3].user)
	}
	if len(store.upserts) != 4 {
		t.Fatalf("upserts = %d, want 4", len(store.upserts))
	}
	for i, up := range store.upserts[:3] {
		if up.Level != "segment" || up.Seq != i || up.OrdinalFrom != i || up.OrdinalTo != i {
			t.Fatalf("segment %d row = %+v", i, up)
		}
	}
	if conv := store.upserts[3]; conv.Level != "conversation" || conv.OrdinalFrom != 0 || conv.OrdinalTo != 2 {
		t.Fatalf("conversation row = %+v", conv)
	}
}

func TestSummarizerCutsAtCompactionBoundary(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	store.msgs = map[string][]Message{"c1": {
		summarizerMsg("c1", 0, "user", "first half of the work"),
		summarizerCompaction("c1", 1),
		summarizerMsg("c1", 2, "assistant", "resumed the work afterwards"),
	}}
	comp := summarizerCompleter("m",
		"TITLE: one\n\nfirst body",
		"TITLE: two\n\nsecond body",
		"TITLE: all\n\ncombined",
	)
	s := summarizerWith(SummarizerOptions{Store: store, Completer: comp})
	if err := s.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(comp.calls) != 3 {
		t.Fatalf("Complete calls = %d, want 3 (2 segments + reduce)", len(comp.calls))
	}
	if strings.Contains(comp.calls[0].user, "compaction") || strings.Contains(comp.calls[1].user, "compaction") {
		t.Fatal("compaction summary text leaked into a prompt")
	}
	if comp.calls[0].user != "Next part of the conversation:\nuser: first half of the work" {
		t.Fatalf("first segment prompt = %q", comp.calls[0].user)
	}
	want := "Story so far:\nfirst body\n\n---\n\nNext part of the conversation:\nassistant: resumed the work afterwards"
	if comp.calls[1].user != want {
		t.Fatalf("second segment prompt = %q, want %q", comp.calls[1].user, want)
	}
	if len(store.upserts) != 3 {
		t.Fatalf("upserts = %d, want 3", len(store.upserts))
	}
	if up := store.upserts[0]; up.OrdinalFrom != 0 || up.OrdinalTo != 0 {
		t.Fatalf("first segment spans %d-%d, want 0-0", up.OrdinalFrom, up.OrdinalTo)
	}
	if up := store.upserts[1]; up.OrdinalFrom != 2 || up.OrdinalTo != 2 {
		t.Fatalf("second segment spans %d-%d, want 2-2", up.OrdinalFrom, up.OrdinalTo)
	}
	if up := store.upserts[2]; up.Level != "conversation" || up.OrdinalFrom != 0 || up.OrdinalTo != 2 {
		t.Fatalf("conversation row = %+v", up)
	}
}

func TestSummarizerIncrementalOnlyNewTail(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	store.summaries = map[string][]Summary{"c1": {{
		ID: "s0", ConversationID: "c1", Level: "segment", Seq: 0, OrdinalFrom: 0, OrdinalTo: 50,
		Title: "old", Summary: "old tail body", PromptVersion: SummaryPromptVersion,
	}}}
	store.msgs = map[string][]Message{"c1": {summarizerMsg("c1", 51, "user", "the new tail message")}}
	comp := summarizerCompleter("m", "TITLE: new\n\nnew tail body", "TITLE: all\n\nwhole story")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(store.msgsFrom) != 1 || store.msgsFrom[0].from != 51 || store.msgsFrom[0].conversationID != "c1" {
		t.Fatalf("MessagesFrom calls = %+v, want [c1 51]", store.msgsFrom)
	}
	if len(comp.calls) != 2 {
		t.Fatalf("Complete calls = %d, want 2 (new segment + reduce)", len(comp.calls))
	}
	if len(store.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2", len(store.upserts))
	}
	seg := store.upserts[0]
	if seg.Level != "segment" || seg.Seq != 1 || seg.OrdinalFrom != 51 || seg.OrdinalTo != 51 {
		t.Fatalf("new segment row = %+v", seg)
	}
	conv := store.upserts[1]
	if conv.Level != "conversation" || conv.OrdinalFrom != 0 || conv.OrdinalTo != 51 {
		t.Fatalf("conversation row = %+v", conv)
	}
	if !strings.Contains(comp.calls[0].user, "old tail body") {
		t.Fatalf("rolling prompt missing kept story: %q", comp.calls[0].user)
	}
	if !strings.Contains(comp.calls[1].user, "Part 1: old tail body") || !strings.Contains(comp.calls[1].user, "Part 2: new tail body") {
		t.Fatalf("reduce prompt missing parts: %q", comp.calls[1].user)
	}
}

func TestSummarizerSkipsReduceWhenConversationRowCurrent(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	store.summaries = map[string][]Summary{"c1": {
		{ID: "s0", ConversationID: "c1", Level: "segment", Seq: 0, OrdinalFrom: 0, OrdinalTo: 50,
			Title: "one", Summary: "one body", PromptVersion: SummaryPromptVersion},
		{ID: "s1", ConversationID: "c1", Level: "segment", Seq: 1, OrdinalFrom: 51, OrdinalTo: 90,
			Title: "two", Summary: "two body", PromptVersion: SummaryPromptVersion},
		{ID: "c-row", ConversationID: "c1", Level: "conversation", Seq: 0, OrdinalFrom: 0, OrdinalTo: 90,
			Title: "all", Summary: "whole story", PromptVersion: SummaryPromptVersion},
	}}
	// Post-summary tail extracts to nothing (tool_result-only): no new
	// segments, and a current-version conversation row already covers the
	// chain, so this pass must cost nothing.
	store.msgs = map[string][]Message{"c1": {summarizerToolResult("c1", 91)}}
	comp := summarizerCompleter("m")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(comp.calls) != 0 {
		t.Fatalf("Complete calls = %d, want 0", len(comp.calls))
	}
	if len(store.upserts) != 0 {
		t.Fatalf("upserts = %d, want 0", len(store.upserts))
	}
}

func TestSummarizerReducesWhenConversationRowMissing(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	// Same shape as the skip case, but no conversation-level row: crash
	// recovery must still reduce the kept chain.
	store.summaries = map[string][]Summary{"c1": {
		{ID: "s0", ConversationID: "c1", Level: "segment", Seq: 0, OrdinalFrom: 0, OrdinalTo: 50,
			Title: "one", Summary: "one body", PromptVersion: SummaryPromptVersion},
		{ID: "s1", ConversationID: "c1", Level: "segment", Seq: 1, OrdinalFrom: 51, OrdinalTo: 90,
			Title: "two", Summary: "two body", PromptVersion: SummaryPromptVersion},
	}}
	store.msgs = map[string][]Message{"c1": {summarizerToolResult("c1", 91)}}
	comp := summarizerCompleter("m", "TITLE: all\n\nwhole story")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(comp.calls) != 1 {
		t.Fatalf("Complete calls = %d, want 1 (reduce)", len(comp.calls))
	}
	if comp.calls[0].system != summaryReducePrompt {
		t.Fatalf("reduce system prompt = %q", comp.calls[0].system)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(store.upserts))
	}
	conv := store.upserts[0]
	if conv.Level != "conversation" || conv.OrdinalFrom != 0 || conv.OrdinalTo != 90 {
		t.Fatalf("conversation row = %+v", conv)
	}
}

func TestSummarizerReducesWhenConversationRowStaleVersion(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	// Segments are current but the conversation-level row was written by an
	// older prompt version: the version check in hasCurrentConversationRow
	// must fall through and re-reduce the chain.
	store.summaries = map[string][]Summary{"c1": {
		{ID: "s0", ConversationID: "c1", Level: "segment", Seq: 0, OrdinalFrom: 0, OrdinalTo: 50,
			Title: "one", Summary: "one body", PromptVersion: SummaryPromptVersion},
		{ID: "s1", ConversationID: "c1", Level: "segment", Seq: 1, OrdinalFrom: 51, OrdinalTo: 90,
			Title: "two", Summary: "two body", PromptVersion: SummaryPromptVersion},
		{ID: "c-row", ConversationID: "c1", Level: "conversation", Seq: 0, OrdinalFrom: 0, OrdinalTo: 90,
			Title: "stale", Summary: "stale version", PromptVersion: SummaryPromptVersion - 1},
	}}
	store.msgs = map[string][]Message{"c1": {summarizerToolResult("c1", 91)}}
	comp := summarizerCompleter("m", "TITLE: all\n\nwhole story")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(comp.calls) != 1 {
		t.Fatalf("Complete calls = %d, want 1 (reduce)", len(comp.calls))
	}
	if comp.calls[0].system != summaryReducePrompt {
		t.Fatalf("reduce system prompt = %q", comp.calls[0].system)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(store.upserts))
	}
	conv := store.upserts[0]
	if conv.Level != "conversation" || conv.PromptVersion != SummaryPromptVersion || conv.OrdinalFrom != 0 || conv.OrdinalTo != 90 {
		t.Fatalf("conversation row = %+v", conv)
	}
}

func TestSummarizerReducesWhenConversationRowShort(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	// Crash between the two upserts: the conversation-level row is
	// current-version but stops at ordinal 50 while the kept chain runs to
	// 90. Coverage, not just version, must gate the skip.
	store.summaries = map[string][]Summary{"c1": {
		{ID: "s0", ConversationID: "c1", Level: "segment", Seq: 0, OrdinalFrom: 0, OrdinalTo: 50,
			Title: "one", Summary: "one body", PromptVersion: SummaryPromptVersion},
		{ID: "s1", ConversationID: "c1", Level: "segment", Seq: 1, OrdinalFrom: 51, OrdinalTo: 90,
			Title: "two", Summary: "two body", PromptVersion: SummaryPromptVersion},
		{ID: "c-row", ConversationID: "c1", Level: "conversation", Seq: 0, OrdinalFrom: 0, OrdinalTo: 50,
			Title: "partial", Summary: "partial story", PromptVersion: SummaryPromptVersion},
	}}
	store.msgs = map[string][]Message{"c1": {summarizerToolResult("c1", 91)}}
	comp := summarizerCompleter("m", "TITLE: all\n\nwhole story")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(comp.calls) != 1 {
		t.Fatalf("Complete calls = %d, want 1 (reduce)", len(comp.calls))
	}
	if comp.calls[0].system != summaryReducePrompt {
		t.Fatalf("reduce system prompt = %q", comp.calls[0].system)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(store.upserts))
	}
	conv := store.upserts[0]
	if conv.Level != "conversation" || conv.OrdinalTo != 90 {
		t.Fatalf("conversation row = %+v", conv)
	}
}

func TestSummarizerBackfillBudgetMissingWarns(t *testing.T) {
	// backfill_since armed with no budget key: the Warn must fire and
	// backfill_since must be cleared (budget reads as 0, spend 0 >= 0).
	for _, budget := range []struct{ name, value string }{{"missing", ""}, {"unparseable", "not-a-float"}} {
		store := summarizerStore()
		store.state["backfill_since"] = summarizerEnabledAt.Format(time.RFC3339)
		if budget.value != "" || budget.name == "unparseable" {
			store.state["backfill_budget_usd"] = budget.value
		}
		var buf bytes.Buffer
		s := summarizerWith(SummarizerOptions{Store: store, Completer: summarizerCompleter("m")})
		s.logger = slog.New(slog.NewTextHandler(&buf, nil))
		if err := s.Pass(context.Background()); err != nil {
			t.Fatalf("%s: Pass: %v", budget.name, err)
		}
		if got := store.state["backfill_since"]; got != "" {
			t.Fatalf("%s: backfill_since = %q, want cleared", budget.name, got)
		}
		if !strings.Contains(buf.String(), "backfill active without backfill_budget_usd") {
			t.Fatalf("%s: warn not logged: %q", budget.name, buf.String())
		}
	}

	// A set, parseable budget with spend under it: no warning, and
	// backfill_since stays armed.
	store := summarizerStore()
	store.state["backfill_since"] = summarizerEnabledAt.Format(time.RFC3339)
	store.state["backfill_budget_usd"] = "5"
	store.state["backfill_spent_usd"] = "1"
	var buf bytes.Buffer
	s := summarizerWith(SummarizerOptions{Store: store, Completer: summarizerCompleter("m")})
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))
	if err := s.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if got := store.state["backfill_since"]; got != summarizerEnabledAt.Format(time.RFC3339) {
		t.Fatalf("backfill_since = %q, want unchanged", got)
	}
	if buf.String() != "" {
		t.Fatalf("unexpected log output: %q", buf.String())
	}
}

func TestSummarizerPromptVersionBumpRestarts(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	store.summaries = map[string][]Summary{"c1": {{
		ID: "s9", ConversationID: "c1", Level: "segment", Seq: 0, OrdinalFrom: 0, OrdinalTo: 50,
		Summary: "stale version", PromptVersion: SummaryPromptVersion - 1,
	}}}
	store.msgs = map[string][]Message{"c1": {
		summarizerMsg("c1", 0, "user", "hello again"),
		summarizerMsg("c1", 1, "assistant", "starting over"),
	}}
	comp := summarizerCompleter("m", "TITLE: fresh\n\nrestarted body")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0].conversationID != "c1" || store.deleted[0].fromSeq != 0 {
		t.Fatalf("DeleteSegmentsFrom calls = %+v, want [c1 0]", store.deleted)
	}
	if len(store.msgsFrom) != 1 || store.msgsFrom[0].from != 0 {
		t.Fatalf("MessagesFrom = %+v, want from 0", store.msgsFrom)
	}
	if len(comp.calls) != 1 {
		t.Fatalf("Complete calls = %d, want 1", len(comp.calls))
	}
	if !strings.HasPrefix(comp.calls[0].user, "Next part of the conversation:\n") {
		t.Fatalf("restart must not roll a stale story so far: %q", comp.calls[0].user)
	}
	if len(store.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2", len(store.upserts))
	}
	seg := store.upserts[0]
	if seg.Level != "segment" || seg.Seq != 0 || seg.OrdinalFrom != 0 || seg.OrdinalTo != 1 {
		t.Fatalf("restarted segment row = %+v", seg)
	}
	if up := store.upserts[1]; up.Level != "conversation" || up.OrdinalFrom != 0 || up.OrdinalTo != 1 {
		t.Fatalf("conversation row = %+v", up)
	}
}

func TestSummarizerFailureRecorded(t *testing.T) {
	store := summarizerStore()
	store.eligible = []ConversationMeta{summarizerConv("c1"), summarizerConv("c2")}
	store.msgs = map[string][]Message{
		"c1": {summarizerMsg("c1", 0, "user", "first conversation text")},
		"c2": {summarizerMsg("c2", 0, "user", "second conversation text")},
	}
	comp := summarizerCompleter("m", "TITLE: ok\n\nbody")
	comp.failAt = 0
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(store.failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(store.failures))
	}
	f := store.failures[0]
	if f.conversationID != "c1" || f.promptVersion != SummaryPromptVersion || f.model != "m" || !strings.Contains(f.errText, "completer exploded") {
		t.Fatalf("failure record = %+v", f)
	}
	if len(store.upserts) < 2 {
		t.Fatalf("c2 not processed: upserts = %+v", store.upserts)
	}
	for _, up := range store.upserts {
		if up.ConversationID != "c2" {
			t.Fatalf("c1 was not skipped: upsert %+v", up)
		}
	}
}

func TestSummarizerBackfillBudgetStops(t *testing.T) {
	store := summarizerStore()
	store.state["backfill_since"] = summarizerEnabledAt.Format(time.RFC3339)
	store.state["backfill_budget_usd"] = "5"
	store.state["backfill_spent_usd"] = "5"
	store.eligible = []ConversationMeta{summarizerConv("c1")}
	store.msgs = map[string][]Message{"c1": {summarizerMsg("c1", 0, "user", "some text")}}
	comp := summarizerCompleter("m", "TITLE: t\n\nbody", "TITLE: all\n\nall")
	if err := summarizerWith(SummarizerOptions{Store: store, Completer: comp}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if got := store.state["backfill_since"]; got != "" {
		t.Fatalf("backfill_since = %q, want cleared", got)
	}

	// Under budget: backfill_since stays, and a conversation whose last
	// activity predates summaries_enabled_at has its spend accounted.
	store2 := summarizerStore()
	store2.state["backfill_since"] = summarizerEnabledAt.Format(time.RFC3339)
	store2.state["backfill_budget_usd"] = "5"
	store2.state["backfill_spent_usd"] = "4"
	store2.eligible = []ConversationMeta{summarizerConvAt("c1", utcDate(2025, 12, 31))}
	store2.msgs = map[string][]Message{"c1": {summarizerMsg("c1", 0, "user", "backfill text")}}
	comp2 := summarizerCompleter("m", "TITLE: t\n\nbody")
	if err := summarizerWith(SummarizerOptions{Store: store2, Completer: comp2}).Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if got := store2.state["backfill_since"]; got != summarizerEnabledAt.Format(time.RFC3339) {
		t.Fatalf("backfill_since = %q, want unchanged", got)
	}
	spent, err := strconv.ParseFloat(store2.state["backfill_spent_usd"], 64)
	if err != nil || math.Abs(spent-4.05) > 1e-9 {
		t.Fatalf("backfill_spent_usd = %v (err %v), want 4.05", spent, err)
	}
}

func TestSummarizerParsesTitle(t *testing.T) {
	title, body := parseSummaryOutput("TITLE: Login fix\n\nFixed the login flow by resetting the OAuth gate.")
	if title != "Login fix" || body != "Fixed the login flow by resetting the OAuth gate." {
		t.Fatalf("titled parse = %q / %q", title, body)
	}
	title, body = parseSummaryOutput("TITLE:   spaced title \n\nbody")
	if title != "spaced title" || body != "body" {
		t.Fatalf("trimmed parse = %q / %q", title, body)
	}
	long := strings.Repeat("x", 100)
	title, body = parseSummaryOutput(long)
	if title != strings.Repeat("x", 80) || body != long {
		t.Fatalf("untitled parse = %q / %q", title, body)
	}
	if title, body = parseSummaryOutput(""); title != "" || body != "" {
		t.Fatalf("empty parse = %q / %q", title, body)
	}
}
