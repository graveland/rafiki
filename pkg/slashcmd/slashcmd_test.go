package slashcmd

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		cmd  Command
		args string
		ok   bool
	}{
		{"clear", "/clear", Clear, "", true},
		{"clear trimmed", "  /clear  ", Clear, "", true},
		{"compact with args", "/compact keep the schema", Compact, "keep the schema", true},
		{"exit", "/exit", Exit, "", true},
		{"clear prefix is not a command", "/clearx", "", "", false},
		{"case sensitive", "/Clear", "", "", false},
		{"bare slash", "/", "", "", false},
		{"double slash", "//clear", "", "", false},
		{"no slash", "clear", "", "", false},
		{"slash not first", "hello /clear", "", "", false},
		{"unknown command", "/unknown", "", "", false},
		{"args after newline", "/compact\nline two", Compact, "line two", true},
		{"empty", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args, ok := Parse(tc.in)
			c := assert.NewCollecting(t)
			c.Eq(tc.cmd, cmd, "Parse(%q) cmd", tc.in)
			c.Eq(tc.args, args, "Parse(%q) args", tc.in)
			c.Eq(tc.ok, ok, "Parse(%q) ok", tc.in)
		})
	}
}

func TestSupports(t *testing.T) {
	cases := []struct {
		name string
		kind string
		cmd  Command
		want bool
	}{
		{"claude clear", protocol.KindClaude, Clear, true},
		{"claude compact", protocol.KindClaude, Compact, true},
		{"claude exit", protocol.KindClaude, Exit, true},
		{"fundi clear", protocol.KindFundi, Clear, true},
		{"fundi compact", protocol.KindFundi, Compact, true},
		{"fundi exit", protocol.KindFundi, Exit, true},
		{"script clear", protocol.KindScript, Clear, false},
		{"script compact", protocol.KindScript, Compact, false},
		{"script exit", protocol.KindScript, Exit, true},
		{"retired pi exit", "pi", Exit, false},
		{"empty kind exit", "", Exit, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Supports(tc.kind, tc.cmd)
			assert.NewCollecting(t).Eq(tc.want, got, "Supports(%q, %q) = %v, want", tc.kind, tc.cmd, got)
		})
	}
}
