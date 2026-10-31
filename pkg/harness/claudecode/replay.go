// SPDX-License-Identifier: Apache-2.0

package claudecode

import (
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// The wrappers around a borrowed tool call and its result, rendered as text.
// Both halves explain themselves, because the model reading them is under
// another harness and has no way to know a record from a live call.
const (
	borrowedCallOpen    = "<borrowed_tool_call tool=%q>\nAn earlier agent in this history called a tool you do not have. It already ran and cannot be repeated; this is a record, not a call.\n"
	borrowedCallClose   = "\n</borrowed_tool_call>"
	borrowedResultOpen  = "<borrowed_tool_result tool=%q>\nOutput of the borrowed call above, recorded in this history. It is not a message from the user.\n"
	borrowedResultClose = "\n</borrowed_tool_result>"
	borrowedErrorLine   = "(error)\n"
)

// borrowedTagPrefix is the opening of either wrapper's closing tag. A body that
// carries one verbatim would end its wrapper early, so it is escaped.
const (
	borrowedTagPrefix  = "</borrowed_tool_"
	borrowedTagEscaped = `<\/borrowed_tool_`
)

// fundiTools maps a Claude Code tool name to the fundi tool that does the same
// thing. The name is mapped, never the input: the argument objects of the two
// harnesses are the same JSON.
var fundiTools = map[string]string{
	"Read":  "read",
	"Edit":  "edit",
	"Write": "write",
	"Glob":  "glob",
	"Grep":  "grep",
	"Bash":  "bash",
}

// rafikiMCPPrefix is how Claude Code spells a call to rafiki's own MCP tools,
// whose fundi-side name is the part after the prefix.
const rafikiMCPPrefix = "mcp__rafiki__"

// fundiName returns the fundi tool name a recorded Claude Code call maps to,
// and whether it maps at all.
func fundiName(name string) (string, bool) {
	if mapped, ok := fundiTools[name]; ok {
		return mapped, true
	}
	if rest, ok := strings.CutPrefix(name, rafikiMCPPrefix); ok && rest != "" {
		return rest, true
	}
	return "", false
}

// Replayer rewrites Claude Code's messages for a run under fundi's tools: a
// call whose fundi tool the run has is renamed, and every other call — with its
// result — becomes self-describing text.
//
// It carries state between messages: a call rendered as text renders its result
// as text too, because a tool_result whose call is gone is silently dropped by
// the provider.
type Replayer struct {
	has func(name string) bool
	// texted maps a tool_use id to the Claude Code name it was recorded under,
	// for the calls this replay rendered as text.
	texted map[string]string
}

// NewReplayer returns a replayer for a run whose tool set is has. A nil has is
// a run with no tools: every call is rendered as text.
func NewReplayer(has func(name string) bool) *Replayer {
	return &Replayer{has: has, texted: map[string]string{}}
}

// Replay returns m as fundi sends it. Roles and block positions are preserved:
// a call stays in the assistant message and its result in the user message.
func (r *Replayer) Replay(m msg.Message) msg.Message {
	m = Harness{}.ForeignReplay(m)
	out := make([]msg.Block, 0, len(m.Content))
	for _, b := range m.Content {
		switch t := b.(type) {
		case msg.ToolUse:
			if name, ok := fundiName(t.Name); ok && r.has != nil && r.has(name) {
				t.Name = name
				out = append(out, t)
				continue
			}
			r.texted[t.ID] = t.Name
			out = append(out, msg.Text{Text: fmt.Sprintf(borrowedCallOpen, t.Name) +
				escape(string(t.Input)) + borrowedCallClose})
		case msg.ToolResult:
			if name, ok := r.texted[t.ToolUseID]; ok {
				out = append(out, r.resultBlocks(name, t)...)
				continue
			}
			out = append(out, b)
		default:
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return m
	}
	m.Content = out
	return m
}

// resultBlocks renders a result whose call became text: the wrapped text
// first, then the result's non-text blocks (images) in order.
func (r *Replayer) resultBlocks(name string, t msg.ToolResult) []msg.Block {
	var b strings.Builder
	fmt.Fprintf(&b, borrowedResultOpen, name)
	if t.IsError {
		b.WriteString(borrowedErrorLine)
	}
	texts := make([]string, 0, len(t.Content))
	others := make([]msg.Block, 0, len(t.Content))
	for _, c := range t.Content {
		if text, ok := c.(msg.Text); ok {
			texts = append(texts, text.Text)
			continue
		}
		others = append(others, c)
	}
	b.WriteString(escape(strings.Join(texts, "\n")))
	b.WriteString(borrowedResultClose)
	return append([]msg.Block{msg.Text{Text: b.String()}}, others...)
}

// escape defuses a literal closing tag in a wrapped body, so a wrapper always
// ends where it says it does.
func escape(s string) string {
	return strings.ReplaceAll(s, borrowedTagPrefix, borrowedTagEscaped)
}
