package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// `fundi agent -h` must print usage. It previously exited 0 having printed
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
	if !strings.Contains(out, "fundi agent") {
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

// The top-level binary has two modes (the daemon, and `fundi agent`), so its
// usage must name both — `fundi -h` used to fall through into daemon startup
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
