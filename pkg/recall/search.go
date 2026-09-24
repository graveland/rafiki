package recall

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Fuse merges per-source ranked lists with reciprocal rank fusion: each hit
// scores 1/(RRFK+rank) per list that contains it. Duplicate ids keep the
// first-seen hit's fields; the result is sorted by score, then recency, then
// id, and truncated to limit.
func Fuse(lists [][]Hit, limit int) []Hit {
	type scored struct {
		hit   Hit
		score float64
	}
	byID := make(map[string]*scored)
	for _, list := range lists {
		for rank, h := range list {
			s, ok := byID[h.ID]
			if !ok {
				s = &scored{hit: h}
				byID[h.ID] = s
			}
			s.score += 1.0 / float64(RRFK+rank+1)
		}
	}
	out := make([]Hit, 0, len(byID))
	for _, s := range byID {
		s.hit.Score = s.score
		out = append(out, s.hit)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if !out[i].When.Equal(out[j].When) {
			return out[i].When.After(out[j].When)
		}
		return out[i].ID < out[j].ID
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Search runs one BM25 list per requested source, adds vector lists when an
// embedder is available, and fuses the lists. Embedding failures degrade to
// BM25-only; they surface in the returned error only when BM25 fails too.
func Search(ctx context.Context, st Store, emb Embedder, q SearchQuery, limit int) ([]Hit, error) {
	limit = clampLimit(limit)
	q.Limit = limit * 4

	sources := q.Sources
	if len(sources) == 0 {
		sources = []Source{SourceMemory, SourceSummary, SourceWindow}
	}

	var vec []float32
	var embErr error
	if emb != nil {
		vecs, err := emb.Embed(ctx, []string{q.Text})
		if err != nil {
			embErr = err
		} else if len(vecs) > 0 {
			vec = vecs[0]
		}
	}

	var (
		lists   [][]Hit
		bm25Err error
		vecErr  error
	)
	for _, src := range sources {
		if src == SourceMemory && q.MemoryOwner == "" {
			continue
		}
		hits, err := st.SearchBM25(ctx, q, src)
		if err != nil {
			if bm25Err == nil {
				bm25Err = err
			}
			continue
		}
		lists = append(lists, hits)
		if emb == nil || vec == nil {
			continue
		}
		vhits, err := st.SearchVector(ctx, q, src, emb.Model(), vec)
		if err != nil {
			if vecErr == nil {
				vecErr = err
			}
			continue
		}
		lists = append(lists, vhits)
	}
	if bm25Err != nil {
		return nil, errors.Join(bm25Err, embErr, vecErr)
	}
	return Fuse(lists, limit), nil
}

// clampLimit forces limit into [1, RecallMaxLimit], mapping 0 to the default.
func clampLimit(limit int) int {
	switch {
	case limit == 0:
		return RecallDefaultLimit
	case limit < 0:
		return 1
	case limit > RecallMaxLimit:
		return RecallMaxLimit
	}
	return limit
}

// Snippet renders up to SnippetChars runes of text centred on the first
// case-insensitive occurrence of any query term of three or more characters,
// with cut ends marked and newlines collapsed. Without a match it renders the
// head of the text.
func Snippet(text, query string) string {
	runes := []rune(text)
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	centre := -1
	for _, term := range strings.Fields(query) {
		t := []rune(term)
		for i, r := range t {
			t[i] = unicode.ToLower(r)
		}
		if len(t) < 3 {
			continue
		}
		if p := indexRunes(lower, t); p >= 0 && (centre < 0 || p < centre) {
			centre = p
		}
	}
	from := 0
	if centre >= 0 {
		from = centre - SnippetChars/2
	}
	if from < 0 {
		from = 0
	}
	to := from + SnippetChars
	if to > len(runes) {
		to = len(runes)
		if from = to - SnippetChars; from < 0 {
			from = 0
		}
	}
	out := make([]rune, 0, to-from)
	for _, r := range runes[from:to] {
		if r == '\n' || r == '\r' {
			r = ' '
		}
		out = append(out, r)
	}
	s := string(out)
	if from > 0 {
		s = "…" + s
	}
	if to < len(runes) {
		s += "…"
	}
	return s
}

// indexRunes finds the first occurrence of needle in haystack, both rune
// slices, or -1.
func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// FormatHits renders fused hits as one line each, stopping once the output
// would exceed RecallMaxOutputChars; truncated hits are counted in a trailing
// line.
func FormatHits(hits []Hit) string {
	if len(hits) == 0 {
		return "no matches"
	}
	var b strings.Builder
	shown := 0
	for _, h := range hits {
		line := formatHit(h)
		need := len(line)
		if b.Len() > 0 {
			need++
		}
		if b.Len()+need > RecallMaxOutputChars {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		shown++
	}
	if shown < len(hits) {
		fmt.Fprintf(&b, "\n… (%d more hits truncated)", len(hits)-shown)
	}
	return b.String()
}

func formatHit(h Hit) string {
	date := h.When.UTC().Format("2006-01-02")
	switch h.Source {
	case SourceMemory:
		return fmt.Sprintf("%s  %-7s  %s/%s  %s  %q", h.ID, h.Source, h.Path, h.Name, date, h.Snippet)
	case SourceSummary:
		return fmt.Sprintf("%s  %-7s  %s · %s · %q  %q", h.ID, h.Source, repoOr(h.Repo), date, h.Title, h.Snippet)
	case SourceWindow:
		return fmt.Sprintf("%s  %-7s  %s · %s · conv %s…#%d-%d  %q",
			h.ID, h.Source, repoOr(h.Repo), date, convPrefix(h.ConversationID), h.OrdinalFrom, h.OrdinalTo, h.Snippet)
	}
	return fmt.Sprintf("%s  %-7s  %q", h.ID, h.Source, h.Snippet)
}

// convPrefix shortens a conversation id to its first 8 characters.
func convPrefix(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
