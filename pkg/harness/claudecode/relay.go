// SPDX-License-Identifier: Apache-2.0

package claudecode

import (
	"slices"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// Claude Code relays a message the user sends mid-turn inside a reminder, either
// appended to the next tool result's text or as a top-level block of its own.
// This only decides how the cockpit draws it, so recognition is by the fixed
// phrase alone: the reminder's exact framing, the trailer's wording and what
// surrounds the relay are all tolerated, and only a relay with no words of the
// user's in it is refused.
const (
	relayPhrase  = "The user sent a new message while you were working:"
	relayTrailer = "This is how Claude Code surfaces messages the user sends mid-turn"
	reminderOpen = "<system-reminder"
	reminderEnd  = "</system-reminder>"
)

// relayText splits s into everything around the relay and the user's own
// message, reporting whether s carries a relay at all. The last occurrence of
// the phrase inside a reminder is the relay; its body runs to the last trailer
// line, or to the reminder's end when there is none.
func relayText(s string) (before, user string, ok bool) {
	p := strings.LastIndex(s, relayPhrase)
	if p < 0 {
		return "", "", false
	}
	open := strings.LastIndex(s[:p], reminderOpen)
	if open < 0 {
		return "", "", false
	}
	gt := strings.IndexByte(s[open:p], '>')
	if gt < 0 || strings.TrimSpace(s[open+gt+1:p]) != "" {
		return "", "", false
	}
	body := s[p+len(relayPhrase):]
	after := ""
	if e := strings.Index(body, reminderEnd); e >= 0 {
		after = body[e+len(reminderEnd):]
		body = body[:e]
	}
	if j := strings.LastIndex(body, relayTrailer); j >= 0 {
		body = body[:j]
	}
	user = strings.TrimSpace(body)
	if user == "" {
		return "", "", false
	}
	before = strings.TrimRight(s[:open], "\n")
	if rest := strings.TrimSpace(after); rest != "" {
		if before != "" {
			before += "\n\n"
		}
		before += rest
	}
	return before, user, true
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
			if !ok {
				continue
			}
			if before == "" {
				blocks[i] = msg.Text{Text: user}
				continue
			}
			blocks[i] = msg.Text{Text: before}
			relays = append(relays, Relayed{Index: i, Text: user})
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
