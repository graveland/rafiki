package claudecode

import (
	"slices"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// Claude Code relays a message the user sends mid-turn by appending a reminder
// to the next tool result's text, or as a top-level block of its own. It is the
// ordinary reminder wrapper around a fixed opening and a fixed trailer, with
// the user's own words in between.
const (
	relayOpen    = "<system-reminder>\nThe user sent a new message while you were working:\n"
	relayTrailer = "\n\nThis is how Claude Code surfaces messages the user sends mid-turn"
)

// relayText splits s into the text before the relay and the user's own message,
// reporting whether s carries a relay at all. The relay must be the last thing
// in s; anything that does not match exactly is a near-miss, never a guess.
func relayText(s string) (before, user string, ok bool) {
	if strings.HasSuffix(s, "\n") && strings.HasSuffix(strings.TrimSuffix(s, "\n"), closeTag) {
		s = strings.TrimSuffix(s, "\n")
	}
	if !strings.HasSuffix(s, closeTag) {
		return "", "", false
	}
	i := strings.LastIndex(s, relayOpen)
	if i < 0 || i+len(relayOpen) > len(s)-len(closeTag) {
		return "", "", false
	}
	b := s[i+len(relayOpen) : len(s)-len(closeTag)]
	if strings.Contains(b, "<system-reminder>") || strings.Contains(b, "</system-reminder>") {
		return "", "", false
	}
	j := strings.LastIndex(b, relayTrailer)
	if j < 0 || strings.TrimSpace(b[:j]) == "" {
		return "", "", false
	}
	return strings.TrimRight(s[:i], "\n"), b[:j], true
}

// Relayed is a message the user sent mid-turn that Claude Code appended to the
// next content it returned. Index is the top-level block it was found in.
type Relayed struct {
	Index int
	Text  string
}

// SplitRelays returns m with a relayed mid-turn message taken out of the
// content it arrived in, and the messages taken out in block order. A relayed
// block that is nothing but the relay leaves no block behind. It clones Content
// and writes nothing to m, whose content the conv cache shares.
func (Harness) SplitRelays(m msg.Message) (msg.Message, []Relayed) {
	if m.Role != msg.User {
		return m, nil
	}
	blocks := slices.Clone(m.Content)
	var relays []Relayed
	for i, b := range blocks {
		switch t := b.(type) {
		case msg.Text:
			before, user, ok := relayText(t.Text)
			if !ok || before != "" {
				continue
			}
			blocks[i] = msg.Text{Text: user}
		case msg.ToolResult:
			if len(t.Content) == 0 {
				continue
			}
			last, ok := t.Content[len(t.Content)-1].(msg.Text)
			if !ok {
				continue
			}
			before, user, ok := relayText(last.Text)
			if !ok {
				continue
			}
			t.Content = slices.Clone(t.Content)
			if before == "" {
				t.Content = t.Content[:len(t.Content)-1]
			} else {
				last.Text = before
				t.Content[len(t.Content)-1] = last
			}
			blocks[i] = t
			relays = append(relays, Relayed{Index: i, Text: user})
		}
	}
	m.Content = blocks
	return m, relays
}
