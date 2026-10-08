package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// reapAsInit makes the process a minimal init when it is PID 1 (a container
// entrypoint). The kernel reparents every orphan to PID 1, and nothing in the
// executor waits for processes it did not start, so a tool's grandchildren
// would otherwise sit as zombies for the container's lifetime. The real work
// runs as a re-exec of this binary (which is never PID 1, so no recursion);
// PID 1 only forwards signals and reaps. The bool is false when this process
// should carry on as the executor itself.
func reapAsInit() (int, bool) {
	if os.Getpid() != 1 {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rafiki: running as PID 1 without a zombie reaper: cannot resolve own executable:", err)
		return 0, false
	}
	code, err := superviseAsInit(exe, os.Args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rafiki: init supervisor:", err)
		return 1, true
	}
	return code, true
}

// superviseAsInit runs argv under exe, forwarding signals to it and reaping
// every child that exits (its own or an adopted orphan) until the main child
// is gone, then returns the main child's exit code (128+signal if killed).
func superviseAsInit(exe string, argv []string) (int, error) {
	// Subscribe before the fork so neither a forwarded signal nor the
	// child's SIGCHLD can be lost in the gap.
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs)
	defer signal.Stop(sigs)

	proc, err := os.StartProcess(exe, argv, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return 0, fmt.Errorf("start %s: %w", exe, err)
	}
	child := proc.Pid
	// We reap with wait4(-1) below; the Process handle must not also try to.
	if err := proc.Release(); err != nil {
		return 0, fmt.Errorf("release child handle: %w", err)
	}

	for sig := range sigs {
		s, ok := sig.(syscall.Signal)
		if !ok {
			continue
		}
		switch s {
		case syscall.SIGCHLD:
			if code, done := reapChildren(child); done {
				return code, nil
			}
		case syscall.SIGURG, syscall.SIGPIPE:
			// Go runtime housekeeping, not something the child should see.
		default:
			if err := syscall.Kill(child, s); err != nil && err != syscall.ESRCH {
				fmt.Fprintf(os.Stderr, "rafiki: forwarding %v to pid %d: %v\n", s, child, err)
			}
		}
	}
	return 0, fmt.Errorf("signal channel closed")
}

// reapChildren collects every exited child without blocking. SIGCHLD
// coalesces, so one delivery can stand for many exits. done reports whether
// main was among them.
func reapChildren(main int) (code int, done bool) {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || pid <= 0 {
			return code, done
		}
		if pid != main {
			continue
		}
		done = true
		if ws.Signaled() {
			code = 128 + int(ws.Signal())
		} else {
			code = ws.ExitStatus()
		}
	}
}
