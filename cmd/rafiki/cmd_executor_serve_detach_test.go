//go:build unix

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// The detached launch is the target-side half of a direct install. Its guards
// are about what must NOT happen: the ticket must stay off argv and out of the
// environment, a second launch must not touch stdin (which would spend the one
// shot ticket), a stale lock FILE must not block a launch (only a held lock
// does), and the log must not grow without bound.

// readFailsIfRead fails the test the moment anything reads it. A launch that
// finds the lock held must report the running executor BEFORE it looks at
// stdin, or the ticket is consumed by a process that starts nothing.
type readFailsIfRead struct{ t *testing.T }

func (r readFailsIfRead) Read([]byte) (int, error) {
	r.t.Errorf("stdin was read: a held instance lock must be reported without touching the ticket")
	return 0, io.EOF
}

// Same rule for flags: a launcher's argv that names a conflicting flag must be
// told WHICH flag, never handed a process that silently ignores it.
func TestServeTicketFlagValidation(t *testing.T) {
	c := assert.NewAborting(t)

	// The two launches T1.3 emits, and an ordinary serve, are all accepted.
	c.NoError(validateTicketFlags(ticketServeFlags{}), "a plain serve must be accepted")
	c.NoError(validateTicketFlags(ticketServeFlags{
		TicketStdin: true, Connect: "127.0.0.1:9000", InstallID: "inst-1",
	}), "the documented ticket launch must be accepted")
	c.NoError(validateTicketFlags(ticketServeFlags{
		TicketFD: fd(3), LockFD: fd(4), Connect: "127.0.0.1:9000", InstallID: "inst-1",
	}), "the detached child --detach re-execs must be accepted")

	base := ticketServeFlags{TicketStdin: true, Connect: "127.0.0.1:9000", InstallID: "inst-1"}
	for _, tc := range []struct {
		name   string
		mutate func(*ticketServeFlags)
		want   []string
	}{
		{"missing --connect", func(f *ticketServeFlags) { f.Connect = "" }, []string{"--ticket-stdin", "--connect"}},
		{"missing --install-id", func(f *ticketServeFlags) { f.InstallID = "" }, []string{"--ticket-stdin", "--install-id"}},
		{"unusable --install-id", func(f *ticketServeFlags) { f.InstallID = "not an id!" }, []string{"--install-id"}},
		{"--stdio", func(f *ticketServeFlags) { f.Stdio = true }, []string{"--ticket-stdin", "--stdio"}},
		{"--connect-socket", func(f *ticketServeFlags) { f.ConnectSocket = "/run/rafikid-exec.sock" }, []string{"--ticket-stdin", "--connect-socket"}},
		{"--enroll-token", func(f *ticketServeFlags) { f.EnrollToken = "t_enroll" }, []string{"--ticket-stdin", "--enroll-token"}},
		{"--credential", func(f *ticketServeFlags) { f.Credential = "a-credential" }, []string{"--ticket-stdin", "--credential"}},
		{"--detach without --ticket-stdin", func(f *ticketServeFlags) {
			*f = ticketServeFlags{Detach: true}
		}, []string{"--detach", "--ticket-stdin"}},
		{"--lock-fd alone", func(f *ticketServeFlags) {
			*f = ticketServeFlags{LockFD: fd(4)}
		}, []string{"--lock-fd", "--ticket-fd"}},
		{"--ticket-fd alone", func(f *ticketServeFlags) {
			*f = ticketServeFlags{TicketFD: fd(3)}
		}, []string{"--ticket-fd", "--lock-fd"}},
		{"--detach with the fd pair", func(f *ticketServeFlags) {
			*f = ticketServeFlags{TicketFD: fd(3), LockFD: fd(4), Detach: true}
		}, []string{"--detach"}},
		{"--ticket-stdin with the fd pair", func(f *ticketServeFlags) {
			f.TicketFD, f.LockFD = fd(inheritedTicketFD), fd(inheritedLockFD)
		}, []string{"--ticket-stdin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.mutate(&f)
			err := validateTicketFlags(f)
			c.Require().Error(err, "%s must be refused", tc.name)
			for _, want := range tc.want {
				c.StrContains(err.Error(), want, "the refusal must name the conflicting flag %s", want)
			}
		})
	}
}

