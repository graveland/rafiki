package recall

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func utcDate(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

// searchFakeStore satisfies Store for the search tests: only the Search*
// methods are wired, the rest embed the interface and are never called.
type searchFakeStore struct {
	Store
	bm25         map[Source][]Hit
	bm25Err      error
	vec          map[Source][]Hit
	vecErr       error
	bm25Called   []Source
	vecCalled    []Source
	lastQuery    SearchQuery
	lastVecModel string
	lastVec      []float32
}

func (f *searchFakeStore) SearchBM25(_ context.Context, q SearchQuery, src Source) ([]Hit, error) {
	f.bm25Called = append(f.bm25Called, src)
	f.lastQuery = q
	if f.bm25Err != nil {
		return nil, f.bm25Err
	}
	return f.bm25[src], nil
}

func (f *searchFakeStore) SearchVector(_ context.Context, q SearchQuery, src Source, model string, vec []float32) ([]Hit, error) {
	f.vecCalled = append(f.vecCalled, src)
	f.lastQuery = q
	f.lastVecModel = model
	f.lastVec = vec
	if f.vecErr != nil {
		return nil, f.vecErr
	}
	return f.vec[src], nil
}

// searchFakeEmbedder records Embed inputs; err fails every call.
type searchFakeEmbedder struct {
	model  string
	err    error
	vecs   [][]float32
	inputs [][]string
}

func (f *searchFakeEmbedder) Model() string   { return f.model }
func (f *searchFakeEmbedder) Dimensions() int { return 8 }
func (f *searchFakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	f.inputs = append(f.inputs, inputs)
	if f.err != nil {
		return nil, f.err
	}
	return f.vecs, nil
}

func TestScopeZeroValueAdmitsNothing(t *testing.T) {
	if (Scope{}).Valid() {
		t.Fatal("Scope{} must admit nothing")
	}
	if !(Scope{All: true}).Valid() {
		t.Fatal("Scope{All:true} must be valid")
	}
	if !(Scope{OwnerUserID: "u"}).Valid() {
		t.Fatal(`Scope{OwnerUserID:"u"} must be valid`)
	}
}

func TestParseHitIDRoundTripAndRejects(t *testing.T) {
	for src, uuid := range map[Source]string{
		SourceMemory: "mem-1", SourceSummary: "sum-2", SourceWindow: "win-3",
	} {
		id := HitID(src, uuid)
		gotSrc, gotUUID, err := ParseHitID(id)
		if err != nil || gotSrc != src || gotUUID != uuid {
			t.Fatalf("ParseHitID(%q) = %v, %q, %v", id, gotSrc, gotUUID, err)
		}
	}
	for _, bad := range []string{"x:1", "m:", "nocolon", ""} {
		_, _, err := ParseHitID(bad)
		if err == nil {
			t.Fatalf("ParseHitID(%q) accepted", bad)
		}
		if !strings.Contains(err.Error(), "recall: bad hit id") {
			t.Fatalf("ParseHitID(%q) error = %v", bad, err)
		}
	}
}

func TestFuseRRF(t *testing.T) {
	d1, d2 := utcDate(2026, 1, 2), utcDate(2026, 1, 3)
	lists := [][]Hit{
		{{ID: "m:a", Source: SourceMemory, When: d1}, {ID: "w:b", Source: SourceWindow, When: d2}, {ID: "s:c", Source: SourceSummary, When: d1}},
		{{ID: "w:b", Source: SourceWindow, When: d2}, {ID: "w:d", Source: SourceWindow, When: d1}, {ID: "w:e", Source: SourceWindow, When: d1}},
	}
	fused := Fuse(lists, 10)
	if len(fused) != 5 {
		t.Fatalf("fused %d hits, want 5", len(fused))
	}
	want := map[string]float64{
		"m:a": 1.0 / 61,
		"w:b": 1.0/62 + 1.0/61,
		"s:c": 1.0 / 63,
		"w:d": 1.0 / 62,
		"w:e": 1.0 / 63,
	}
	for _, h := range fused {
		if math.Abs(h.Score-want[h.ID]) > 1e-12 {
			t.Errorf("%s score %v, want %v", h.ID, h.Score, want[h.ID])
		}
	}
	gotIDs := ids(fused)
	wantIDs := []string{"w:b", "m:a", "w:d", "s:c", "w:e"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("order %v, want %v", gotIDs, wantIDs)
		}
	}
	if n := strings.Count(strings.Join(gotIDs, ","), "w:b"); n != 1 {
		t.Fatalf("w:b appears %d times", n)
	}
	if got := Fuse(lists, 2); len(got) != 2 || got[0].ID != "w:b" || got[1].ID != "m:a" {
		t.Fatalf("Fuse limit 2 = %v", ids(got))
	}
}

