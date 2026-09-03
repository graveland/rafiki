// SPDX-License-Identifier: Apache-2.0

// Package daraja hosts exactly one child process on behalf of a remote
// controller and relays its stdio.
//
// It deliberately understands nothing about the child's protocol. Parsing,
// translation and persistence live in rafikid, which is the only side that has
// the conversation state to attach them to.
package daraja

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/claudeargv"
)

// defaultGrace is how long Restart and Shutdown wait after the first signal
// before escalating to a hard kill.
const defaultGrace = 3 * time.Second

// stdoutChunk bounds one read from the child's stdout.
const stdoutChunk = 32 * 1024

// eventBuffer is how many events may queue before the reader blocks.
//
// Blocking is the correct backpressure: dropping the child's output to keep a
// slow consumer happy would silently truncate a conversation, and the consumer
// is the only side that can persist it.
const eventBuffer = 64

// Event is one ordered thing that happened to the hosted process.
//
// Exactly one field is set. They share a channel rather than living on separate
// ones because their ORDER is the contract: a restart marker means "everything
// after this came from a different process", which is meaningless unless it is
// sequenced against the bytes.
type Event struct {
	Stdout    []byte
	Restarted *int
	Exited    *ExitInfo
}

// ExitInfo reports how the process ended. ExitCode is 0 for a signalled
// process, which reports in Signal instead; -1 means indeterminate.
type ExitInfo struct {
	ExitCode int
	Signal   string
}

// ChildSpec is the typed description of the child process, held so daraja can
// rebuild the command line without being told it again — on a caller's Restart
// that omits one, and on its own respawn after an unexpected exit.
type ChildSpec struct {
	Kind           string
	Model          string
	ResumeSession  string
	PermissionMode string
}

// KindClaude is the only child protocol daraja hosts today.
const KindClaude = "claude"

// argv builds the child's command line for this spec. An unknown kind returns
// nil, which startLocked reports as an error rather than launching a bare
// binary with no arguments — a claude with no --output-format runs, and emits
// something nothing downstream can parse.
func (s ChildSpec) argv() []string {
	if s.Kind != KindClaude {
		return nil
	}
	return claudeargv.Build(claudeargv.Params{
		Model:          s.Model,
		ResumeSession:  s.ResumeSession,
		PermissionMode: s.PermissionMode,
	})
}

// HostOptions describes the process to host.
type HostOptions struct {
	Binary string
	Spec   ChildSpec
	Cwd    string
	Env    []string

	// EnvOverride replaces the inherited environment with Env instead of
	// appending to it.
	//
	// Explicit rather than inferred from len(Env), which is what this used to
	// be: passthrough auth is defined by the ABSENCE of ANTHROPIC_AUTH_TOKEN
	// and ANTHROPIC_API_KEY, so a caller that builds a complete environment
	// needs override, while a caller adding one variable needs the opposite —
	// and inferring it from a length silently gives the second caller the
	// first behaviour, dropping HOME, PATH and the credential store the child
	// needs.
	EnvOverride bool
}

// Host owns one child process. Its methods are safe for concurrent use: the
// service serves Restart, Shutdown and Health on the same connection as Relay.
type Host struct {
	opts HostOptions

	// spec is the child description currently hosted, updated by every
	// successful startLocked so a spec-less Restart reuses it. Guarded by mu.
	spec ChildSpec

	mu      sync.Mutex
	runner  child.Runner
	stdin   io.WriteCloser
	exitCh  chan ExitInfo
	running bool

	events chan Event

	// done closes when the host is finished, releasing every blocked sender
	// and telling the consumer to stop.
	//
	// events is deliberately NEVER closed. It has several senders — the stdout
	// pump, the exit watcher, and Restart's marker — and closing a channel out
	// from under a blocked sender panics the process, which is exactly what
	// happened when Shutdown closed it directly. A done channel every sender
	// and the consumer select on removes the whole class.
	done     chan struct{}
	doneOnce sync.Once
}

func NewHost(opts HostOptions) *Host {
	return &Host{
		opts:   opts,
		spec:   opts.Spec,
		events: make(chan Event, eventBuffer),
		done:   make(chan struct{}),
	}
}

// Events returns the ordered event stream. One consumer. Never closed — select
// on Done to learn that the host has finished.
func (h *Host) Events() <-chan Event { return h.events }

// Done closes when the host has finished and no further events will be sent.
func (h *Host) Done() <-chan struct{} { return h.done }

// emit publishes ev, or reports false if the host finished first.
func (h *Host) emit(ev Event) bool {
	select {
	case h.events <- ev:
		return true
	case <-h.done:
		return false
	}
}

// Start launches the process.
func (h *Host) Start() error {
	h.mu.Lock()
	stdout, err := h.startLocked(h.spec)
	h.mu.Unlock()
	if err != nil {
		return err
	}
	go h.pump(stdout)
	return nil
}