// The re-exec reshapes the parent's arguments: the two flags that describe the
// PARENT's job are dropped, the inherited descriptors are named, and nothing
// else changes.
func TestDetachChildArgs(t *testing.T) {
	c := assert.NewAborting(t)

	parent := []string{
		"executor", "serve",
		"--connect", "127.0.0.1:9000",
		"--install-id", "inst-1",
		"--ticket-stdin", "--detach",
	}
	c.EqDeep([]string{
		"executor", "serve",
		"--connect", "127.0.0.1:9000",
		"--install-id", "inst-1",
		"--ticket-fd=3", "--lock-fd=4",
	}, detachChildArgs(parent), "the child must get the parent's arguments minus --detach/--ticket-stdin, plus the inherited descriptors")

	// The =value spelling is what pflag also accepts, and existing fd flags must
	// not survive to give the child two of them.
	c.EqDeep([]string{"executor", "serve", "--ticket-fd=3", "--lock-fd=4"},
		detachChildArgs([]string{"executor", "serve", "--detach=true", "--ticket-stdin=true", "--ticket-fd=9", "--lock-fd=10"}),
		"the =value spelling and any inherited fd flags must be handled the same way")
}

// The ticket's ONLY carrier is the inherited pipe. argv and the environment of a
// running process are readable by every local user (ps, /proc), so this is the
// pin that keeps a future change from smuggling the ticket into either.
func TestDetachTicketNeverInArgvOrEnv(t *testing.T) {
	c := assert.NewAborting(t)
	const sentinel = "ticket-sentinel-9f3a2c"

	child, err := detachChildFor("/usr/local/bin/rafiki", []string{
		"executor", "serve",
		"--connect", "127.0.0.1:9000",
		"--install-id", "inst-1",
		"--ticket-stdin", "--detach",
	}, sentinel)
	c.Require().NoError(err, "detachChildFor")
	t.Cleanup(func() { _ = child.Ticket.Close() })

	for _, a := range child.Args {
		c.False(strings.Contains(a, sentinel), "the ticket must never be an argv element; found %q", a)
	}
	for _, e := range child.Env {
		c.False(strings.Contains(e, sentinel), "the ticket must never be an environment variable; found %q", e)
	}

	got, err := io.ReadAll(child.Ticket)
	c.Require().NoError(err, "read the ticket pipe")
	c.Eq(sentinel+"\n", string(got), "the inherited pipe is the ticket's only carrier")
}

// A held lock means an executor is already running: report it and return
// without reading stdin, so the ticket the launcher sent is not spent.
func TestDetachSecondLaunchReportsRunningAndReadsNoStdin(t *testing.T) {
	c := assert.NewAborting(t)

	dir := t.TempDir()
	lock, held, err := acquireInstanceLock(dir)
	c.Require().NoError(err, "acquireInstanceLock")
	c.Require().True(held, "the first launch must take the lock")
	t.Cleanup(func() { _ = lock.Close() })
	c.Require().NoError(writePID(dir, 4242), "write the running executor's pid")

	var out bytes.Buffer
	err = runDetach(newExecutorServeCmd(), detachConfig{
		DataDir: dir,
		Stdin:   readFailsIfRead{t},
		Stdout:  &out,
	})
	c.Require().NoError(err, "a held lock is not an error")
	c.Eq("running pid=4242\n", out.String(), "exactly one line, naming the pid from executor.pid")
}

// The lock is a flock on an open descriptor. A lock FILE with no holder — what a
// crashed executor leaves behind — must never refuse a launch.
func TestDetachStaleLockFileDoesNotBlock(t *testing.T) {
	c := assert.NewAborting(t)

	dir := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "executor.lock"), []byte("stale\n"), 0o600),
		"seed a stale lock file")

	lock, held, err := acquireInstanceLock(dir)
	c.Require().NoError(err, "acquireInstanceLock")
	c.Require().True(held, "a lock file with no holder must not block")
	_ = lock.Close()

	// And a whole launch proceeds over it.
	var out bytes.Buffer
	c.Require().NoError(runDetach(newExecutorServeCmd(), detachConfig{
		DataDir:    dir,
		Executable: "/bin/sh",
		Args:       []string{"-c", ":"},
		Stdin:      strings.NewReader("ticket-xyz\n"),
		Stdout:     &out,
	}), "runDetach over a stale lock file")
	c.StrContains(out.String(), "started pid=", "the launch must proceed, not report running")
}

