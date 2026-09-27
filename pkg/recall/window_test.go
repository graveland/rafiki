package recall

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multigres/testkit/assert"
)

// testMessages builds n non-Skip extracted messages of exactly runes runes,
// ordinals starting at from.
func testMessages(n, from, runes int) []ExtractedMessage {
	ms := make([]ExtractedMessage, n)
	for i := range ms {
		ms[i] = ExtractedMessage{
			Ordinal: from + i,
			Role:    "user",
			Text:    fmt.Sprintf("m%d ", from+i) + strings.Repeat("a", runes-len(fmt.Sprintf("m%d ", from+i))),
		}
	}
	return ms
}

func TestBuildWindowsSealsAtTarget(t *testing.T) {
	c := assert.NewAborting(t)
	ws := BuildWindows("c1", "u1", nil, testMessages(10, 0, 1000))
	c.Len(ws, 5, "windows = %d, want 5", len(ws))
	for i, w := range ws {
		c.Eq(i, w.Seq, "window")
		c.False(w.ConversationID != "c1" || w.OwnerUserID != "u1", "window %d conversation/owner = %q/%q", i, w.ConversationID, w.OwnerUserID)
		c.Eq(ExtractorVersion, w.ExtractorVersion, "window %d extractor version =", i)
		n := utf8.RuneCountInString(w.Text)
		c.LessOrEqual(WindowTargetChars, n, "window %d holds %d chars, over target", i, n)
		wantSealed := i < len(ws)-1
		c.Eq(wantSealed, w.Sealed, "window %d sealed = %v, want", i, w.Sealed)
	}
	for i := 0; i+1 < len(ws); i++ {
		c.Eq(ws[i].OrdinalTo, ws[i+1].OrdinalFrom, "overlap broken at %d: next starts %d, prev ends", i, ws[i+1].OrdinalFrom)
	}
}

func TestBuildWindowsRebuildsUnsealedTail(t *testing.T) {
	c := assert.NewAborting(t)
	tail := &Window{ID: "t", Seq: 3, Sealed: false, OrdinalFrom: 5, OrdinalTo: 7, ConversationID: "c1", OwnerUserID: "u1"}
	ws := BuildWindows("c1", "u1", tail, testMessages(4, 5, 500))
	c.Len(ws, 1, "windows = %d, want 1", len(ws))
	w := ws[0]
	c.False(w.Seq != 3 || w.ID != "t", "rebuild lost tail identity: seq %d id %q", w.Seq, w.ID)
	c.False(w.Sealed, "rebuilt tail must stay unsealed")
	c.False(w.OrdinalFrom != 5 || w.OrdinalTo != 8, "ordinals %d-%d, want 5-8", w.OrdinalFrom, w.OrdinalTo)
	c.Eq(ExtractorVersion, w.ExtractorVersion, "extractor version =")
}

func TestBuildWindowsSplitsHugeMessage(t *testing.T) {
	c := assert.NewAborting(t)
	ws := BuildWindows("c1", "u1", nil, []ExtractedMessage{
		{Ordinal: 0, Role: "assistant", Text: strings.Repeat("h", 10000)},
		{Ordinal: 1, Role: "user", Text: strings.Repeat("s", 50)},
	})
	c.GreaterOrEqual(3, len(ws), "windows")
	total := 0
	for i, w := range ws {
		n := utf8.RuneCountInString(w.Text)
		c.LessOrEqual(WindowTargetChars, n, "window %d holds %d chars, over target", i, n)
		c.False(i < len(ws)-1 && !w.Sealed, "window %d not sealed", i)
		c.False(i == len(ws)-1 && w.Sealed, "last window must be unsealed")
		total += utf8.RuneCountInString(w.Text)
	}
	c.Less(10000+2*50, total, "huge message repeated as overlap: total")
	for _, w := range ws[:len(ws)-1] {
		c.False(w.OrdinalFrom != 0 || w.OrdinalTo != 0, "chunk window spans ordinals %d-%d, want 0-0", w.OrdinalFrom, w.OrdinalTo)
	}
	if last := ws[len(ws)-1]; last.OrdinalFrom != 1 || last.OrdinalTo != 1 {
		t.Fatalf("tail window spans ordinals %d-%d, want 1-1", last.OrdinalFrom, last.OrdinalTo)
	}
}

func TestBuildWindowsSealedTailOnlyOverlapReturnsNil(t *testing.T) {
	tail := &Window{ID: "t", Seq: 2, Sealed: true, OrdinalTo: 7, ConversationID: "c1", OwnerUserID: "u1"}
	ws := BuildWindows("c1", "u1", tail, []ExtractedMessage{
		{Ordinal: 7, Role: "user", Text: "user: last message inside the sealed tail"},
		{Ordinal: 8, Role: "user", Text: "", Skip: true},
	})
	assert.NewAborting(t).Nil(ws, "expected nil, got %d windows", len(ws))
}

func TestBuildWindowsShortYesJoinsNeighbours(t *testing.T) {
	ws := BuildWindows("c1", "u1", nil, []ExtractedMessage{
		{Ordinal: 0, Role: "assistant", Text: "assistant: should we ship the recall package today?"},
		{Ordinal: 1, Role: "user", Text: "user: yes"},
	})
	assert.NewAborting(t).Len(ws, 1, "windows = %d, want 1", len(ws))
	if !strings.Contains(ws[0].Text, "should we ship") || !strings.Contains(ws[0].Text, "user: yes") {
		t.Fatalf("messages not joined: %q", ws[0].Text)
	}
}

func TestBuildWindowsSkipsOnlyMessagesReturnsNil(t *testing.T) {
	if ws := BuildWindows("c1", "u1", nil, []ExtractedMessage{
		{Ordinal: 0, Role: "user", Text: "", Skip: true},
		{Ordinal: 1, Role: "user", Text: "", Skip: true},
	}); ws != nil {
		t.Fatalf("expected nil for all-Skip input, got %d windows", len(ws))
	}
	ws := BuildWindows("c1", "u1", nil, nil)
	assert.NewAborting(t).Nil(ws, "expected nil for empty input, got %d windows", len(ws))
}

func TestEmbedHeaderFormat(t *testing.T) {
	c := assert.NewAborting(t)
	meta := ConversationMeta{
		Name: "fix login flow", Repo: "rafiki", Kind: "claude",
		CreatedAt: utcDate(2026, 1, 2),
	}
	want := "repo: rafiki · conversation: \"fix login flow\" · 2026-01-02 · claude\n\n"
	c.Eq(want, EmbedHeader(meta), "EmbedHeader")
	meta.Repo = ""
	want = "repo: - · conversation: \"fix login flow\" · 2026-01-02 · claude\n\n"
	c.Eq(want, EmbedHeader(meta), "EmbedHeader")
}
