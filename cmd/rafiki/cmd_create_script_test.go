// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// profileFixtureWithModel returns a resolved profile whose default model is
// m — the declaration resolveSpawnModel must NOT forward to a script spawn.
func profileFixtureWithModel(t *testing.T, m string) profile.Resolved {
	t.Helper()
	return profile.Resolved{Profile: profile.Profile{Name: "p1", Model: m}}
}

// parseCreate parses the given flags/positionals on a full create command
// (RunE replaced with a no-op) and returns the command plus cobra's own
// positional view — the same args buildSpawnRequest receives after parsing,
// with ArgsLenAtDash set for the `--` split.
func parseCreate(t *testing.T, args ...string) (*cobra.Command, []string) {
	t.Helper()
	cmd := newCreateCmd()
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	assert.NewAborting(t).NoError(cmd.ParseFlags(args), "ParseFlags(%v)", args)
	return cmd, cmd.Flags().Args()
}

// TestCreateArgsDashRequiresScriptKind pins the positional contract: the
// after-`--` tail is the script child's argv, refused on an explicit
// non-script kind, accepted on --kind script, and the name positional stays
// capped at one in both worlds.
func TestCreateArgsDashRequiresScriptKind(t *testing.T) {
	t.Run("refused on --kind fundi", func(t *testing.T) {
		cmd, args := parseCreate(t, "--kind", "fundi", "name", "--", "arg")
		err := validateCreateArgs(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "require --kind script"), "err = %v, want the script-argv refusal", err)
	})
	t.Run("accepted on --kind script", func(t *testing.T) {
		cmd, args := parseCreate(t, "--kind", "script", "name", "--", "--fast", "5")
		assert.NewCollecting(t).NoError(validateCreateArgs(cmd, args), "err")
	})
	t.Run("more than one name refused", func(t *testing.T) {
		cmd, args := parseCreate(t, "a", "b")
		err := validateCreateArgs(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "at most 1 arg"), "err = %v, want the one-name cap", err)
	})
	t.Run("many script args accepted", func(t *testing.T) {
		cmd, args := parseCreate(t, "--kind", "script", "--", "a", "b", "c")
		assert.NewCollecting(t).NoError(validateCreateArgs(cmd, args), "err")
	})
}

// TestBuildSpawnRequest_ScriptSpec pins the full client-side script shaping:
// --pymodule becomes the ScriptSpec, the after-`--` tail becomes the script's
// argv, and a positional before `--` still names the child.
func TestBuildSpawnRequest_ScriptSpec(t *testing.T) {
	t.Run("spec and argv", func(t *testing.T) {
		c := assert.NewCollecting(t)
		cmd, args := parseCreate(t, "--cwd", "/tmp", "--kind", "script",
			"--pymodule", "local:driver", "--label", "env=work", "d1", "--", "--fast", "5")
		req, err := buildSpawnRequest(cmd, args)
		c.Require().NoError(err, "buildSpawnRequest")
		c.Eq(protocol.KindScript, req.Kind, "Kind")
		c.Eq("d1", req.Name, "Name")
		c.Require().False(req.Script == nil || req.Script.Repo != "local" || req.Script.Script != "driver", "Script = %+v, want {local driver}", req.Script)
		if len(req.Script.Args) != 2 || req.Script.Args[0] != "--fast" || req.Script.Args[1] != "5" {
			t.Errorf("Script.Args = %v, want [--fast 5]", req.Script.Args)
		}
		c.Eq("work", req.Labels["env"], "Labels = %v, want env=work", req.Labels)
	})
	t.Run("missing --pymodule refused", func(t *testing.T) {
		cmd, args := parseCreate(t, "--cwd", "/tmp", "--kind", "script")
		_, err := buildSpawnRequest(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "--pymodule"), "err = %v, want the --pymodule requirement", err)
	})
	t.Run("malformed --pymodule refused", func(t *testing.T) {
		cmd, args := parseCreate(t, "--cwd", "/tmp", "--kind", "script", "--pymodule", "driver")
		_, err := buildSpawnRequest(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "<repo>:<script>"), "err = %v, want the shape refusal", err)
	})
	t.Run("non-identifier script refused before any dial", func(t *testing.T) {
		cmd, args := parseCreate(t, "--cwd", "/tmp", "--kind", "script", "--pymodule", "local:bad name")
		_, err := buildSpawnRequest(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "bare Python identifier"), "err = %v, want the name rule", err)
	})
	t.Run("dash tail refused once the profile resolves the kind", func(t *testing.T) {
		// No --kind on the line: the flag check in validateCreateArgs cannot
		// fire, and buildSpawnRequest re-checks once the kind is fully
		// resolved (here the fundi default).
		cmd, args := parseCreate(t, "--cwd", "/tmp", "--", "arg")
		_, err := buildSpawnRequest(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "require --kind script"), "err = %v, want the resolved-kind re-check", err)
	})
	t.Run("no script arm on a fundi spawn", func(t *testing.T) {
		c := assert.NewCollecting(t)
		cmd, args := parseCreate(t, "--cwd", "/tmp", "name")
		req, err := buildSpawnRequest(cmd, args)
		c.Require().NoError(err, "buildSpawnRequest")
		c.Nil(req.Script, "Script")
	})
	t.Run("--pymodule without --kind script refused", func(t *testing.T) {
		// The fail-closed guard: --pymodule with the kind resolving non-script
		// (here the fundi default, because no --kind was typed) is refused
		// rather than silently dropped into a fundi spawn with the profile's
		// default model attached.
		cmd, args := parseCreate(t, "--cwd", "/tmp", "--pymodule", "local:driver")
		_, err := buildSpawnRequest(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "--kind script"), "err = %v, want the --kind script pointer", err)
	})
	t.Run("--pymodule with an explicit non-script kind refused", func(t *testing.T) {
		cmd, args := parseCreate(t, "--cwd", "/tmp", "--kind", "fundi", "--pymodule", "local:driver")
		_, err := buildSpawnRequest(cmd, args)
		assert.NewCollecting(t).False(err == nil || !strings.Contains(err.Error(), "--kind script"), "err = %v, want the --kind script pointer", err)
	})
}

