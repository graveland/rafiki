//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
)

// This file is the near end of `executor serve --ticket-stdin --detach`: the
// launcher starts ONE of these — over ssh, or on this machine — and it takes
// the instance lock, reads the ticket, and re-execs itself as a detached
// process that holds the lock for its whole life. The session that started it
// ends; the executor does not.
//
// Three invariants decide the shape:
//
//   - the ticket is never an argv element and never an environment variable. It
//     arrives on stdin once, then travels on an inherited pipe descriptor (fd
//     3) and is closed the moment the child has read it. argv and the
//     environment of a running process are readable by every local user
//     (`ps`, /proc).
//   - the lock is a flock on an OPEN DESCRIPTOR, not the existence of a file. A
//     lock file left behind by a crashed executor is inert, so a relaunch after
//     a crash must not be refused. The descriptor is inherited by the detached
//     child (fd 4), which is what keeps the lock held after the parent exits.
//   - a second launch reads no stdin at all when the lock is held. Reading it
//     would spend the one-shot ticket on a launch that starts nothing.

// detachTicketMax bounds the ticket line this file reads and writes. It is
// generous beside the --stdio limit because the line is still one short opaque
// token; a longer "line" is a protocol mistake, not a credential.
const detachTicketMax = 4096

// detachLogMax is the size at which executor.log is rotated. A detached
// executor runs for months with nobody watching, and a reconnect loop logs a
// line per attempt; unbounded, the log eventually fills a small target's disk.
const detachLogMax = 4 << 20

// The descriptors the detached child inherits from its parent: the ticket pipe
// and the locked lock file, in ExtraFiles order.
const (
	inheritedTicketFD = 3
	inheritedLockFD   = 4
)

// detachConfig is runDetach's injectable inputs. Production leaves every field
// empty, which means paths.DataDir(), os.Executable(), os.Args[1:], os.Stdin
// and the command's own stdout. Tests pass them so the lock, pid and log code
// runs exactly as it does in a launcher-started process.
type detachConfig struct {
	DataDir    string
	Executable string
	Args       []string // this process's arguments, WITHOUT the program name
	Stdin      io.Reader
	Stdout     io.Writer
}

// acquireInstanceLock takes the instance's exclusive launch lock. held=false
// means another process holds it — an executor is already running here — and is
// not an error.
func acquireInstanceLock(dir string) (*os.File, bool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "executor.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, true, nil
}

// readPID reads <dir>/executor.pid: the detached executor this instance last
// recorded. 0 when the file is absent or malformed — a pid that cannot be read
// is reported as "no pid", never as an error that would block a launch.
func readPID(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "executor.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// writePID records the detached child's pid, which uninstall needs in order to
// signal the process group, and which a second launch names when it reports the
// executor as already running.
func writePID(dir string, pid int) error {
	path := filepath.Join(dir, "executor.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// rotateLog renames an oversized executor.log to executor.log.1, replacing the
// previous rotation. It runs BEFORE the child opens the log, so the child's
// append can never straddle the rename. A log at or under detachLogMax is left
// alone: rotation is a disk guard, not a log policy.
func rotateLog(dir string) {
	logPath := filepath.Join(dir, "executor.log")
	st, err := os.Stat(logPath)
	if err != nil || st.Size() <= detachLogMax {
		return
	}
	_ = os.Rename(logPath, logPath+".1")
}

// detachChild is what the parent execs for the detached executor: the same
// binary, the parent's arguments reshaped for the child, this process's
// environment unchanged, and the READ end of the pipe that carries the ticket.
//
// Nothing about the ticket is in Args or Env. Ticket is the pipe, handed to the
// child as fd 3; it is the ticket's only carrier.
type detachChild struct {
	Path   string
	Args   []string
	Env    []string
	Ticket *os.File
}

// detachChildFor builds that description, writing the ticket into the pipe and
// closing the write end so the child sees exactly one line followed by EOF.
//
// The ticket is a parameter precisely so this function has it in hand and the
// tests can prove it put it nowhere but the pipe: argv and the environment are
// world-readable on the target.
func detachChildFor(exe string, parentArgs []string, ticket string) (detachChild, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return detachChild{}, fmt.Errorf("create the ticket pipe: %w", err)
	}
	if _, err := io.WriteString(w, ticket+"\n"); err != nil {
		_ = r.Close()
		_ = w.Close()
		return detachChild{}, fmt.Errorf("write the ticket to its pipe: %w", err)
	}
	if err := w.Close(); err != nil {
		_ = r.Close()
		return detachChild{}, fmt.Errorf("close the ticket pipe's write end: %w", err)
	}
	return detachChild{
		Path:   exe,
		Args:   detachChildArgs(parentArgs),
		Env:    os.Environ(),
		Ticket: r,
	}, nil
}

// detachChildArgs reshapes the parent's arguments (without the program name)
// for the detached child. Two flags describe the PARENT's job and are dropped:
// --detach (the child must not detach again) and --ticket-stdin (the ticket is
// no longer on stdin — it rides the inherited pipe named by --ticket-fd). Any
// fd flags already present are dropped too, so the child is given exactly one
// pair.
func detachChildArgs(parent []string) []string {
	out := make([]string, 0, len(parent)+2)
	for _, a := range parent {
		if isDetachParentFlag(a) {
			continue
		}
		out = append(out, a)
	}
	return append(out,
		fmt.Sprintf("--ticket-fd=%d", inheritedTicketFD),
		fmt.Sprintf("--lock-fd=%d", inheritedLockFD))
}

// isDetachParentFlag reports whether arg is one of the flags the detached child
// must not inherit. Both spellings pflag accepts are covered (--flag and
// --flag=value).
func isDetachParentFlag(arg string) bool {
	name := arg
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimPrefix(strings.TrimPrefix(name, "-"), "-")
	switch name {
	case "detach", "ticket-stdin", "ticket-fd", "lock-fd":
		return true
	}
	return false
}

// readTicketLine reads ONE line of at most max bytes. It reads a byte at a time
// on purpose: a detached child's fd 3 carries nothing after the line, but the
// stdin case is a relay and over-reading would consume bytes belonging to
// whatever the peer sends next.
func readTicketLine(r io.Reader, max int) (string, error) {
	var line []byte
	one := make([]byte, 1)
	for {
		if len(line) >= max {
			return "", fmt.Errorf("the install ticket line is longer than %d bytes", max)
		}
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return strings.TrimSpace(string(line)), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return strings.TrimSpace(string(line)), nil
			}
			return "", err
		}
	}
}

