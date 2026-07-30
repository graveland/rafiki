package child

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// Runner abstracts a child's execution context: how it starts, how its three
// streams are obtained, how it is waited on, and how it is signalled. The
// supervise and readStdout loops are written against the streams alone, so they
// are identical for a subprocess and for an in-process engine.
//
// Runner is exported and injected via SpawnSpec rather than selected inside this
// package: internal/agent already imports internal/child, so an in-process
// implementation cannot live here without an import cycle.
type Runner interface {
	// Start begins execution and returns the child's three streams. stdin is
	// written by the supervise loop, stdout is read by readStdout, and stderr is
	// drained into the child's error buffer. An implementation with no separate
	// diagnostic stream must return a reader at EOF, never nil.
	Start() (stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser, err error)

	// Wait blocks until execution has finished and reports the outcome.
	// exitCode is -1 when it could not be determined; signal is empty when the
	// runner was not signalled.
	Wait() (exitCode int, signal string)

	// PID reports the OS process id, or 0 when there is no process.
	PID() int

	// Terminate requests a graceful stop.
	Terminate() error

	// Kill forces an immediate stop.
	Kill() error

	// Interrupt asks the runner to abort its current turn without stopping.
	Interrupt() error
}

// processRunner runs a child as a subprocess: the behaviour Child had before
// the Runner seam existed.
type processRunner struct {
	cmd *exec.Cmd
}

// newProcessRunner builds a subprocess runner for spec. The child gets its own
// process group so subprocesses it spawns can be signalled as a group during
// shutdown — otherwise an orphan keeps a pipe write end open and blocks our
// readers.
func newProcessRunner(spec SpawnSpec) (*processRunner, error) {
	if spec.PiBinary == "" {
		return nil, errors.New("pi binary path required")
	}
	argv := append([]string{}, spec.Argv...)
	argv = append(argv, spec.ExtraArgs...)

	cmd := exec.Command(spec.PiBinary, argv...)
	cmd.Dir = spec.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(spec.Env) > 0 {
		if spec.EnvOverride {
			cmd.Env = append([]string{}, spec.Env...)
		} else {
			cmd.Env = append(os.Environ(), spec.Env...)
		}
	}
	return &processRunner{cmd: cmd}, nil
}

func (p *processRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := p.cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start: %w", err)
	}
	return stdin, stdout, stderr, nil
}

func (p *processRunner) Wait() (int, string) {
	state, err := p.cmd.Process.Wait()
	if err != nil {
		return -1, ""
	}
	// -1 is reserved for the err != nil case above (Wait() itself failed, so
	// the outcome is genuinely indeterminate). A process that exited via
	// signal has state.ExitCode() == -1 too, but that is a determinate
	// outcome — the pre-seam code left ExitCode at its zero value (0) for a
	// signalled process, recording the signal separately in Signal. Preserve
	// that exact contract: 0 here means "terminated by signal, see Signal",
	// not "unknown." This is API-visible (Session.ExitCode, ChildSummary.ExitCode
	// on the wire), so do not "fix" this to -1 without a deliberate, separate
	// decision to change that contract.
	code := 0
	if state.ExitCode() >= 0 {
		code = state.ExitCode()
	}
	sig := ""
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig = ws.Signal().String()
	}
	return code, sig
}

func (p *processRunner) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *processRunner) Terminate() error { return p.signalGroup(syscall.SIGTERM) }
func (p *processRunner) Kill() error      { return p.signalGroup(syscall.SIGKILL) }
func (p *processRunner) Interrupt() error { return p.signalGroup(syscall.SIGINT) }

// signalGroup signals the whole process group (negative PID) so subprocesses the
// child spawned are signalled too. A process that exited between the caller's
// liveness check and here yields ESRCH — that is the no-op the caller asked
// for, not an error.
func (p *processRunner) signalGroup(sig syscall.Signal) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("signal: no process handle")
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
