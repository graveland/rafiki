package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestAgentToolAllowlistFlagsRoundTrip pins the whole --tools/--no-builtin-tools
// path for a fundi child: buildAgentArgv emits both flags from their
// SpawnRequest fields, parseAgentFlags (newAgentFlagSet) reads them back,
// toRuntimeOptions carries both into the engine options, and — because
// parseAgentFlags only exercises the pflag set — the `rafikid fundi` cobra
// command's own flag set must define them too, or the standalone process dies
// on an unknown flag the in-process path accepts.
func TestAgentToolAllowlistFlagsRoundTrip(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:           protocol.KindFundi,
		Model:          "anthropic/claude-sonnet-4-5",
		Tools:          "read,bash",
		NoBuiltinTools: true,
	}
	argv := buildAgentArgv(req, "c_tools", "/state")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--tools read,bash") {
		t.Fatalf("argv missing --tools read,bash: %v", argv)
	}
	if !strings.Contains(joined, "--no-builtin-tools") {
		t.Fatalf("argv missing --no-builtin-tools: %v", argv)
	}

	f, err := parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags(%q): %v", argv[1:], err)
	}
	if f.tools != "read,bash" {
		t.Errorf("f.tools = %q, want \"read,bash\"", f.tools)
	}
	if !f.noBuiltinTools {
		t.Error("f.noBuiltinTools = false, want true")
	}

	got, err := f.toRuntimeOptions(t.TempDir(), nil, false, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Tools != "read,bash" {
		t.Errorf("RuntimeOptions.Tools = %q, want \"read,bash\"", got.Tools)
	}
	if !got.NoBuiltinTools {
		t.Error("RuntimeOptions.NoBuiltinTools = false, want true")
	}

	// The cobra face must know both flags as well. parseAgentFlags is built on
	// newAgentFlagSet, so this Lookup is the only thing that fails when a flag
	// is registered in one flag set but not the other.
	cmd := newFundiCmd()
	for _, name := range []string{"tools", "no-builtin-tools"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("`rafikid fundi` does not define --%s; the flag is registered in only one of the two flag sets", name)
		}
	}
}
