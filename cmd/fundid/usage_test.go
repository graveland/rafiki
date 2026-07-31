package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// `fundid agent -h` must print usage. It previously exited 0 having printed
// NOTHING: parseAgentFlags sets the FlagSet output to io.Discard so runAgent can
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
	if !strings.Contains(out, "fundid agent") {
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
		if err == flag.ErrHelp {
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

// The top-level binary has two modes (the daemon, and `fundid agent`), so its
// usage must name both — `fundid -h` used to fall through into daemon startup
// and fail on the controller socket instead of printing anything.
func TestPrintRootUsageCoversBothModes(t *testing.T) {
	var buf bytes.Buffer
	printRootUsage(&buf)
	out := buf.String()

	if strings.TrimSpace(out) == "" {
		t.Fatal("printRootUsage wrote nothing")
	}
	for _, want := range []string{"agent", "daemon"} {
		if !strings.Contains(out, want) {
			t.Errorf("root usage missing %q; got:\n%s", want, out)
		}
	}

	// The daemon must call itself fundid. Usage that still said "fundi" would
	// tell users to run the client binary to start the daemon.
	if !strings.Contains(out, "fundid") {
		t.Errorf("root usage does not name the fundid binary; got:\n%s", out)
	}
	if strings.Contains(out, "fundi ") {
		t.Errorf("root usage refers to `fundi ` as if it were the daemon; got:\n%s", out)
	}
}

// printRootUsage is the only place the daemon reports where it resolved its
// own config paths — `make print-config` shells out to `fundid -h` rather
// than re-guessing them, so this output IS the config-discoverability
// feature (task H5), not just documentation. If instructions/skills/presets/
// mcp ever drop out of this listing, `make print-config` silently regresses
// back to reporting nothing about them with no test noticing.
//
// The env vars are pinned so the assertions check against a resolved path
// this test controls, not whatever happens to be in the developer's real
// $HOME.
func TestPrintRootUsageListsResolvedConfigPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "cfg"))
	t.Setenv("FUNDI_INSTRUCTIONS", "")
	t.Setenv("FUNDI_SKILLS_DIRS", "")
	t.Setenv("FUNDI_MCP_CONFIG", "")

	var buf bytes.Buffer
	printRootUsage(&buf)
	out := buf.String()

	for _, label := range []string{"instructions", "skills", "presets", "mcp"} {
		if !strings.Contains(out, label) {
			t.Errorf("root usage missing %q label; got:\n%s", label, out)
		}
	}

	for _, want := range []string{
		paths.InstructionsFile(),
		paths.PresetsFile(),
		paths.GlobalMCPConfig(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("root usage missing resolved path %q; got:\n%s", want, out)
		}
	}
	for _, d := range paths.SkillsDirs() {
		if !strings.Contains(out, d) {
			t.Errorf("root usage missing skills dir %q; got:\n%s", d, out)
		}
	}
}

// A default-config daemon still has exactly one skills directory, so this is
// the only place multi-directory rendering (task H5's per-line loop in
// printRootUsage) gets exercised at all.
func TestPrintRootUsageListsAllSkillsDirs(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("FUNDI_SKILLS_DIRS", "/a"+sep+"/b"+sep+"/c")

	var buf bytes.Buffer
	printRootUsage(&buf)
	out := buf.String()

	for _, d := range []string{"/a", "/b", "/c"} {
		if !strings.Contains(out, d) {
			t.Errorf("root usage missing skills dir %q; got:\n%s", d, out)
		}
	}
	// Only the first line of a multi-dir listing carries the "  skills " label
	// (anchored on the leading two-space path-block indent, since the prose
	// elsewhere in this output also mentions "skills" in passing); continuation
	// lines must NOT repeat it, or grep-based consumers (e.g. `make
	// print-config`'s awk range) would misparse the block.
	if got, want := strings.Count(out, "\n  skills "), 1; got != want {
		t.Errorf("root usage has %d occurrences of the \"  skills \" label line, want %d (only the first dir should be labelled); got:\n%s", got, want, out)
	}
}

// TestMCPConfigPrecedence_CwdBeatsGlobal covers task A6 step 4: a project's
// own .mcp.json must win over the machine-wide $FUNDI_MCP_CONFIG fallback —
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
	t.Setenv("FUNDI_MCP_CONFIG", global)

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
	t.Setenv("FUNDI_MCP_CONFIG", global)

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
	t.Setenv("FUNDI_MCP_CONFIG", filepath.Join(t.TempDir(), "global.json"))

	explicit := "/explicit/.mcp.json"
	if got := resolveMCPConfig(explicit, cwd); got != explicit {
		t.Fatalf("resolveMCPConfig = %q, want the explicit flag value %q", got, explicit)
	}
}

// isHelpArg is what main() consults before any daemon setup. It must catch the
// conventional spellings and nothing else — treating a stray arg as help would
// silently refuse to start the daemon.
func TestIsHelpArg(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		if !isHelpArg(arg) {
			t.Errorf("isHelpArg(%q) = false, want true", arg)
		}
	}
	for _, arg := range []string{"", "agent", "-v", "--version", "serve", "helpful"} {
		if isHelpArg(arg) {
			t.Errorf("isHelpArg(%q) = true, want false", arg)
		}
	}
}