// Rotation is a disk guard, not a log policy: under or at 4 MiB the log is left
// exactly as it is; over it, the previous log becomes executor.log.1.
func TestRotateLogOnlyWhenOverFourMiB(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "executor.log")

	c.Require().NoError(os.WriteFile(logPath, bytes.Repeat([]byte("x"), detachLogMax), 0o600),
		"seed a log at the limit")
	rotateLog(dir)
	st, err := os.Stat(logPath)
	c.Require().NoError(err, "the log at the limit must survive")
	c.Eq(int64(detachLogMax), st.Size(), "the log at the limit must not be rotated")
	c.False(fileExists(logPath+".1"), "no rotation below the limit")

	c.Require().NoError(os.WriteFile(logPath, bytes.Repeat([]byte("y"), detachLogMax+1), 0o600),
		"seed a log past the limit")
	rotateLog(dir)
	c.False(fileExists(logPath), "an oversized log must be rotated away")
	st, err = os.Stat(logPath + ".1")
	c.Require().NoError(err, "the previous log must become executor.log.1")
	c.Eq(int64(detachLogMax+1), st.Size(), "executor.log.1 holds the whole previous log")
}

// fd is the pointer a --ticket-fd/--lock-fd flag value arrives as. Pointer
// fields keep "unset" distinguishable from fd 0.
func fd(n int) *int { return &n }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestDetachHelperProcess stands a real detached child up through the same
// lock, pid and log code. The child is a stand-in for `rafiki executor serve`:
// it reads its ticket from fd 3, records its argv and environment, keeps fd 4
// (and therefore the flock) by not exiting, and sleeps until the test kills it.
func TestDetachHelperProcess(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()

	argvOut := filepath.Join(dir, "child-argv.txt")
	envOut := filepath.Join(dir, "child-env.txt")
	ticketOut := filepath.Join(dir, "child-ticket.txt")
	script := filepath.Join(dir, "sleeper.sh")
	body := fmt.Sprintf(`#!/bin/sh
IFS= read -r ticket <&3
printf '%%s' "$ticket" > %s
printf '%%s\n' "$@" > %s
env > %s
exec sleep 60
`, shellQuoteForTest(ticketOut), shellQuoteForTest(argvOut), shellQuoteForTest(envOut))
	c.Require().NoError(os.WriteFile(script, []byte(body), 0o700), "write the helper child")

	const ticket = "ticket-sentinel-4d1f"
	var out bytes.Buffer
	c.Require().NoError(runDetach(newExecutorServeCmd(), detachConfig{
		DataDir:    dir,
		Executable: script,
		Args: []string{
			"executor", "serve",
			"--connect", "127.0.0.1:9000",
			"--install-id", "inst-1",
		},
		Stdin:  strings.NewReader(ticket + "\n"),
		Stdout: &out,
	}), "the first launch must start a detached child")

	pid := readPID(dir)
	c.True(pid > 0, "runDetach must record the child's pid")
	c.Eq(fmt.Sprintf("started pid=%d\n", pid), out.String(), "the parent prints exactly one line and returns")
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	// The child is a session leader (Setsid), so -pid signals its whole group.
	childPID, err := os.FindProcess(pid)
	c.Require().NoError(err, "FindProcess")
	t.Cleanup(func() { _ = childPID.Kill() })

	c.Eq(ticket, string(waitForFile(t, ticketOut)), "the ticket must arrive on the inherited pipe, one line")

	// argv and environment of the REAL child: no ticket, and the descriptors
	// named so the child knows where its lock and ticket are.
	argv := string(waitForFile(t, argvOut))
	c.StrContains(argv, "--ticket-fd=3", "the child must be told its ticket pipe")
	c.StrContains(argv, "--lock-fd=4", "the child must be told its inherited lock")
	c.False(strings.Contains(argv, ticket), "the ticket must never reach the child's argv")
	c.False(strings.Contains(argv, "--ticket-stdin"), "the child reads the pipe, not stdin")
	c.False(strings.Contains(string(waitForFile(t, envOut)), ticket), "the ticket must never reach the child's environment")

	// A second launch finds the lock held by that child: same pid, no stdin.
	out.Reset()
	c.Require().NoError(runDetach(newExecutorServeCmd(), detachConfig{
		DataDir:    dir,
		Executable: script,
		Args:       []string{},
		Stdin:      readFailsIfRead{t},
		Stdout:     &out,
	}), "a second launch over a held lock is not an error")
	c.Eq(fmt.Sprintf("running pid=%d\n", pid), out.String(), "the second launch must report the same executor")
}

// waitForFile polls for a file the detached child writes, because the parent
// deliberately does not wait for it.
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	c := assert.NewAborting(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return b
		}
		c.False(time.Now().After(deadline), "the detached child never wrote %s", path)
		time.Sleep(20 * time.Millisecond)
	}
}

// shellQuoteForTest single-quotes s for /bin/sh, so a temp path can never be
// read as shell syntax.
func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
