package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// `rafikid fundi -h` must print usage. It previously exited 0 having printed
// NOTHING: parseAgentFlags sets the FlagSet output to io.Discard so the caller can
// report parse errors itself, which also silently swallowed the -h usage text.
func TestPrintAgentUsageListsFlags(t *testing.T) {
	var buf bytes.Buffer
	printAgentUsage(&buf)
	out := buf.String()

	if strings.TrimSpace(out) == "" {
		t.Fatal("printAgentUsage wrote nothing")
	}
	// The flags a caller most needs to discover — notably -model, which is
	// required, so a user who can't see it can't run the command at all.
	for _, want := range []string{"model", "thinking", "skills-dir", "mcp-config", "db"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing flag %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "rafikid fundi") {
		t.Errorf("usage should name the command; got:\n%s", out)
	}
}

// -h must be reported as flag.ErrHelp (so the caller can exit 0 and print
// usage) and must NOT be mistaken for a normal parse failure.
func TestParseAgentFlagsHelpReturnsErrHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		if _, err := parseAgentFlags([]string{arg}); !errorsIsHelp(err) {
			t.Errorf("parseAgentFlags(%q) error = %v, want flag.ErrHelp", arg, err)
		}
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
	cwd := t.TempDir()
	cwdCfg := filepath.Join(cwd, ".mcp.json")
	if err := os.WriteFile(cwdCfg, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "global.json")
	if err := os.WriteFile(global, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_MCP_CONFIG", global)

	if got := resolveMCPConfig("", cwd); got != cwdCfg {
		t.Fatalf("resolveMCPConfig = %q, want the cwd file %q", got, cwdCfg)
	}
}

// TestMCPConfigPrecedence_GlobalUsedWhenNoCwdFile covers the fallback half of
// the same precedence: paths.GlobalMCPConfig() (Task A2) was dead code until
// this wiring, so this proves it is actually reachable.
func TestMCPConfigPrecedence_GlobalUsedWhenNoCwdFile(t *testing.T) {
	global := filepath.Join(t.TempDir(), "global.json")
	if err := os.WriteFile(global, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_MCP_CONFIG", global)

	if got := resolveMCPConfig("", t.TempDir()); got != global {
		t.Fatalf("resolveMCPConfig = %q, want the global file %q", got, global)
	}
}

// TestMCPConfigPrecedence_ExplicitFlagWinsOutright confirms an explicit
// --mcp-config value is never second-guessed against either fallback, even
// when neither the cwd file nor the flag's own path exists — resolveMCPConfig
// does no existence check on an explicit value; runAgent's caller-side
// os.Stat handles "explicit but missing" as an error.
func TestMCPConfigPrecedence_ExplicitFlagWinsOutright(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_MCP_CONFIG", filepath.Join(t.TempDir(), "global.json"))

	explicit := "/explicit/.mcp.json"
	if got := resolveMCPConfig(explicit, cwd); got != explicit {
		t.Fatalf("resolveMCPConfig = %q, want the explicit flag value %q", got, explicit)
	}
}