// TestPymoduleInteractiveExclusive pins fix for the self-contradictory
// refusal: -i opens the create form, the form has no script-spec field, and
// without this exclusion `-i --kind script --pymodule local:driver` refused
// through the form branch with a "pass --pymodule" the caller had already
// passed. The flags are mutually exclusive at parse time, like -i and
// --prefill-files (TestPrefillFilesInteractiveExclusive).
func TestPymoduleInteractiveExclusive(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newCreateCmd()
	c.Require().NoError(cmd.Flags().Set("interactive", "true"))
	c.Require().NoError(cmd.Flags().Set("pymodule", "local:driver"))

	err := cmd.ValidateFlagGroups()
	c.Require().Error(err, "want -i and --pymodule to be rejected together, got none")
	c.False(!strings.Contains(err.Error(), "interactive") || !strings.Contains(err.Error(), "pymodule"), "error = %q, want it to name both flags", err)
}

// TestResolveSpawnModelSkipsScriptKind pins the model strip: a script kind
// gets neither the profile's default model nor the remembered one — both are
// inferences about LLM children and would make every script spawn fail on
// `field "model" does not apply` for a profile that names a default. A
// fundi kind keeps the chain unchanged.
func TestResolveSpawnModelSkipsScriptKind(t *testing.T) {
	p := profileFixtureWithModel(t, "anthropic/claude-sonnet-4")
	t.Run("script kind gets nothing inferred", func(t *testing.T) {
		req := protocol.SpawnRequest{Kind: protocol.KindScript}
		resolveSpawnModel(&req, p, "p1")
		assert.NewCollecting(t).Eq("", req.Model, "Model")
	})
	t.Run("fundi kind keeps the chain", func(t *testing.T) {
		req := protocol.SpawnRequest{Kind: protocol.KindFundi}
		resolveSpawnModel(&req, p, "p1")
		assert.NewCollecting(t).Eq("anthropic/claude-sonnet-4", req.Model, "Model")
	})
	t.Run("explicit flag travels on a script spawn", func(t *testing.T) {
		req := protocol.SpawnRequest{Kind: protocol.KindScript, Model: "anthropic/claude-sonnet-4"}
		resolveSpawnModel(&req, p, "p1")
		assert.NewCollecting(t).Eq("anthropic/claude-sonnet-4", req.Model, "Model")
	})
}
