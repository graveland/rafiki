package executor

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// hasStandaloneLine reports whether s contains a line exactly equal to want.
// Used instead of a substring check because the shell also prints its failed
// redirection diagnostic on stderr ("bash: line 1: /dev/tty: ..."), and
// "NOTTY" contains "TTY" as a substring.
func hasStandaloneLine(s, want string) bool {
	for _, ln := range strings.Split(s, "\n") {
		if ln == want {
			return true
		}
	}
	return false
}

// TestJobSessionHasNoControllingTerminal: a background job runs in its own
// session with no controlling terminal, so a command that opens /dev/tty
// fails instead of blocking on terminal input.
func TestJobSessionHasNoControllingTerminal(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-tty") })

	handle, err := r.start(`if : </dev/tty 2>/dev/null; then echo TTY; else echo NOTTY; fi`,
		"h-tty", "ws-tty", "")
	c.NoError(err, "start")

	// The /dev/tty probe cannot tell Setsid from Setpgid when THIS process has
	// no controlling terminal (the case under `make check`), so assert the
	// session on the job's own process: it must be a session leader (sid ==
	// pid) in a session other than the test's. Getsid works on darwin and
	// Linux. start() returns with the process already exec'd and running.
	r.mu.Lock()
	j := r.jobs[handle]
	r.mu.Unlock()
	pid := j.cmd.Process.Pid
	sid, err := syscall.Getsid(pid)
	c.NoError(err, "Getsid(job pid %d)", pid)
	c.Eq(pid, sid, "job must be a session leader (its session id should equal its pid)")
	selfSid, err := syscall.Getsid(os.Getpid())
	c.NoError(err, "Getsid(self)")
	c.False(sid == selfSid, "job shares the test process's session (sid %d); it did not start a new session", sid)

	// Read until exited — a test may loop on a short sleep here; the
	// no-polling rule is for production code.
	deadline := time.Now().Add(10 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		d, _, exited, _, found := r.output(handle, 0)
		c.True(found, "job vanished while waiting for it to exit")
		data = d
		if exited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := string(data)
	c.True(hasStandaloneLine(out, "NOTTY"), "background job reached a controlling terminal, got %q", out)
	c.False(hasStandaloneLine(out, "TTY"), "background job reached a controlling terminal, got %q", out)
}

// TestJobSessionKillStillKillsTheGroup is a pattern shim: the verify command's
// -run pattern is an unanchored substring match, and TestJobKillStillKillsTheGroup
// contains neither "Session" nor "NonInteractive", so it would be silently
// skipped and the group-kill path would be untested by the gate. This parent
// carries the pattern and invokes the pinned body.
func TestJobSessionKillStillKillsTheGroup(t *testing.T) {
	t.Run("group-kill", TestJobKillStillKillsTheGroup)
}

// TestJobKillStillKillsTheGroup: kill signals the whole process group, so a
// backgrounded grandchild dies with the job.
func TestJobKillStillKillsTheGroup(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-kill") })

	handle, err := r.start("sleep 30 & sleep 30", "h-kill", "ws-kill", "")
	c.NoError(err, "start")

	c.NoError(r.kill(handle), "kill")
	start := time.Now()
	waitForExit(t, r, handle, 2*time.Second)
	c.LessOrEqual(2*time.Second, time.Since(start), "job did not exit within 2s of a group kill, took %v", time.Since(start))
}
