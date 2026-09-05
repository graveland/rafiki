package daraja

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testChildBinary writes a fake child: a shell script with the given body that
// ignores its arguments. Ignoring them is the point — the host now builds argv
// through claudeargv.Build, so the fake runs under claude's flags (-p
// --input-format stream-json ...) and /bin/sh or /bin/cat would die on the
// first one.
func testChildBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-child")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// testEchoBinary is the long-lived fake child: it announces itself by echoing
// its argv and stays up, so a restart test can tell the processes apart by the
// model the spec names.
func testEchoBinary(t *testing.T) string {
	t.Helper()
	return testChildBinary(t, `echo "$@"; sleep 30`)
}

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
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	collectStdout(t, h, "stream-json", 5*time.Second)

	if h.PID() == 0 {
		t.Error("PID is 0 after a successful Start")
	}
	if !h.Running() {
		t.Error("Running is false after a successful Start")
	}
}

// TestProxyModelArgsReplacesPlainModelFlag proves the suppression startLocked
// applies: with ProxyModelArgs set, the spawned process's argv carries the
// proxy's own --model pair (matching its custom-model-option env vars) and
// does NOT also carry a second, plain --model from claudeargv.Build — which
// would risk Claude Code's client-side allowlist rejecting the model before
// the custom option is even consulted (see HostOptions.ProxyModelArgs).
func TestProxyModelArgsReplacesPlainModelFlag(t *testing.T) {
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "openai/gpt-4o"},
		ProxyModelArgs: []string{
			"--model", "rafiki: openai/gpt-4o",
		},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	argv := collectStdout(t, h, "stream-json", 5*time.Second)

	if got := strings.Count(argv, "--model"); got != 1 {
		t.Fatalf("argv %q contains %d occurrences of --model, want exactly 1", argv, got)
	}
	if !strings.Contains(argv, "rafiki: openai/gpt-4o") {
		t.Fatalf("argv %q missing the proxy's own --model value", argv)
	}
	if strings.Contains(argv, "--model openai/gpt-4o") {
		t.Fatalf("argv %q carries the PLAIN --model claudeargv.Build would add unsuppressed", argv)
	}
}

// TestNilProxyModelArgsLeavesPlainModelFlagAlone is the control case: with no
// ProxyModelArgs (every caller before Phase 2, and any unproxied daraja
// today), Model must still produce claudeargv.Build's ordinary --model.
func TestNilProxyModelArgsLeavesPlainModelFlagAlone(t *testing.T) {
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "claude-sonnet-5"},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	argv := collectStdout(t, h, "stream-json", 5*time.Second)
	if !strings.Contains(argv, "--model claude-sonnet-5") {
		t.Fatalf("argv %q missing the plain --model claude-sonnet-5", argv)
	}
}

// stdin reaches the process.
func TestHostWritesStdin(t *testing.T) {
	h := NewHost(HostOptions{Binary: testChildBinary(t, "cat"), Spec: ChildSpec{Kind: KindClaude}})
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
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "first"},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	collectStdout(t, h, "first", 5*time.Second)
	oldPID := h.PID()

	newPID, err := h.Restart(ChildSpec{Kind: KindClaude, Model: "second"}, time.Second)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if newPID == oldPID {
		t.Fatalf("pid unchanged across restart: %d", newPID)
	}

	// The marker must arrive, and it must arrive BEFORE the new process's bytes.
	deadline := time.After(5 * time.Second)
	sawMarker := false
	var got strings.Builder
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
				got.Write(ev.Stdout)
				if strings.Contains(got.String(), "second") {
					if !sawMarker {
						t.Fatal("new process output arrived before the restart marker; " +
							"a consumer would fold it into the old process's state")
					}
					return
				}
			}
		case <-deadline:
			t.Fatalf("timeout waiting for restart marker + new output; got %q", got.String())
		}
	}
}

// A Restart with no spec must reuse the held one. The alternative — treating an
// absent spec as an empty one — would relaunch claude with no --output-format
// and no --resume: a running process that emits nothing parseable and has lost
// the conversation.
func TestRestartWithNoSpecReusesTheHeldOne(t *testing.T) {
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "m1", ResumeSession: "s1"},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	if _, err := h.Restart(ChildSpec{}, time.Second); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	h.mu.Lock()
	got := h.spec
	h.mu.Unlock()
	if got.Model != "m1" || got.ResumeSession != "s1" {
		t.Errorf("after a spec-less Restart the host holds %+v, want the original", got)
	}
}

// Shutdown ends the process and reports how it went.
func TestHostShutdownReportsOutcome(t *testing.T) {
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
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
	h := NewHost(HostOptions{
		Binary: testChildBinary(t, `while :; do echo x; done`),
		Spec:   ChildSpec{Kind: KindClaude},
	})
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
	h := NewHost(HostOptions{
		Binary: testChildBinary(t, `while :; do echo x; done`),
		Spec:   ChildSpec{Kind: KindClaude},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	time.Sleep(300 * time.Millisecond)

	go func() { _, _ = h.Restart(ChildSpec{}, time.Second) }()

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
	h := NewHost(HostOptions{Binary: testChildBinary(t, "exit 7"), Spec: ChildSpec{Kind: KindClaude}})
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
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
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

// testShortLivedBinary returns a binary that exits immediately and
// successfully, standing in for a claude that dies on its own.
func testShortLivedBinary(t *testing.T) string {
	t.Helper()
	return "/usr/bin/true"
}

// A child that dies on its own must come back: when the controller's connection
// is also down, nothing else can restart it, and the alternative is a daraja
// hosting nothing.
func TestUnexpectedExitRespawnsTheChild(t *testing.T) {
	h := NewHost(HostOptions{
		Binary:         testShortLivedBinary(t),
		Spec:           ChildSpec{Kind: KindClaude},
		RespawnBackoff: time.Millisecond,
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	// The first exit is reported, then a replacement is announced.
	var sawExit, sawRestart bool
	deadline := time.After(10 * time.Second)
	for !sawRestart {
		select {
		case ev := <-h.Events():
			switch {
			case ev.Exited != nil:
				sawExit = true
			case ev.Restarted != nil:
				sawRestart = true
			}
		case <-deadline:
			t.Fatalf("timed out; sawExit=%v sawRestart=%v", sawExit, sawRestart)
		}
	}
	if !sawExit {
		t.Error("a respawn was announced without the exit that caused it")
	}
}

// A child that dies instantly and forever — a bad --resume, a missing binary —
// must stop being respawned, or daraja forks at whatever rate the kernel
// allows for as long as it lives.
func TestRespawnStopsAtTheLimit(t *testing.T) {
	h := NewHost(HostOptions{
		Binary:         testShortLivedBinary(t),
		Spec:           ChildSpec{Kind: KindClaude},
		RespawnBackoff: time.Millisecond,
		RespawnLimit:   2,
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	var restarts int
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-h.Events():
			if ev.Restarted != nil {
				restarts++
				if restarts > 2 {
					t.Fatalf("respawned %d times, want at most 2", restarts)
				}
			}
		case <-h.Done():
			if restarts != 2 {
				t.Errorf("host finished after %d respawns, want 2", restarts)
			}
			return
		case <-deadline:
			t.Fatalf("host never gave up; restarts=%d", restarts)
		}
	}
}
