package tools

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// hasStandaloneLine reports whether s contains a line exactly equal to want.
// Not a substring check: the shell also prints its failed redirection
// diagnostic on stderr ("bash: line 1: /dev/tty: ..."), and "NOTTY" contains
// "TTY" as a substring.
func hasStandaloneLine(s, want string) bool {
	for _, ln := range strings.Split(s, "\n") {
		if ln == want {
			return true
		}
	}
	return false
}

// TestSessionCommandHasNoControllingTerminal is the task-1.1 reproduction:
// a spawned command must not be able to reach a controlling terminal. Without
// a new session, a program that opens /dev/tty (sudo, ssh, gpg) blocks on
// terminal input instead of failing; with Setsid there is no controlling
// terminal and /dev/tty fails.
func TestSessionCommandHasNoControllingTerminal(t *testing.T) {
	c := assert.NewAborting(t)
	out, _, err := runSubprocess(context.Background(), t.TempDir(), time.Second,
		"bash", []string{"-c", "if : </dev/tty 2>/dev/null; then echo TTY; else echo NOTTY; fi"}, nil)
	c.NoError(err)
	c.True(hasStandaloneLine(out, "NOTTY"), "spawned command reached a controlling terminal, got %q", out)
	c.False(hasStandaloneLine(out, "TTY"), "spawned command reached a controlling terminal, got %q", out)
}

// TestSubprocessUsesANewSession asserts runSubprocess's session semantics
// directly on its SysProcAttr, because the /dev/tty probe cannot tell Setsid
// from Setpgid when the test process has no controlling terminal (the case
// under `make check`). Setsid makes the child a session leader with no
// controlling terminal; Setpgid must NOT also be set (the two conflict with
// EPERM).
func TestSubprocessUsesANewSession(t *testing.T) {
	c := assert.NewAborting(t)
	attr := subprocessSysProcAttr()
	c.True(attr.Setsid, "runSubprocess must start a new session (Setsid), got %+v", attr)
	c.False(attr.Setpgid, "Setsid and Setpgid must not both be set (the child gets EPERM), got %+v", attr)
}

// TestBashToolSetsNonInteractiveEnv checks the bash tool's Materialize applies
// nonInteractiveDefaults to the pinned environment: the interpreter's env
// carries GIT_TERMINAL_PROMPT=0 and PAGER=cat even though the caller pinned
// only PATH.
func TestBashToolSetsNonInteractiveEnv(t *testing.T) {
	c := assert.NewAborting(t)
	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{
		OutputPolicy: OutputPolicy{Budget: 30000, SpillDir: t.TempDir()},
		Cwd:          t.TempDir(),
		Env:          []string{"PATH=" + os.Getenv("PATH")},
	})
	c.NoError(err)
	res, err := tool.Execute(context.Background(),
		ToolInput([]byte(`{"command":"echo \"$GIT_TERMINAL_PROMPT:$PAGER\""}`)))
	c.NoError(err)
	c.StrContains(res.Text, "0:cat", "non-interactive defaults missing from the bash tool environment, got")
}
