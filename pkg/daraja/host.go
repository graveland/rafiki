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

// HostOptions describes the process to host.
type HostOptions struct {
	Binary string
	Argv   []string
	Cwd    string
	Env    []string
}

// Host owns one child process. Its methods are safe for concurrent use: the
// service serves Restart, Shutdown and Health on the same connection as Relay.
type Host struct {
	opts HostOptions

	mu      sync.Mutex
	runner  child.Runner
	stdin   io.WriteCloser
	running bool

	events chan Event
	// closeOnce guards events, which exactly one goroutine may close.
	closeOnce sync.Once
}

func NewHost(opts HostOptions) *Host {
	return &Host{opts: opts, events: make(chan Event, eventBuffer)}
}

// Events returns the ordered event stream. One consumer.
func (h *Host) Events() <-chan Event { return h.events }

// Start launches the process.
func (h *Host) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.startLocked(h.opts.Argv)
}

// startLocked launches a process. h.mu must be held.
func (h *Host) startLocked(argv []string) error {
	if h.running {
		return errors.New("daraja: already running")
	}
	runner, err := child.NewProcessRunner(child.SpawnSpec{
		PiBinary:    h.opts.Binary,
		Argv:        argv,
		Cwd:         h.opts.Cwd,
		Env:         h.opts.Env,
		EnvOverride: len(h.opts.Env) > 0,
	})
	if err != nil {
		return fmt.Errorf("daraja: build runner: %w", err)
	}
	stdin, stdout, stderr, err := runner.Start()
	if err != nil {
		return fmt.Errorf("daraja: start: %w", err)
	}
	h.runner, h.stdin, h.running = runner, stdin, true

	go h.pump(stdout)
	// stderr is drained and discarded: the controller reads the child's
	// protocol on stdout, and an undrained pipe eventually blocks the writer.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	return nil
}

// pump copies stdout into the event stream until EOF.
func (h *Host) pump(stdout io.ReadCloser) {
	defer stdout.Close()
	buf := make([]byte, stdoutChunk)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			h.events <- Event{Stdout: chunk}
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

// Restart signals the process, waits for it, and launches a replacement with
// argv. The boundary marker is emitted BEFORE the new process can write, which
// is what lets a consumer reset its per-process state at the right point.
func (h *Host) Restart(argv []string, grace time.Duration) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.running {
		h.stopLocked(grace, true)
	}
	if err := h.startLocked(argv); err != nil {
		return 0, err
	}
	pid := h.runner.PID()
	h.events <- Event{Restarted: &pid}
	return pid, nil
}

// Shutdown ends the process and reports the outcome. Idempotent.
func (h *Host) Shutdown(grace time.Duration) (int, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running {
		return 0, "", nil
	}
	code, sig := h.stopLocked(grace, false)
	h.closeOnce.Do(func() { close(h.events) })
	return code, sig, nil
}

// stopLocked interrupts, waits out grace, escalates to Kill, and reaps.
// h.mu must be held. interrupt selects SIGINT (a turn abort) over SIGTERM.
func (h *Host) stopLocked(grace time.Duration, interrupt bool) (int, string) {
	if grace <= 0 {
		grace = defaultGrace
	}
	if interrupt {
		_ = h.runner.Interrupt()
	} else {
		_ = h.runner.Terminate()
	}

	type outcome struct {
		code int
		sig  string
	}
	done := make(chan outcome, 1)
	runner := h.runner
	go func() {
		code, sig := runner.Wait()
		done <- outcome{code, sig}
	}()

	select {
	case o := <-done:
		h.running, h.stdin, h.runner = false, nil, nil
		return o.code, o.sig
	case <-time.After(grace):
		_ = runner.Kill()
		o := <-done
		h.running, h.stdin, h.runner = false, nil, nil
		return o.code, o.sig
	}
}
