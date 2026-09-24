package recall

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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
	ws := BuildWindows("c1", "u1", nil, testMessages(10, 0, 1000))
	if len(ws) != 5 {
		t.Fatalf("windows = %d, want 5", len(ws))
	}
	for i, w := range ws {
		if w.Seq != i {
			t.Fatalf("window %d has seq %d", i, w.Seq)
		}
		if w.ConversationID != "c1" || w.OwnerUserID != "u1" {
			t.Fatalf("window %d conversation/owner = %q/%q", i, w.ConversationID, w.OwnerUserID)
		}
		if w.ExtractorVersion != ExtractorVersion {
			t.Fatalf("window %d extractor version = %d", i, w.ExtractorVersion)
		}
		if n := utf8.RuneCountInString(w.Text); n > WindowTargetChars {
			t.Fatalf("window %d holds %d chars, over target", i, n)
		}
		wantSealed := i < len(ws)-1
		if w.Sealed != wantSealed {
			t.Fatalf("window %d sealed = %v, want %v", i, w.Sealed, wantSealed)
		}
	}
	for i := 0; i+1 < len(ws); i++ {
		if ws[i+1].OrdinalFrom != ws[i].OrdinalTo {
			t.Fatalf("overlap broken at %d: next starts %d, prev ends %d", i, ws[i+1].OrdinalFrom, ws[i].OrdinalTo)
		}
	}
}

func TestBuildWindowsRebuildsUnsealedTail(t *testing.T) {
	tail := &Window{ID: "t", Seq: 3, Sealed: false, OrdinalFrom: 5, OrdinalTo: 7, ConversationID: "c1", OwnerUserID: "u1"}
	ws := BuildWindows("c1", "u1", tail, testMessages(4, 5, 500))
	if len(ws) != 1 {
		t.Fatalf("windows = %d, want 1", len(ws))
	}
	w := ws[0]
	if w.Seq != 3 || w.ID != "t" {
		t.Fatalf("rebuild lost tail identity: seq %d id %q", w.Seq, w.ID)
	}
	if w.Sealed {
		t.Fatal("rebuilt tail must stay unsealed")
	}
	if w.OrdinalFrom != 5 || w.OrdinalTo != 8 {
		t.Fatalf("ordinals %d-%d, want 5-8", w.OrdinalFrom, w.OrdinalTo)
	}
	if w.ExtractorVersion != ExtractorVersion {
		t.Fatalf("extractor version = %d", w.ExtractorVersion)
	}
}

func TestBuildWindowsSplitsHugeMessage(t *testing.T) {
	ws := BuildWindows("c1", "u1", nil, []ExtractedMessage{
		{Ordinal: 0, Role: "assistant", Text: strings.Repeat("h", 10000)},
		{Ordinal: 1, Role: "user", Text: strings.Repeat("s", 50)},
	})
	if len(ws) < 3 {
		t.Fatalf("windows = %d, want >= 3", len(ws))
	}
	total := 0
	for i, w := range ws {
		if n := utf8.RuneCountInString(w.Text); n > WindowTargetChars {
			t.Fatalf("window %d holds %d chars, over target", i, n)
		}
		if i < len(ws)-1 && !w.Sealed {
			t.Fatalf("window %d not sealed", i)
		}
		if i == len(ws)-1 && w.Sealed {
			t.Fatal("last window must be unsealed")
		}
		total += utf8.RuneCountInString(w.Text)
	}
	if total >= 10000+2*50 {
		t.Fatalf("huge message repeated as overlap: total %d", total)
	}
	for _, w := range ws[:len(ws)-1] {
		if w.OrdinalFrom != 0 || w.OrdinalTo != 0 {
			t.Fatalf("chunk window spans ordinals %d-%d, want 0-0", w.OrdinalFrom, w.OrdinalTo)
		}
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
	if ws != nil {
		t.Fatalf("expected nil, got %d windows", len(ws))
	}
}

func TestBuildWindowsShortYesJoinsNeighbours(t *testing.T) {
	ws := BuildWindows("c1", "u1", nil, []ExtractedMessage{
		{Ordinal: 0, Role: "assistant", Text: "assistant: should we ship the recall package today?"},
		{Ordinal: 1, Role: "user", Text: "user: yes"},
	})
	if len(ws) != 1 {
		t.Fatalf("windows = %d, want 1", len(ws))
	}
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
	if ws := BuildWindows("c1", "u1", nil, nil); ws != nil {
		t.Fatalf("expected nil for empty input, got %d windows", len(ws))
	}
}

func TestEmbedHeaderFormat(t *testing.T) {
	meta := ConversationMeta{
		Name: "fix login flow", Repo: "rafiki", Kind: "claude",
		CreatedAt: utcDate(2026, 1, 2),
	}
	want := "repo: rafiki · conversation: \"fix login flow\" · 2026-01-02 · claude\n\n"
	if got := EmbedHeader(meta); got != want {
		t.Fatalf("EmbedHeader = %q, want %q", got, want)
	}
	meta.Repo = ""
	want = "repo: - · conversation: \"fix login flow\" · 2026-01-02 · claude\n\n"
	if got := EmbedHeader(meta); got != want {
		t.Fatalf("EmbedHeader = %q, want %q", got, want)
	}
}
