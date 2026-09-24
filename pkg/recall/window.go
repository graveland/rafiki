package recall

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// BuildWindows slices extracted messages into windows of at most
// WindowTargetChars characters, overlapping consecutive windows by one
// message (skipped when that message exceeds half a window, so a huge message
// is never re-added forever; a message longer than a whole window is split
// into one sealed window per WindowTargetChars chunk).
//
// tail positions the build in the conversation: nil starts it (messages run
// from ordinal 0, windows from Seq 0); an unsealed tail is rebuilt in place
// from msgs (same Seq and ID, so result[0] replaces it); a sealed tail means
// msgs start at its last message (the overlap) and new windows start at
// Seq+1. The last returned window is always unsealed; nil means nothing to
// window.
func BuildWindows(conversationID, ownerUserID string, tail *Window, msgs []ExtractedMessage) []Window {
	b := &windowBuilder{conversationID: conversationID, ownerUserID: ownerUserID}
	switch {
	case tail == nil:
	case !tail.Sealed:
		b.seq = tail.Seq
		b.firstID = tail.ID
	default:
		b.seq = tail.Seq + 1
		if onlyOverlapLeft(msgs) {
			return nil
		}
	}
	last := -1
	for i, m := range msgs {
		if !m.Skip {
			last = i
		}
	}
	for i, m := range msgs {
		if m.Skip {
			continue
		}
		b.addMessage(m.Ordinal, m.Text, i == last)
	}
	if len(b.acc) > 0 {
		b.emit(false)
	}
	return b.out
}

// EmbedHeader renders the conversation-level prefix embedded ahead of every
// window and summary text, so vector search can match repo and conversation
// names.
func EmbedHeader(meta ConversationMeta) string {
	return fmt.Sprintf("repo: %s · conversation: %q · %s · %s\n\n",
		repoOr(meta.Repo), meta.Name, meta.CreatedAt.UTC().Format("2006-01-02"), meta.Kind)
}

func repoOr(repo string) string {
	if repo == "" {
		return "-"
	}
	return repo
}

// onlyOverlapLeft reports whether every message after the first — the sealed
// tail's overlap — is Skip, so no window beyond the overlap should be emitted.
func onlyOverlapLeft(msgs []ExtractedMessage) bool {
	for i := 1; i < len(msgs); i++ {
		if !msgs[i].Skip {
			return false
		}
	}
	return true
}

// wentry is one message (or message chunk) inside the window under
// construction.
type wentry struct {
	ordinal int
	text    string
	runes   int
}

// windowBuilder accumulates messages into windows, sealing each as it fills.
type windowBuilder struct {
	conversationID string
	ownerUserID    string
	out            []Window
	seq            int
	firstID        string // ID the first emitted window takes (tail rebuild)
	acc            []wentry
	count          int
	overlap        *wentry // last message of the previous sealed window; nil after chunk windows
}

// seal emits the window under construction, sealed, remembering its last
// message as the next window's overlap.
func (b *windowBuilder) seal() {
	if len(b.acc) == 0 {
		return
	}
	last := b.acc[len(b.acc)-1]
	b.emit(true)
	b.overlap = &last
}

// add appends one message, sealing the current window first when it would
// overflow, and seeding the fresh window with the one-message overlap when
// that fits.
func (b *windowBuilder) add(e wentry) {
	if len(b.acc) > 0 && b.count+e.runes > WindowTargetChars {
		b.seal()
	}
	if len(b.acc) == 0 && b.overlap != nil &&
		b.overlap.runes <= WindowTargetChars/2 &&
		b.overlap.runes+e.runes <= WindowTargetChars {
		b.acc = append(b.acc, *b.overlap)
		b.count = b.overlap.runes
	}
	b.acc = append(b.acc, e)
	b.count += e.runes
}

// addMessage adds one extracted message, splitting messages longer than a
// whole window. Every chunk becomes its own sealed window, except the final
// chunk of the final message, which stays in the unsealed tail like any other
// text. Chunk windows never seed an overlap.
func (b *windowBuilder) addMessage(ordinal int, text string, final bool) {
	n := utf8.RuneCountInString(text)
	if n <= WindowTargetChars {
		b.add(wentry{ordinal: ordinal, text: text, runes: n})
		return
	}
	b.seal()
	chunks := splitRunes(text, WindowTargetChars)
	for i, ch := range chunks {
		if final && i == len(chunks)-1 {
			b.add(wentry{ordinal: ordinal, text: ch, runes: utf8.RuneCountInString(ch)})
			continue
		}
		b.acc = []wentry{{ordinal: ordinal, text: ch, runes: utf8.RuneCountInString(ch)}}
		b.emit(true)
		b.overlap = nil
	}
}

// emit flushes the window under construction into the result.
func (b *windowBuilder) emit(sealed bool) {
	texts := make([]string, len(b.acc))
	for i, e := range b.acc {
		texts[i] = e.text
	}
	b.out = append(b.out, Window{
		ID:               b.firstID,
		ConversationID:   b.conversationID,
		OwnerUserID:      b.ownerUserID,
		Seq:              b.seq,
		OrdinalFrom:      b.acc[0].ordinal,
		OrdinalTo:        b.acc[len(b.acc)-1].ordinal,
		Text:             strings.Join(texts, "\n\n"),
		Sealed:           sealed,
		ExtractorVersion: ExtractorVersion,
	})
	b.firstID = ""
	b.seq++
	b.acc = nil
	b.count = 0
}

// splitRunes cuts s into chunks of at most n runes, on rune boundaries.
func splitRunes(s string, n int) []string {
	var out []string
	for i := 0; i < len(s); {
		j, c := i, 0
		for j < len(s) && c < n {
			_, sz := utf8.DecodeRuneInString(s[j:])
			j += sz
			c++
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}