func ids(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestSearchDegradesToBM25OnEmbedError(t *testing.T) {
	st := &searchFakeStore{bm25: map[Source][]Hit{
		SourceWindow: {{ID: "w:1", Source: SourceWindow, Snippet: "hit", Rank: 1}},
	}}
	emb := &searchFakeEmbedder{model: "emb-1", err: errors.New("embed down")}
	q := SearchQuery{Scope: Scope{All: true}, Text: "needle", Sources: []Source{SourceWindow}}
	hits, err := Search(context.Background(), st, emb, q, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "w:1" {
		t.Fatalf("hits = %v", hits)
	}
	if len(emb.inputs) != 1 || len(emb.inputs[0]) != 1 || emb.inputs[0][0] != "needle" {
		t.Fatalf("embed inputs = %v", emb.inputs)
	}
	if len(st.vecCalled) != 0 {
		t.Fatalf("SearchVector called despite embed error: %v", st.vecCalled)
	}
}

func TestSearchSkipsMemoryWithoutOwner(t *testing.T) {
	st := &searchFakeStore{}
	q := SearchQuery{Scope: Scope{All: true}, Text: "needle"}
	if _, err := Search(context.Background(), st, nil, q, 5); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, src := range st.bm25Called {
		if src == SourceMemory {
			t.Fatal("memory searched without MemoryOwner")
		}
	}
	if len(st.bm25Called) != 2 {
		t.Fatalf("bm25 called for %v, want summary+window", st.bm25Called)
	}
	if st.lastQuery.Limit != 20 {
		t.Fatalf("store saw Limit %d, want 20 (5*4)", st.lastQuery.Limit)
	}

	st2 := &searchFakeStore{}
	q2 := SearchQuery{Scope: Scope{All: true}, MemoryOwner: "u1", Text: "needle"}
	if _, err := Search(context.Background(), st2, nil, q2, 5); err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, src := range st2.bm25Called {
		if src == SourceMemory {
			found = true
		}
	}
	if !found {
		t.Fatalf("memory not searched with MemoryOwner: %v", st2.bm25Called)
	}
}

func TestSearchFusesVectorWithBM25(t *testing.T) {
	st := &searchFakeStore{
		bm25: map[Source][]Hit{
			SourceWindow: {{ID: "w:1", Source: SourceWindow, When: utcDate(2026, 1, 2)}},
		},
		vec: map[Source][]Hit{
			SourceWindow: {{ID: "w:2", Source: SourceWindow, When: utcDate(2026, 1, 3)}, {ID: "w:1", Source: SourceWindow, When: utcDate(2026, 1, 2)}},
		},
	}
	emb := &searchFakeEmbedder{model: "emb-1", vecs: [][]float32{{0.1, 0.2}}}
	q := SearchQuery{Scope: Scope{All: true}, Text: "needle", Sources: []Source{SourceWindow}}
	hits, err := Search(context.Background(), st, emb, q, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %v", ids(hits))
	}
	// w:1 ranks 1 in BM25 and 2 in vector (summed); w:2 ranks 1 in vector only.
	if hits[0].ID != "w:1" || hits[1].ID != "w:2" {
		t.Fatalf("order %v, want w:1 then w:2", ids(hits))
	}
	if len(emb.inputs) != 1 {
		t.Fatalf("embed called %d times, want 1", len(emb.inputs))
	}
	if st.lastVecModel != "emb-1" || len(st.lastVec) != 2 {
		t.Fatalf("vector call model %q vec %v", st.lastVecModel, st.lastVec)
	}
}

func TestFormatHitsTruncates(t *testing.T) {
	hits := make([]Hit, 100)
	for i := range hits {
		hits[i] = Hit{
			ID:             HitID(SourceWindow, fmt.Sprintf("%03d", i)),
			Source:         SourceWindow,
			Snippet:        strings.Repeat("x", SnippetChars),
			When:           utcDate(2026, 1, 2),
			ConversationID: "conv1234567890",
			OrdinalFrom:    1,
			OrdinalTo:      2,
		}
	}
	out := FormatHits(hits)
	if len(out) > RecallMaxOutputChars+60 {
		t.Fatalf("output %d bytes exceeds cap %d", len(out), RecallMaxOutputChars+60)
	}
	if !strings.HasSuffix(out, "more hits truncated)") {
		t.Fatalf("missing truncation line in %q", out[len(out)-60:])
	}
	if strings.Count(out, "\n") < 2 {
		t.Fatalf("expected multiple hit lines, got %q", out)
	}
	if got := FormatHits(nil); got != "no matches" {
		t.Fatalf("FormatHits(nil) = %q", got)
	}
}

func TestFormatHitLines(t *testing.T) {
	when := utcDate(2026, 1, 2)
	memory := formatHit(Hit{ID: "m:1", Source: SourceMemory, Path: "project", Name: "api", Snippet: "the api", When: when})
	if want := "m:1  memory   project/api  2026-01-02  \"the api\""; memory != want {
		t.Fatalf("memory line = %q, want %q", memory, want)
	}
	summary := formatHit(Hit{ID: "s:2", Source: SourceSummary, Repo: "rafiki", Title: "fix", Snippet: "did it", When: when})
	if want := "s:2  summary  rafiki · 2026-01-02 · \"fix\"  \"did it\""; summary != want {
		t.Fatalf("summary line = %q, want %q", summary, want)
	}
	window := formatHit(Hit{ID: "w:3", Source: SourceWindow, ConversationID: "conv12345678", OrdinalFrom: 5, OrdinalTo: 9, Snippet: "the text", When: when})
	if want := "w:3  window   - · 2026-01-02 · conv conv1234…#5-9  \"the text\""; window != want {
		t.Fatalf("window line = %q, want %q", window, want)
	}
}
