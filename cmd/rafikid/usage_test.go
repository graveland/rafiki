package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/multigres/testkit/assert"
)

// `rafikid fundi -h` must print usage. It previously exited 0 having printed
// NOTHING: parseAgentFlags sets the FlagSet output to io.Discard so the caller can
// report parse errors itself, which also silently swallowed the -h usage text.
func TestPrintAgentUsageListsFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	printAgentUsage(&buf)
	out := buf.String()

	c.Require().NotEq("", strings.TrimSpace(out), "printAgentUsage wrote nothing")
	// The flags a caller most needs to discover — notably -model, which is
	// required, so a user who can't see it can't run the command at all.
	for _, want := range []string{"model", "thinking", "skills-dir", "mcp-config", "db"} {
		c.StrContains(out, want, "usage missing flag")
	}
	c.StrContains(out, "rafikid fundi", "usage should name the command; got:\n")
}

// -h must be reported as flag.ErrHelp (so the caller can exit 0 and print
// usage) and must NOT be mistaken for a normal parse failure.
func TestParseAgentFlagsHelpReturnsErrHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		_, err := parseAgentFlags([]string{arg})
		assert.NewCollecting(t).True(errorsIsHelp(err), "parseAgentFlags(%q) error = %v, want flag.ErrHelp", arg, err)
	}
}

func errorsIsHelp(err error) bool {
	for err != nil {
		if err == pflag.ErrHelp {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestMCPConfigPrecedence_CwdBeatsGlobal covers task A6 step 4: a project's
// own .mcp.json must win over the machine-wide $RAFIKI_MCP_CONFIG fallback —
// getting this backwards would silently apply the wrong MCP servers to a
// project.
//
// Both the cwd file AND the global file are created and left existing: a
// version of resolveMCPConfig that checks global-first-then-cwd, gated only
// on the global file's existence, would still pass this test if the global
// file were absent (as in the plan's originally prescribed test, which never
// wrote a global.json). Writing both is what actually forces the ordering to
// matter.
func TestMCPConfigPrecedence_CwdBeatsGlobal(t *testing.T) {
	c := assert.NewAborting(t)
	cwd := t.TempDir()
	cwdCfg := filepath.Join(cwd, ".mcp.json")
	c.NoError(os.WriteFile(cwdCfg, []byte(`{"mcpServers":{}}`), 0o644))
	global := filepath.Join(t.TempDir(), "global.json")
	c.NoError(os.WriteFile(global, []byte(`{"mcpServers":{}}`), 0o644))
	t.Setenv("RAFIKI_MCP_CONFIG", global)

	c.Eq(cwdCfg, resolveMCPConfig("", cwd), "resolveMCPConfig")
}

// TestMCPConfigPrecedence_GlobalUsedWhenNoCwdFile covers the fallback half of
// the same precedence: paths.GlobalMCPConfig() (Task A2) was dead code until
// this wiring, so this proves it is actually reachable.
func TestMCPConfigPrecedence_GlobalUsedWhenNoCwdFile(t *testing.T) {
	c := assert.NewAborting(t)
	global := filepath.Join(t.TempDir(), "global.json")
	c.NoError(os.WriteFile(global, []byte(`{"mcpServers":{}}`), 0o644))
	t.Setenv("RAFIKI_MCP_CONFIG", global)

	c.Eq(global, resolveMCPConfig("", t.TempDir()), "resolveMCPConfig")
}

// TestMCPConfigPrecedence_ExplicitFlagWinsOutright confirms an explicit
// --mcp-config value is never second-guessed against either fallback, even
// when neither the cwd file nor the flag's own path exists — resolveMCPConfig
// does no existence check on an explicit value; runAgent's caller-side
// os.Stat handles "explicit but missing" as an error.
func TestMCPConfigPrecedence_ExplicitFlagWinsOutright(t *testing.T) {
	c := assert.NewAborting(t)
	cwd := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(cwd, ".mcp.json"), []byte(`{}`), 0o644))
	t.Setenv("RAFIKI_MCP_CONFIG", filepath.Join(t.TempDir(), "global.json"))

	explicit := "/explicit/.mcp.json"
	c.Eq(explicit, resolveMCPConfig(explicit, cwd), "resolveMCPConfig")
}
