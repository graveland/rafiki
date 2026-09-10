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
	"reflect"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/claudeargv"
	"go.graveland.dev/rafiki/pkg/daraja"
	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"
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
				if v.MCPConfig == "" || len(v.ModelArgs) == 0 {
					t.Fatalf("proxied values = %+v, want MCPConfig and ModelArgs set", v)
				}
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
			// Local-subprocess path: buildClaudeArgv's body, verbatim.
			local := claudeargv.Build(claudeargv.ParamsFromSpawnRequest(req, tc.vals))

			// Daraja path: the wire spec production composes, the host maps
			// and rebuilds — the chain a Restart (and every launch through an
			// executor pool) actually runs.
			wire := &darajapb.ChildSpec{
				Kind:   darajapb.Kind_KIND_CLAUDE,
				Claude: daraja.ClaudeParamsForRequest(req),
			}
			darajaArgv := daraja.SpecFromProto(wire).Argv(tc.vals.MCPConfig, tc.vals.ModelArgs)

			if len(darajaArgv) == 0 {
				t.Fatalf("daraja path produced no argv; Kind must be %q for SpecFromProto to map the spec", req.Kind)
			}
			if !reflect.DeepEqual(local, darajaArgv) {
				t.Fatalf("argv paths diverged:\n local: %q\ndaraja: %q", local, darajaArgv)
			}

			// The properties the identity exists to protect, asserted on the
			// shared argv so a vacuous-equal pair cannot hide a lost flag.
			for _, want := range []string{
				"--resume",
				"--append-system-prompt",
				"--dangerously-skip-permissions",
				"--mcp-config=" + tc.vals.MCPConfig,
			} {
				if tc.vals.MCPConfig == "" && strings.HasPrefix(want, "--mcp-config=") {
					continue // unproxied: the element must be absent, checked below
				}
				if !argvCarries(local, want) {
					t.Errorf("argv %q missing %q", local, want)
				}
			}
			if tc.vals.MCPConfig == "" && argvCarries(local, "--mcp-config=") {
				t.Errorf("unproxied argv carries an --mcp-config element: %q", local)
			}
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
