//go:build unix

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
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
	// The detached child presents the same ticket on the same dial, so it is
	// held to the same rules; only the flag NAMING the carrier differs.
	childOf := func(f *ticketServeFlags) {
		f.TicketStdin = false
		f.TicketFD, f.LockFD = fd(inheritedTicketFD), fd(inheritedLockFD)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ticketServeFlags)
		want   []string
	}{
		{"missing --connect", func(f *ticketServeFlags) { f.Connect = "" }, []string{"--connect"}},
		{"missing --install-id", func(f *ticketServeFlags) { f.InstallID = "" }, []string{"--install-id"}},
		{"unusable --install-id", func(f *ticketServeFlags) { f.InstallID = "not an id!" }, []string{"--install-id"}},
		{"--stdio", func(f *ticketServeFlags) { f.Stdio = true }, []string{"--stdio"}},
		{"--connect-socket", func(f *ticketServeFlags) { f.ConnectSocket = "/run/rafikid-exec.sock" }, []string{"--connect-socket"}},
		{"--enroll-token", func(f *ticketServeFlags) { f.EnrollToken = "t_enroll" }, []string{"--enroll-token"}},
		{"--credential", func(f *ticketServeFlags) { f.Credential = "a-credential" }, []string{"--credential"}},
		{"detached child: missing --connect", func(f *ticketServeFlags) {
			childOf(f)
			f.Connect = ""
		}, []string{"--connect"}},
		{"detached child: missing --install-id", func(f *ticketServeFlags) {
			childOf(f)
			f.InstallID = ""
		}, []string{"--install-id"}},
		{"detached child: --stdio", func(f *ticketServeFlags) {
			childOf(f)
			f.Stdio = true
		}, []string{"--stdio"}},
		{"detached child: --connect-socket", func(f *ticketServeFlags) {
			childOf(f)
			f.ConnectSocket = "/run/rafikid-exec.sock"
		}, []string{"--connect-socket"}},
		{"detached child: --enroll-token", func(f *ticketServeFlags) {
			childOf(f)
			f.EnrollToken = "t_enroll"
		}, []string{"--enroll-token"}},
		{"detached child: --credential", func(f *ticketServeFlags) {
			childOf(f)
			f.Credential = "a-credential"
		}, []string{"--credential"}},
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

	// A flag VALUE that happens to spell one of those names is an ordinary
	// argument and must survive: only a token that STARTS with a dash is a flag.
	c.EqDeep([]string{"executor", "serve", "--install-id", "detach", "--root", "ticket-stdin", "--ticket-fd=3", "--lock-fd=4"},
		detachChildArgs([]string{"executor", "serve", "--install-id", "detach", "--root", "ticket-stdin"}),
		"a flag value spelled like a dropped flag must not be dropped with it")
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

// An adopted descriptor must be close-on-exec. fd 4 arrives as a dup of the
// parent's — and a dup clears FD_CLOEXEC — so without setting it here every
// process this executor later spawns (bash, a background job, an MCP server, an
// LSP) inherits the locked file description. An orphaned grandchild then holds
// executor.lock for the rest of its life with no executor running, and every
// relaunch prints "running pid=<dead pid>" and starts nothing.
func TestHoldInheritedLockMarksCloseOnExec(t *testing.T) {
	c := assert.NewAborting(t)

	r, w, err := os.Pipe()
	c.Require().NoError(err, "os.Pipe")
	dup, err := syscall.Dup(int(r.Fd()))
	c.Require().NoError(err, "dup the pipe's read end, standing in for the inherited fd")
	_ = r.Close()
	_ = w.Close()

	lock, err := holdInheritedLock(dup)
	c.Require().NoError(err, "holdInheritedLock")
	t.Cleanup(func() { _ = lock.Close() })

	flags, err := unix.FcntlInt(uintptr(dup), unix.F_GETFD, 0)
	c.Require().NoError(err, "F_GETFD on the adopted descriptor")
	c.True(flags&unix.FD_CLOEXEC != 0,
		"the adopted lock descriptor must be close-on-exec, or every process this executor spawns inherits the flock")
}

// os.NewFile wraps any fd >= 0, so "is this an open descriptor" has to be asked
// of the kernel. A --lock-fd that is not open would otherwise leave the child
// claiming a lock it does not hold, and a second launch would start a second
// executor.
func TestHoldInheritedLockRefusesAClosedDescriptor(t *testing.T) {
	c := assert.NewAborting(t)

	lock, err := holdInheritedLock(closedDescriptor(t))
	c.Require().Error(err, "a --lock-fd that is not open must be refused")
	c.StrContains(err.Error(), "--lock-fd", "the refusal must name the flag the operator passed")
	c.True(lock == nil, "no descriptor may be adopted for a closed fd")
}

// Same rule for the ticket descriptor: reading a closed fd would fail as an
// opaque read error — or, worse, close whatever now holds that number.
func TestReadInheritedTicketRefusesAClosedDescriptor(t *testing.T) {
	c := assert.NewAborting(t)

	ticket, err := readInheritedTicket(closedDescriptor(t))
	c.Require().Error(err, "a --ticket-fd that is not open must be refused")
	c.StrContains(err.Error(), "--ticket-fd", "the refusal must name the flag the operator passed")
	c.Eq("", ticket, "nothing may be presented")
}

// closedDescriptor returns a descriptor number that is open in no process: the
// pipe it belonged to is closed on both ends, and the dup of it is closed too.
func closedDescriptor(t *testing.T) int {
	t.Helper()
	c := assert.NewAborting(t)

	r, w, err := os.Pipe()
	c.Require().NoError(err, "os.Pipe")
	n, err := syscall.Dup(int(r.Fd()))
	c.Require().NoError(err, "dup the read end")
	c.Require().NoError(r.Close(), "close the read end")
	c.Require().NoError(w.Close(), "close the write end")
	c.Require().NoError(syscall.Close(n), "close the dup")
	return n
}

// An empty ticket is the SAME refusal whether stdin was empty from the start or
// ended without a newline: the operator is told there was no ticket, never
// handed the io.EOF a reader ends with.
func TestDetachNoInstallTicketOnStdin(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name  string
		stdin io.Reader
	}{
		{"empty reader", strings.NewReader("")},
		{"empty pipe", emptyPipe(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runDetach(newExecutorServeCmd(), detachConfig{
				DataDir: t.TempDir(),
				Stdin:   tc.stdin,
				Stdout:  &out,
			})
			c.Require().Error(err, "a launch with no ticket must fail")
			c.Eq("no install ticket on stdin", err.Error(),
				"the operator must be told there was no ticket, not that a read hit EOF")
			c.Eq("", out.String(), "a refused launch prints nothing")
		})
	}
}

