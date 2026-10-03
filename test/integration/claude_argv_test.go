// SPDX-License-Identifier: Apache-2.0

package integration_test

// Proves the two claude argv pipelines cannot drift: one SpawnRequest, driven
// through the local-subprocess path and the daraja path, must produce
// byte-identical argv. This is the test whose absence let the two Params
// literals drift in the first place — AppendSystemPrompt and ExtraArgs were
// both dropped from the daraja path's wire type at birth, and nothing failed
// until a child spawned through an executor pool silently lost its system
// prompt appendix and operator flags.
//
// Both builders live in cmd/rafikid (a main package, unimportable from here),
// so each has been reduced to a delegation over an exported mapping, and THIS
// test drives the real mappings rather than copies:
//
//	local-subprocess path:  claudeargv.ParamsFromSpawnRequest → claudeargv.Build
//	                        (cmd/rafikid's buildClaudeArgv is exactly this
//	                        composition — see its doc comment, which requires
//	                        it to stay one)
//	daraja path:            daraja.ClaudeParamsForRequest → the wire ChildSpec
//	                        → daraja.SpecFromProto → daraja.ChildSpec.Argv
//	                        (cmd/rafikid's darajaClaudeParams delegates the
//	                        req→wire mapping to ClaudeParamsForRequest and
//	                        layers Controller-derived proxy fields on top)
//
// The Controller-layered fields (ProxyUrl/ProxyToken/PassthroughAuth/
// AutoCompactWindow) are deliberately absent from this comparison: they shape
// the daraja host's ENVIRONMENT (which proxyenv.ClaudeEnv builds from them —
// pinned in pkg/proxyenv and by the cmd_daraja wiring), never the child's
// argv, and SpecFromProto does not read them. RecordRequests IS set on the
// request to pin that it, too, stays out of argv on both paths.
//
// The proxy's argv decisions ride as proxyenv.Values, the same Values fed to
// both paths: MCPConfig is the bare inline JSON and ModelArgs the --model
// pair that must REPLACE the plain pair, so a proxied child carries exactly
// one of each.

import (
	"os"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/claudeargv"
	"go.graveland.dev/rafiki/pkg/daraja"
	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"

	"github.com/multigres/testkit/assert"
)