// startLocked launches a process and returns its stdout WITHOUT pumping it.
//
// The caller starts the pump, because Restart must emit its boundary marker
// before the replacement's first byte can reach the consumer — and it cannot
// emit while holding h.mu. Returning the pipe unread is what lets it do both.
// h.mu must be held.
func (h *Host) startLocked(spec ChildSpec) (io.ReadCloser, error) {
	if h.running {
		return nil, errors.New("daraja: already running")
	}
	argv := spec.argv()
	if argv == nil {
		return nil, fmt.Errorf("daraja: unsupported child kind %q", spec.Kind)
	}
	h.spec = spec
	runner, err := child.NewProcessRunner(child.SpawnSpec{
		PiBinary:    h.opts.Binary,
		Argv:        argv,
		Cwd:         h.opts.Cwd,
		Env:         h.opts.Env,
		EnvOverride: h.opts.EnvOverride,
	})
	if err != nil {
		return nil, fmt.Errorf("daraja: build runner: %w", err)
	}
	stdin, stdout, stderr, err := runner.Start()
	if err != nil {
		return nil, fmt.Errorf("daraja: start: %w", err)
	}
	exitCh := make(chan ExitInfo, 1)
	h.runner, h.stdin, h.exitCh, h.running = runner, stdin, exitCh, true

	go h.watch(runner, exitCh)
	// stderr is drained and discarded: the controller reads the child's
	// protocol on stdout, and an undrained pipe eventually blocks the writer.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	return stdout, nil
}

// watch reaps the process exactly once and decides whether anybody asked for
// it. There is ONE waiter per process: os.Process.Wait is not safe to call
// twice, so stopLocked reads this outcome rather than waiting itself.
func (h *Host) watch(runner child.Runner, exitCh chan ExitInfo) {
	code, sig := runner.Wait()
	info := ExitInfo{ExitCode: code, Signal: sig}
	exitCh <- info // buffered: a deliberate stop may or may not be reading

	// stopLocked holds h.mu for its whole body and clears h.runner before
	// returning, so an exit it caused is identified by the runner no longer
	// being the current one. Anything else is the child dying on its own.
	h.mu.Lock()
	unexpected := h.runner == runner
	if unexpected {
		h.running, h.stdin = false, nil
	}
	h.mu.Unlock()

	if unexpected {
		h.emit(Event{Exited: &info})
	}
}

// pump copies stdout into the event stream until EOF or shutdown.
func (h *Host) pump(stdout io.ReadCloser) {
	defer stdout.Close()
	buf := make([]byte, stdoutChunk)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if !h.emit(Event{Stdout: chunk}) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// PID reports the hosted process id, or 0.
func (h *Host) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runner == nil {
		return 0
	}
	return h.runner.PID()
}

// Running reports whether a process is currently hosted.
func (h *Host) Running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// WriteStdin forwards bytes to the child verbatim.
func (h *Host) WriteStdin(p []byte) error {
	h.mu.Lock()
	stdin, running := h.stdin, h.running
	h.mu.Unlock()
	if !running || stdin == nil {
		return errors.New("daraja: no process")
	}
	_, err := stdin.Write(p)
	return err
}

// Restart signals the process, waits for it, and launches a replacement.
//
// A zero spec means "reuse the one I am holding", which is what a caller
// restarting for any reason other than changing the child's parameters wants.
func (h *Host) Restart(spec ChildSpec, grace time.Duration) (int, error) {
	h.mu.Lock()
	if spec == (ChildSpec{}) {
		spec = h.spec
	}
	if h.running {
		h.stopLocked(grace, true)
	}
	stdout, err := h.startLocked(spec)
	if err != nil {
		h.mu.Unlock()
		return 0, err
	}
	pid := h.runner.PID()
	h.mu.Unlock()

	// The marker is emitted with the lock RELEASED — this send blocks until the
	// consumer drains, and blocking under h.mu wedges Health, WriteStdin and
	// Shutdown behind a slow reader. Ordering still holds because the
	// replacement's stdout is not pumped until after this returns.
	h.emit(Event{Restarted: &pid})
	go h.pump(stdout)
	return pid, nil
}

// Shutdown ends the process and reports the outcome. Idempotent.
//
// No Exited event is emitted: the outcome is this call's return value, and the
// caller that asked for the shutdown does not need telling twice. Exited is for
// a child that died on its own.
func (h *Host) Shutdown(grace time.Duration) (int, string, error) {
	h.mu.Lock()
	var info ExitInfo
	if h.running {
		info = h.stopLocked(grace, false)
	}
	h.mu.Unlock()

	h.doneOnce.Do(func() { close(h.done) })
	return info.ExitCode, info.Signal, nil
}

// stopLocked signals, waits out grace, escalates to Kill, and takes the
// outcome from the watcher. h.mu must be held; interrupt selects SIGINT (a turn
// abort) over SIGTERM.
//
// This does block while holding h.mu, deliberately: the wait is bounded by
// grace plus a reap, and releasing the lock would let a concurrent Restart
// start a process this call is about to declare stopped.
func (h *Host) stopLocked(grace time.Duration, interrupt bool) ExitInfo {
	if grace <= 0 {
		grace = defaultGrace
	}
	runner, exitCh := h.runner, h.exitCh
	if interrupt {
		_ = runner.Interrupt()
	} else {
		_ = runner.Terminate()
	}

	var info ExitInfo
	select {
	case info = <-exitCh:
	case <-time.After(grace):
		_ = runner.Kill()
		info = <-exitCh
	}
	h.running, h.stdin, h.runner, h.exitCh = false, nil, nil, nil
	return info
}