// The detached child's version of the same refusal, named for its descriptor:
// this line is what lands in executor.log, and it must say what is missing.
func TestReadInheritedTicketNoInstallTicketOnEmptyPipe(t *testing.T) {
	c := assert.NewAborting(t)

	r, w, err := os.Pipe()
	c.Require().NoError(err, "os.Pipe")
	c.Require().NoError(w.Close(), "close the write end: the reader sees EOF at once")
	// readInheritedTicket closes the descriptor it is handed, so it gets its own
	// dup rather than the pipe's read end (which would then be closed twice).
	n, err := syscall.Dup(int(r.Fd()))
	c.Require().NoError(err, "dup the read end")
	_ = r.Close()

	ticket, err := readInheritedTicket(n)
	c.Require().Error(err, "an empty inherited pipe carries no ticket")
	c.Eq("", ticket, "nothing may be presented")
	c.Eq(fmt.Sprintf("no install ticket on fd %d", n), err.Error(),
		"the log must say there was no ticket on the descriptor, not that a read ended")
}

// One line, bounded at 4096 bytes. A longer "line" is a protocol mistake, and it
// must be refused before a child is started rather than truncated mid-credential.
func TestDetachOversizedTicketLineIsRefused(t *testing.T) {
	c := assert.NewAborting(t)

	dir := t.TempDir()
	var out bytes.Buffer
	err := runDetach(newExecutorServeCmd(), detachConfig{
		DataDir:    dir,
		Executable: "/bin/sh",
		Args:       []string{"-c", ":"},
		Stdin:      strings.NewReader(strings.Repeat("x", detachTicketMax+1)),
		Stdout:     &out,
	})
	c.Require().Error(err, "a ticket line past the bound must be refused")
	c.StrContains(err.Error(), fmt.Sprintf("longer than %d bytes", detachTicketMax),
		"the refusal must name the bound")
	c.Eq("", out.String(), "a refused launch prints nothing")
	c.Eq(0, readPID(dir), "a refused launch must start nothing")
}