// readInheritedTicket reads the ticket from the inherited descriptor and closes
// it at once: the ticket is spent by the first connect, so it must not stay
// readable in this process a moment longer than needed.
func readInheritedTicket(fd int) (string, error) {
	f := os.NewFile(uintptr(fd), "install-ticket")
	if f == nil {
		return "", fmt.Errorf("no install ticket on fd %d", fd)
	}
	defer func() { _ = f.Close() }()
	ticket, err := readTicketLine(f, detachTicketMax)
	if err != nil {
		return "", fmt.Errorf("read the install ticket from fd %d: %w", fd, err)
	}
	if ticket == "" {
		return "", fmt.Errorf("no install ticket on fd %d", fd)
	}
	return ticket, nil
}

// holdInheritedLock adopts the inherited lock descriptor. It is never closed:
// the lock is this instance's claim to "an executor is running here", and
// releasing it early would let a second launch start a second executor.
func holdInheritedLock(fd int) *os.File {
	return os.NewFile(uintptr(fd), "executor.lock")
}

// runDetach is `executor serve --ticket-stdin --detach`: take the instance
// lock, read exactly one ticket line, re-exec detached with the ticket on an
// inherited pipe and the locked descriptor inherited, record the child's pid,
// print ONE line, and return without waiting. The parent exits; the executor
// does not.
func runDetach(cmd *cobra.Command, cfg detachConfig) error {
	dir := cfg.DataDir
	if dir == "" {
		dir = paths.DataDir()
	}
	exe := cfg.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve own binary: %w", err)
		}
	}
	args := cfg.Args
	if args == nil {
		args = os.Args[1:]
	}
	stdin := cfg.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = cmd.OutOrStdout()
	}

	// The lock decides idempotence, and it is taken BEFORE stdin is touched: a
	// second launch must leave the ticket unread, and therefore unspent.
	lock, held, err := acquireInstanceLock(dir)
	if err != nil {
		return err
	}
	if !held {
		_, err := fmt.Fprintf(stdout, "running pid=%d\n", readPID(dir))
		return err
	}
	// The lock belongs to this process now, and then to its child. Release this
	// copy only if the launch fails.
	releaseLock := true
	defer func() {
		if releaseLock {
			_ = lock.Close()
		}
	}()

	ticket, err := readTicketLine(stdin, detachTicketMax)
	if err != nil {
		return fmt.Errorf("read the install ticket: %w", err)
	}
	if ticket == "" {
		return errors.New("no install ticket on stdin")
	}

	rotateLog(dir)
	logPath := filepath.Join(dir, "executor.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}

	child, err := detachChildFor(exe, args, ticket)
	if err != nil {
		_ = logFile.Close()
		return err
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		_ = child.Ticket.Close()
		_ = logFile.Close()
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	proc := exec.Command(child.Path, child.Args...)
	proc.Env = child.Env
	proc.Stdin = devnull
	proc.Stdout = logFile
	proc.Stderr = logFile
	// Setsid: the child becomes a session and process-group leader, so
	// uninstall can signal its whole group and the session that started it
	// cannot take it down.
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	proc.ExtraFiles = []*os.File{child.Ticket, lock}

	if err := proc.Start(); err != nil {
		_ = child.Ticket.Close()
		_ = logFile.Close()
		return fmt.Errorf("start the detached executor: %w", err)
	}
	// Captured before Release, which renders the Process unusable and clears
	// the pid this has to record.
	pid := proc.Process.Pid
	// The child holds its own copies of the ticket pipe and the lock descriptor
	// now. Drop the parent's copies and release the handle without waiting: the
	// launcher returns while the executor keeps running.
	_ = child.Ticket.Close()
	_ = logFile.Close()
	releaseLock = false
	_ = lock.Close()
	_ = proc.Process.Release()

	if err := writePID(dir, pid); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "started pid=%d\n", pid)
	return err
}
