package daraja

import (
	"strings"
	"testing"
	"time"
)

// collectStdout drains events until it sees want or the deadline passes.
func collectStdout(t *testing.T, h *Host, want string, d time.Duration) string {
	t.Helper()
	deadline := time.After(d)
	var sb strings.Builder
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatalf("event channel closed; got %q, want %q", sb.String(), want)
			}
			if len(ev.Stdout) > 0 {
				sb.Write(ev.Stdout)
				if strings.Contains(sb.String(), want) {
					return sb.String()
				}
			}
		case <-deadline:
			t.Fatalf("timeout; got %q, want %q", sb.String(), want)
		}
	}
}

// The host runs a process and relays what it writes.
func TestHostRelaysStdout(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "echo hello; sleep 30"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	collectStdout(t, h, "hello", 5*time.Second)

	if h.PID() == 0 {
		t.Error("PID is 0 after a successful Start")
	}
	if !h.Running() {
		t.Error("Running is false after a successful Start")
	}
}

// stdin reaches the process.
func TestHostWritesStdin(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/cat"})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	if err := h.WriteStdin([]byte("ping\n")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	collectStdout(t, h, "ping", 5*time.Second)
}

// Restart replaces the process and announces the boundary IN the event stream,
// so a consumer holding per-process state knows exactly where to reset.
func TestHostRestartEmitsBoundaryMarker(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "echo first; sleep 30"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	collectStdout(t, h, "first", 5*time.Second)
	oldPID := h.PID()

	newPID, err := h.Restart([]string{"-c", "echo second; sleep 30"}, time.Second)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if newPID == oldPID {
		t.Fatalf("pid unchanged across restart: %d", newPID)
	}

	// The marker must arrive, and it must arrive BEFORE the new process's bytes.
	deadline := time.After(5 * time.Second)
	sawMarker := false
	for {
		select {
		case ev := <-h.Events():
			switch {
			case ev.Restarted != nil:
				sawMarker = true
				if *ev.Restarted != newPID {
					t.Fatalf("marker pid = %d, want %d", *ev.Restarted, newPID)
				}
			case len(ev.Stdout) > 0:
				if strings.Contains(string(ev.Stdout), "second") {
					if !sawMarker {
						t.Fatal("new process output arrived before the restart marker; " +
							"a consumer would fold it into the old process's state")
					}
					return
				}
			}
		case <-deadline:
			t.Fatal("timeout waiting for restart marker + new output")
		}
	}
}

// Shutdown ends the process and reports how it went.
func TestHostShutdownReportsOutcome(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "sleep 30"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, sig, err := h.Shutdown(time.Second)
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if sig == "" {
		t.Error("signal is empty; a sleeping process must have been signalled")
	}
	if h.Running() {
		t.Error("Running is true after Shutdown")
	}
}

// Shutdown with a chatty child and no consumer used to panic: Shutdown closed
// the event channel while the stdout pump was blocked sending into it.
// os.Process.Wait returns when the process is reaped, NOT when its pipes drain,
// so the pump is essentially always still live at that moment.
func TestShutdownWithBlockedPumpDoesNotPanic(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "while :; do echo x; done"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // fill the buffer so the pump blocks

	if _, _, err := h.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // a surviving pump would panic here

	select {
	case <-h.Done():
	default:
		t.Fatal("Done did not close after Shutdown")
	}
}

// Restart used to emit the boundary marker while holding h.mu, so a full buffer
// with no consumer blocked every other method behind it — including Health.
func TestRestartDoesNotBlockOtherCallsOnASlowConsumer(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "while :; do echo x; done"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	time.Sleep(300 * time.Millisecond)

	go func() { _, _ = h.Restart([]string{"-c", "sleep 30"}, time.Second) }()

	done := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		h.PID()
		h.Running()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("PID/Running blocked behind Restart's marker send; the marker must " +
			"be emitted with h.mu released")
	}
}

// A child that dies on its own must say so: nothing else tells the consumer,
// and Running must stop claiming a process that is gone.
func TestUnexpectedExitEmitsExitedAndClearsRunning(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "exit 7"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.Events():
			if ev.Exited == nil {
				continue
			}
			if ev.Exited.ExitCode != 7 {
				t.Fatalf("exit code = %d, want 7", ev.Exited.ExitCode)
			}
			if h.Running() {
				t.Error("Running is still true after the child exited on its own")
			}
			return
		case <-deadline:
			t.Fatal("no Exited event for a child that exited on its own")
		}
	}
}

// A deliberate stop reports through Shutdown's return value and must NOT also
// arrive as an Exited event; the caller that asked does not need telling twice.
func TestDeliberateShutdownEmitsNoExitedEvent(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/sh", Argv: []string{"-c", "sleep 30"}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, _, err := h.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case ev := <-h.Events():
		if ev.Exited != nil {
			t.Fatal("deliberate Shutdown also emitted an Exited event")
		}
	default:
	}
}