// The pid file is read by a concurrent launch to answer "running pid=", so it is
// written to a temp file and renamed: a reader sees the old pid or the new one,
// never an empty or half-written file it would report as "running pid=0".
func TestWritePIDIsAtomic(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()

	c.Require().NoError(writePID(dir, 8123), "writePID")
	c.Eq(8123, readPID(dir), "the pid file must read back what was written")

	st, err := os.Stat(filepath.Join(dir, "executor.pid"))
	c.Require().NoError(err, "stat the pid file")
	c.Eq(os.FileMode(0o600), st.Mode().Perm(), "the pid file must be 0600")

	ents, err := os.ReadDir(dir)
	c.Require().NoError(err, "read the data dir")
	for _, e := range ents {
		c.False(strings.HasPrefix(e.Name(), ".executor.pid-"),
			"the temp file must be renamed away, not left behind: %s", e.Name())
	}

	// Overwriting is the normal case: a relaunch records the pid it started,
	// and it must be a REPLACE — a fresh file renamed over the old one — so a
	// concurrent reader never observes the old file truncated. An in-place
	// rewrite keeps the inode; the rename swaps it.
	before := inodeOf(t, filepath.Join(dir, "executor.pid"))
	c.Require().NoError(writePID(dir, 8124), "rewrite the pid")
	c.Eq(8124, readPID(dir), "a rewrite must replace the recorded pid")
	c.True(inodeOf(t, filepath.Join(dir, "executor.pid")) != before,
		"the pid file must be replaced by a rename, not rewritten in place")
}

// inodeOf reads a file's inode number, which is how a test can tell a rename
// apart from an in-place rewrite without watching the write itself.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	c := assert.NewAborting(t)
	st, err := os.Stat(path)
	c.Require().NoError(err, "stat %s", path)
	sys, ok := st.Sys().(*syscall.Stat_t)
	c.Require().True(ok, "stat %s did not yield a syscall.Stat_t", path)
	return uint64(sys.Ino)
}

// A pid file that cannot be written is not a failed launch: the child is running
// and holds the instance lock, so reporting a failure would invite a second
// launch that starts nothing. It is logged instead.
func TestDetachPidWriteFailureStillReportsStarted(t *testing.T) {
	c := assert.NewAborting(t)

	dir := t.TempDir()
	// A DIRECTORY at executor.pid: the rename cannot replace it, so the pid file
	// cannot be written while every other part of the launch succeeds.
	c.Require().NoError(os.Mkdir(filepath.Join(dir, "executor.pid"), 0o700), "block the pid file")

	prev := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var out bytes.Buffer
	err := runDetach(newExecutorServeCmd(), detachConfig{
		DataDir:    dir,
		Executable: "/bin/sh",
		Args:       []string{"-c", ":"},
		Stdin:      strings.NewReader("ticket-xyz\n"),
		Stdout:     &out,
	})
	c.Require().NoError(err, "an unrecordable pid is not a failed launch")
	c.StrContains(out.String(), "started pid=", "the launch must still be reported as started")
	c.StrContains(logs.String(), "level=WARN", "the failure must be logged at Warn")
	c.StrContains(logs.String(), "could not record the detached executor's pid",
		"the log must say what could not be recorded")
}

// The OnEnrolled hook of a ticket-authenticated serve does two things: it clears
// this process's copy of the spent ticket, and it records the install marker,
// because a credential on disk is what makes the install known to be live.
func TestInstallEnrollHookClearsTheTicketAndRecordsTheMarker(t *testing.T) {
	c := assert.NewAborting(t)
	isolateExecutorDataDir(t)

	marker := filepath.Join(paths.DataDir(), "installed-executor")
	ticket := "ticket-one-shot"
	hook := installEnrollHook(&ticket, "inst-42")

	c.False(fileExists(marker), "the marker must not exist before the credential is on disk")
	hook()
	c.Eq("", ticket, "the hook must clear this process's copy of the spent ticket")

	b, err := os.ReadFile(marker)
	c.Require().NoError(err, "read the install marker")
	c.Eq("inst-42\n", string(b), "the marker names the install the daemon recorded")
	st, err := os.Stat(marker)
	c.Require().NoError(err, "stat the install marker")
	c.Eq(os.FileMode(0o600), st.Mode().Perm(), "the marker must be 0600")
}

