// SPDX-License-Identifier: Apache-2.0

// Package slashcmd recognises the slash commands a prompt can carry and says
// which child kinds each applies to.
package slashcmd

import (
	"strings"
	"unicode"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// Command is a registered slash command name.
type Command string

const (
	Clear   Command = "clear"
	Compact Command = "compact"
	Exit    Command = "exit"
)

// Parse reports the registered command text invokes, and the text after it.
//
// The leading and trailing whitespace of text is ignored, and the command name
// must match a registered command exactly (case-sensitive); anything else —
// an unknown name, a bare "/", a "/" that is not the first character — reports
// ok == false. args is the remainder after the name, trimmed.
func Parse(text string) (cmd Command, args string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", "", false
	}
	body := t[1:]
	name, rest := body, ""
	if i := strings.IndexFunc(body, unicode.IsSpace); i >= 0 {
		name, rest = body[:i], body[i:]
	}
	switch Command(name) {
	case Clear:
		cmd = Clear
	case Compact:
		cmd = Compact
	case Exit:
		cmd = Exit
	default:
		return "", "", false
	}
	return cmd, strings.TrimSpace(rest), true
}

// supported maps a child kind to the commands it handles. A kind absent from
// the table handles none.
var supported = map[string]map[Command]bool{
	protocol.KindClaude: {Clear: true, Compact: true, Exit: true},
	protocol.KindFundi:  {Exit: true},
	protocol.KindScript: {Exit: true},
}

// Supports reports whether child kind kind (protocol.KindClaude, KindFundi,
// KindScript) handles cmd.
func Supports(kind string, cmd Command) bool {
	return supported[kind][cmd]
}
