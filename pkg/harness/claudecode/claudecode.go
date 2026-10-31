// Package claudecode is Claude Code's own view of the content it wrote: which
// text blocks in a user message are its injected reminders, and which of those
// reminders are false once the history is replayed under another harness.
//
// It is the only code that knows the wrapper's shape. It satisfies
// harness.Harness structurally and must not import pkg/harness.
package claudecode

import (
	"slices"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// The wrapper Claude Code puts around every reminder it injects into a user
// message, and the opening of the one reminder that describes the running model
// rather than the conversation — true only where Claude Code itself runs.
const (
	open          = "<system-reminder>\n"
	closeTag      = "\n</system-reminder>"
	modelReminder = "You are powered by the model named "
)

// Harness is Claude Code's view of its own content.
type Harness struct{}

// body returns s's reminder body and whether s is exactly a reminder:
// open + body + closeTag, optionally followed by one "\n". A body that itself
// carries a tag is a near-miss, never a reminder.
func body(s string) (string, bool) {
	if strings.HasSuffix(s, "\n") && strings.HasSuffix(strings.TrimSuffix(s, "\n"), closeTag) {
		s = strings.TrimSuffix(s, "\n")
	}
	if !strings.HasPrefix(s, open) || !strings.HasSuffix(s, closeTag) {
		return "", false
	}
	if len(s) < len(open)+len(closeTag) {
		return "", false
	}
	b := s[len(open) : len(s)-len(closeTag)]
	if strings.Contains(b, "<system-reminder>") || strings.Contains(b, "</system-reminder>") {
		return "", false
	}
	return b, true
}

// Mark marks every top-level text block of a user message that is exactly a
// reminder. Nothing is dropped, no block is reordered, and m is not written to:
// the returned message carries a new Content slice.
func (Harness) Mark(m msg.Message) msg.Message {
	if m.Role != msg.User {
		return m
	}
	blocks := slices.Clone(m.Content)
	for i, b := range blocks {
		t, ok := b.(msg.Text)
		if !ok {
			continue
		}
		if _, ok := body(t.Text); ok {
			t.Injected = true
			blocks[i] = t
		}
	}
	m.Content = blocks
	return m
}

// ForeignReplay marks m and drops the model reminder, which is false in a run
// under another harness. It never empties a message: if every block would be
// dropped, nothing is.
func (h Harness) ForeignReplay(m msg.Message) msg.Message {
	marked := h.Mark(m)
	kept := make([]msg.Block, 0, len(marked.Content))
	for _, b := range marked.Content {
		if t, ok := b.(msg.Text); ok && t.Injected {
			if bd, ok := body(t.Text); ok && strings.HasPrefix(bd, modelReminder) {
				continue
			}
		}
		kept = append(kept, b)
	}
	if len(kept) == 0 {
		return marked
	}
	marked.Content = kept
	return marked
}

// Unwrap returns a reminder's body for display, and any other text unchanged.
func (Harness) Unwrap(t msg.Text) string {
	if b, ok := body(t.Text); ok {
		return b
	}
	return t.Text
}