// The same hook, driven by a REAL redemption: pkg/execpool redeems the ticket
// over the executor socket, writes the credential, and only then calls it. So
// this pins the marker against the whole path rather than the hook alone.
func TestServeInstallTicketWritesTheMarkerAfterRedemption(t *testing.T) {
	c := assert.NewAborting(t)
	isolateExecutorDataDir(t)

	// A unix socket path is bounded by sun_path (104 bytes on darwin), so this
	// cannot live under t.TempDir(): its path carries the test's name.
	dir, err := os.MkdirTemp("/tmp", "exec-dial-")
	c.Require().NoError(err, "create a short scratch directory")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "exec.sock")
	ln, err := net.Listen("unix", sock)
	c.Require().NoError(err, "listen on the executor socket")

	srv := &http.Server{
		Handler: upgradeconn.Handler(upgradeconn.Executor,
			func(*http.Request) (struct{}, http.Header, error) {
				return struct{}{}, http.Header{
					upgradeconn.HeaderCredential: {"minted-cred"},
					upgradeconn.HeaderExecutorID: {"inst-42"},
				}, nil
			},
			// Hold the connection open: everything after the 101 is the
			// executor's own side, and this test is about what it did before.
			func(*upgradeconn.Conn, struct{}) {},
		),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ticket := "ticket-one-shot"
	hook := installEnrollHook(&ticket, "inst-42")
	enrolled := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- execpool.Connect(ctx, execpool.ConnectOptions{
			SocketPath:     sock,
			CredentialFile: filepath.Join(t.TempDir(), "executor.cred"),
			InstallTicket:  ticket,
			OnEnrolled: func() {
				hook()
				close(enrolled)
			},
			Handler: http.NotFoundHandler(),
		})
	}()

	select {
	case <-enrolled:
	case <-time.After(10 * time.Second):
		c.Fail("the install ticket was never redeemed over the executor socket")
	}

	// After the hook returned, and therefore after the marker was renamed into
	// place: a credential on disk is what the hook is called for.
	b, err := os.ReadFile(filepath.Join(paths.DataDir(), "installed-executor"))
	c.Require().NoError(err, "read the install marker a redeemed ticket wrote")
	c.Eq("inst-42\n", string(b), "a redeemed ticket must record the install marker the daemon named")
	c.Eq("", ticket, "and the spent ticket must be gone from this process's copy")

	cancel()
	select {
	case <-runErr:
	case <-time.After(10 * time.Second):
		c.Fail("Connect never returned after the context was cancelled")
	}
}

// Options.ConnectAddr is the address another machine is told to dial to reach
// the same daemon. It is the TCP target and NOTHING on a unix socket: a socket
// path means something on this host only.
func TestExecutorDialTargetConnectAddr(t *testing.T) {
	c := assert.NewAborting(t)

	tcp, err := resolveExecutorDialTarget("127.0.0.1:9000", "")
	c.Require().NoError(err, "resolveExecutorDialTarget over TCP")
	c.Eq("127.0.0.1:9000", tcp.Addr, "the TCP transport dials --connect")
	c.Eq("", tcp.Socket, "a TCP dial has no socket")
	c.Eq("127.0.0.1:9000", tcp.SelfAddr, "a launcher must be handed the address this executor dials")

	sck, err := resolveExecutorDialTarget("", "/run/rafikid-exec.sock")
	c.Require().NoError(err, "resolveExecutorDialTarget over a socket")
	c.Eq("", sck.Addr, "a socket dial has no TCP address")
	c.Eq("/run/rafikid-exec.sock", sck.Socket, "the socket transport dials --connect-socket")
	c.Eq("", sck.SelfAddr, "a socket path is meaningless on another machine, so nothing is advertised")

	_, err = resolveExecutorDialTarget("127.0.0.1:9000", "/run/rafikid-exec.sock")
	c.Require().Error(err, "both transports at once is refused, so SelfAddr can never name the wrong one")
}

// isolateExecutorDataDir points paths.DataDir() at a scratch directory, so a
// test that writes an install marker cannot record one in the developer's own
// data dir — which the next real install would then read as "already installed".
func isolateExecutorDataDir(t *testing.T) {
	t.Helper()
	paths.ResetInstanceForTest()
	t.Cleanup(paths.ResetInstanceForTest)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// emptyPipe is the read end of a pipe whose write end is already closed: a
// reader that has produced nothing and never will.
func emptyPipe(t *testing.T) *os.File {
	t.Helper()
	c := assert.NewAborting(t)
	r, w, err := os.Pipe()
	c.Require().NoError(err, "os.Pipe")
	c.Require().NoError(w.Close(), "close the write end: the reader sees EOF at once")
	t.Cleanup(func() { _ = r.Close() })
	return r
}