func TestClaudeArgvIdenticalAcrossPaths(t *testing.T) {
	t.Parallel()

	// Every argv-shaping field set to a distinct non-zero value. A non-empty
	// Model is what forces the ModelArgs pair (the gate is o.Model != "", not
	// model shape); the slash id exercises the custom-model-option spelling.
	req := protocol.SpawnRequest{
		Kind:               "claude",
		Model:              "anthropic/sonnet-latest",
		ResumeSession:      "sess-abc",
		AppendSystemPrompt: "be terse",
		ExtraArgs:          []string{"--foo", "bar"},
		RecordRequests:     true,
	}

	cases := []struct {
		name string
		vals proxyenv.Values
	}{
		{
			name: "proxied",
			vals: func() proxyenv.Values {
				// The same ClaudeEnv call the local path's proxyChildEnv and the
				// daraja host's own env build make; only its Values matter here.
				_, v := proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{
					URL:   "https://proxy.example.test:8443",
					Token: "proxy-bearer",
					Model: req.Model,
				})
				assert.NewAborting(t).False(v.MCPConfig == "" || len(v.ModelArgs) == 0, "proxied values = %+v, want MCPConfig and ModelArgs set", v)
				return v
			}(),
		},
		{
			name: "unproxied",
			vals: proxyenv.Values{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			// One shared stage dir: promptfile names a file by the SHA-256 of its
			// content, so both paths staging the SAME bytes land on the SAME path —
			// a shared dir lets the elided comparison below prove byte equality as
			// well as flag equality.
			dir := t.TempDir()

			// Local-subprocess path: buildClaudeArgv's body, verbatim, plus the
			// Stage the exec path runs immediately before Build.
			stagedLocal, err := claudeargv.Stage(claudeargv.ParamsFromSpawnRequest(req, tc.vals), dir)
			c.Require().NoError(err, "stage local launch files")
			local := claudeargv.Build(stagedLocal)

			// Daraja path: the wire spec production composes, the host maps and
			// rebuilds — the chain a Restart (and every launch through an executor
			// pool) actually runs. StagedArgv is the daraja host's own
			// stage-then-build, exactly what startLocked calls.
			wire := &darajapb.ChildSpec{
				Kind:   darajapb.Kind_KIND_CLAUDE,
				Claude: daraja.ClaudeParamsForRequest(req, tc.vals.MCPConfig != ""),
			}
			darajaArgv, err := daraja.SpecFromProto(wire).StagedArgv(tc.vals.MCPConfig, tc.vals.ModelArgs, dir)
			c.Require().NoError(err, "stage daraja launch files")
			c.Require().NotEmpty(darajaArgv, "daraja path produced no argv; Kind must be %q for SpecFromProto to map the spec", req.Kind)

			// Compare with the stage directory elided (the paths differ only in
			// the random temp root): outside it the two argvs must be identical,
			// and because file names are content hashes, equality here also
			// proves both paths staged identical bytes.
			c.Require().EqDiff(elideDir(dir, darajaArgv), elideDir(dir, local), "argv paths diverged:\n local")

			// The two staged files must hold identical bytes on both paths.
			localApp, localMCP := stagedPaths(t, local)
			darajaApp, darajaMCP := stagedPaths(t, darajaArgv)
			if tc.vals.MCPConfig != "" {
				c.Eq(readStagedFile(t, localMCP), readStagedFile(t, darajaMCP), "staged MCP config differs across paths")
			} else {
				c.Eq("", localMCP, "unproxied local argv carries an --mcp-config element: %q", local)
				c.Eq("", darajaMCP, "unproxied daraja argv carries an --mcp-config element: %q", darajaArgv)
			}
			c.Eq(readStagedFile(t, localApp), readStagedFile(t, darajaApp), "staged appendix differs across paths")

			// The properties the identity exists to protect, asserted on the
			// shared argv so a vacuous-equal pair cannot hide a lost flag.
			for _, want := range []string{
				"--resume",
				"--append-system-prompt-file",
				"--dangerously-skip-permissions",
			} {
				c.True(argvCarries(local, want), "argv %q missing %q", local, want)
			}
			c.NotContains(local, "--append-system-prompt", "argv %q still carries the inline append element", local)

			// The coordination prompt rides a proxied child exactly once, inside
			// the single staged appendix file, and never reaches an unproxied one
			// (there it would name tools that do not exist). Both halves of the
			// gate are the identity above; this is the property the gate protects.
			appendix := readStagedFile(t, localApp)
			c.StrContains(appendix, "be terse", "staged appendix")
			if tc.vals.MCPConfig != "" {
				c.StrContains(appendix, claudeargv.CoordinationPrompt, "proxied staged appendix carries no coordination prompt")
			} else {
				c.NotStrContains(appendix, claudeargv.CoordinationPrompt, "unproxied staged appendix carries the coordination prompt")
			}

			appendCount := 0
			for _, a := range local {
				if a == "--append-system-prompt-file" {
					appendCount++
				}
			}
			c.Eq(1, appendCount, "argv %q: want exactly one --append-system-prompt-file element, got", local)
		})
	}
}

// argvCarries reports whether argv contains want as a whole element. Pairs
// ("--resume", "sess-abc") are checked via their flag element; value elements
// are covered by the paired assertions below.
func argvCarries(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// stagedPaths returns the file paths argv's launch-file flags point at: the
// system-prompt appendix, and the MCP config (empty when the child is
// unproxied and carries none).
func stagedPaths(t *testing.T, argv []string) (appendix, mcp string) {
	t.Helper()
	for i, a := range argv {
		switch {
		case a == "--append-system-prompt-file" && i+1 < len(argv):
			appendix = argv[i+1]
		case strings.HasPrefix(a, "--mcp-config="):
			mcp = strings.TrimPrefix(a, "--mcp-config=")
		}
	}
	return appendix, mcp
}

// elideDir replaces the stage directory in every argv element with a
// placeholder, so two argvs differing only in their random temp root compare
// equal — while the content-hash file names they carry still must match.
func elideDir(dir string, argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, dir, "<stage>")
	}
	return out
}

// readStagedFile reads a staged launch file, failing the test if it is missing.
func readStagedFile(t *testing.T, path string) string {
	t.Helper()
	c := assert.NewCollecting(t)
	b, err := os.ReadFile(path)
	c.Require().NoError(err, "read staged file %s", path)
	return string(b)
}

// argvValue returns the element following flag in argv, or "" when the flag is
// absent or valueless — for asserting on the VALUE of a pair-flag such as
// --append-system-prompt, whose value is a merged text rather than a whole
// element worth finding with argvCarries.
func argvValue(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
