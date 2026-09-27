package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestAgentToolAllowlistFlagsRoundTrip pins the whole --tools/--no-builtin-tools
// path for a fundi child: buildAgentArgv emits both flags from their
// SpawnRequest fields, parseAgentFlags (newAgentFlagSet) reads them back,
// toRuntimeOptions carries both into the engine options, and — because
// parseAgentFlags only exercises the pflag set — the `rafikid fundi` cobra
// command's own flag set must define them too, or the standalone process dies
// on an unknown flag the in-process path accepts.
func TestAgentToolAllowlistFlagsRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	req := protocol.SpawnRequest{
		Kind:           protocol.KindFundi,
		Model:          "anthropic/claude-sonnet-4-5",
		Tools:          "read,bash",
		NoBuiltinTools: true,
	}
	argv := buildAgentArgv(req, "c_tools", "/state")
	joined := strings.Join(argv, " ")
	c.Require().StrContains(joined, "--tools read,bash", "argv missing --tools read,bash: %v", argv)
	c.Require().StrContains(joined, "--no-builtin-tools", "argv missing --no-builtin-tools: %v", argv)

	f, err := parseAgentFlags(argv[1:])
	c.Require().NoError(err, "parseAgentFlags(%q)", argv[1:])
	c.Eq("read,bash", f.tools, "f.tools = %q, want \"read,bash\"", f.tools)
	c.True(f.noBuiltinTools, "f.noBuiltinTools = false, want true")

	got, err := f.toRuntimeOptions(t.TempDir(), nil, false, nil)
	c.Require().NoError(err, "toRuntimeOptions")
	c.Eq("read,bash", got.Tools, "RuntimeOptions.Tools = %q, want \"read,bash\"", got.Tools)
	c.True(got.NoBuiltinTools, "RuntimeOptions.NoBuiltinTools = false, want true")

	// The cobra face must know both flags as well. parseAgentFlags is built on
	// newAgentFlagSet, so this Lookup is the only thing that fails when a flag
	// is registered in one flag set but not the other.
	cmd := newFundiCmd()
	for _, name := range []string{"tools", "no-builtin-tools"} {
		c.NotNil(cmd.Flags().Lookup(name), "`rafikid fundi` does not define --%s; the flag is registered in only one of the two flag sets", name)
	}
}
