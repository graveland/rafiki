package child_test

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func fakePiPath(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(here), "..", "..")
	return filepath.Join(repoRoot, "test", "integration", "fake-pi.sh")
}

func TestChild_SpawnAndCleanShutdown(t *testing.T) {
	ck := assert.NewAborting(t)
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
	}

	c, err := child.Spawn(context.Background(), spec)
	ck.NoError(err)
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	// Wait for the supervise loop to enter the read/write loop.
	select {
	case <-c.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready timed out")
	}

	// Graceful shutdown should exit cleanly without escalation.
	res, err := c.Shutdown(time.Second*5, time.Second)
	ck.NoError(err)
	ck.False(res.Escalated, "clean exit should not need SIGTERM")
	ck.Eq(0, res.ExitCode, "exit code")
}

func TestChild_StuckProcess_Escalates(t *testing.T) {
	ck := assert.NewAborting(t)
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
		Env:      []string{"FAKE_PI_SHUTDOWN_DELAY=999"},
	}

	c, err := child.Spawn(context.Background(), spec)
	ck.NoError(err)
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	select {
	case <-c.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready timed out")
	}

	// Short shutdown timeout; expect escalation to SIGTERM.
	res, err := c.Shutdown(100*time.Millisecond, 500*time.Millisecond)
	ck.NoError(err)
	ck.True(res.Escalated, "expected escalation")
}

// TestChild_StuckProcess_SignalExitCodeIsZero pins the pre-Runner-seam
// contract for a process that is escalated to signal termination: ExitCode
// stays 0 (its zero value), not -1. -1 is reserved for the case where Wait()
// itself errored and the outcome is genuinely indeterminate; a process that
// exited via signal is a determinate outcome recorded separately in Signal.
// This guards against a regression where processRunner.Wait() returned -1
// for any state.ExitCode() < 0, which also fires for the signalled-but-known
// case and silently changed the ExitCode the wire API reports for every
// escalated Shutdown/Interrupt.
func TestChild_StuckProcess_SignalExitCodeIsZero(t *testing.T) {
	ck := assert.NewAborting(t)
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
		Env:      []string{"FAKE_PI_SHUTDOWN_DELAY=999"},
	}

	c, err := child.Spawn(context.Background(), spec)
	ck.NoError(err)
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	select {
	case <-c.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready timed out")
	}

	// Short shutdown timeout; expect escalation to SIGTERM (or SIGKILL).
	res, err := c.Shutdown(100*time.Millisecond, 500*time.Millisecond)
	ck.NoError(err)
	ck.True(res.Escalated, "expected escalation")
	ck.NotEq("", res.Signal, "expected a recorded signal name")
	ck.Eq(0, res.ExitCode, "ExitCode = %d, want 0 (signal-terminated, see Signal=%q); -1 is reserved for an indeterminate Wait() error", res.ExitCode, res.Signal)
}

func TestChild_BinaryMissing_SpawnFails(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: "/this/path/does/not/exist",
	}
	_, err := child.Spawn(context.Background(), spec)
	assert.NewAborting(t).Error(err, "expected spawn failure")
}

func TestChild_KickstartAndMetadata(t *testing.T) {
	ck := assert.NewCollecting(t)
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
		Env:      []string{"FAKE_PI_SESSION_ID=test-sid", "FAKE_PI_SESSION_NAME=initial"},
	}

	c, err := child.Spawn(context.Background(), spec)
	ck.Require().NoError(err)
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	// Wait for the kickstart get_state response to arrive.
	select {
	case <-c.Idle():
	case <-time.After(2 * time.Second):
		t.Fatalf("did not transition to idle: %v", c.Status())
	}

	ck.Require().Eq(protocol.StatusIdle, c.Status(), "status after idle")

	md := c.Metadata()
	ck.Require().Eq("test-sid", md.SessionID, "sessionId: got")
	ck.Require().Eq("initial", md.SessionName, "sessionName: got")
	ck.NotEq("", md.SessionFile, "SessionFile not extracted")
	ck.NotEq("", md.Model, "Model not extracted")
}

func TestChild_BeginShutdown(t *testing.T) {
	ck := assert.NewAborting(t)
	// BeginShutdown should drive the SM from idle to shutting_down and report
	// the previous status correctly.
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
	}

	c, err := child.Spawn(context.Background(), spec)
	ck.NoError(err)
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	// Wait until the child is idle (SM is in StatusIdle).
	select {
	case <-c.Idle():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not reach idle")
	}

	ck.Eq(protocol.StatusIdle, c.Status(), "pre-shutdown status")

	// First call: should transition idle → shutting_down.
	changed, prev := c.BeginShutdown()
	ck.True(changed, "BeginShutdown: expected transition to occur")
	ck.Eq(protocol.StatusIdle, prev, "BeginShutdown: prev")
	ck.Eq(protocol.StatusShuttingDown, c.Status(), "status after BeginShutdown")

	// Second call: already shutting_down, should be a no-op.
	changed2, _ := c.BeginShutdown()
	ck.False(changed2, "BeginShutdown: second call should not report a transition")
}

func TestChild_ProcessExits_ReadyStillFires(t *testing.T) {
	// If pi exits immediately Done() must still close — the supervise loop
	// must reap the process and signal completion even without any output.
	spec := child.SpawnSpec{
		ChildID:   "c_test",
		Cwd:       t.TempDir(),
		PiBinary:  "/bin/sh",
		ExtraArgs: []string{"-c", "exit 0"},
	}
	c, err := child.Spawn(context.Background(), spec)
	assert.NewAborting(t).NoError(err)
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed for instantly-exiting child")
	}
}

func TestChild_InterruptSendsSIGINT(t *testing.T) {
	c := assert.NewAborting(t)
	// The fake child installs NO signal trap, so a default-disposition SIGINT
	// terminates it and is recorded as exit signal "interrupt". Asserting on the
	// recorded exit signal proves Interrupt() delivers SIGINT specifically,
	// without depending on a shell trap firing — trapped-SIGINT delivery to bash
	// subprocesses is unreliable under the `go test` harness (an externally sent
	// SIGINT terminates the shell but its INT trap does not run, while a
	// self-sent one does). The graceful claude-interrupt behavior (the
	// "[Request interrupted by user]" result) is covered by the live smoke test.
	//
	// A test binary started with SIGINT ignored (e.g. backgrounded by a
	// non-job-control shell) passes SIG_IGN through exec to the fake child,
	// which then survives the signal. Installing a handler makes exec reset
	// SIGINT to its default disposition in the child.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	t.Cleanup(func() { signal.Stop(sigCh) })
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	body := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s1\"}'\n" +
		"while true; do sleep 0.05; done\n"
	c.NoError(os.WriteFile(script, []byte(body), 0o755), "write script")
	cwd, _ := os.Getwd()
	ch, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  "c_int",
		Cwd:      cwd,
		PiBinary: script,
		Provider: child.ClaudeProvider{},
	})
	c.NoError(err, "spawn")
	t.Cleanup(func() { _, _ = ch.Shutdown(100*time.Millisecond, 100*time.Millisecond) })

	select {
	case <-ch.Idle():
	case <-time.After(15 * time.Second):
		// Generous for the same reason as the exit deadline below: the fixture
		// is a bash loop with a 50ms sleep, and what is being asserted is that
		// it STARTS, not that it starts quickly.
		t.Fatal("never idle")
	}
	time.Sleep(100 * time.Millisecond) // ensure the process is fully running

	c.NoError(ch.Interrupt(), "interrupt")

	// Generous: the assertion is that SIGINT TERMINATES the child, not that it
	// does so within any particular time. The loop returns as soon as the
	// signal is recorded, so a longer deadline costs nothing on a healthy run
	// and stops a loaded machine from failing the test on scheduling alone.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if sig := ch.ExitResult().Signal; sig != "" {
			c.Eq(syscall.SIGINT.String(), sig, "child exit signal")
			return // exited via SIGINT — Interrupt() delivered the right signal
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child did not exit after Interrupt()")
}

func TestChild_InterruptAfterExitIsNoOp(t *testing.T) {
	c := assert.NewAborting(t)
	// Interrupt() on an already-exited child must be a no-op returning nil.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	body := "#!/bin/bash\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nexit 0\n"
	c.NoError(os.WriteFile(script, []byte(body), 0o755), "write script")
	cwd, _ := os.Getwd()
	ch, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  "c_int2",
		Cwd:      cwd,
		PiBinary: script,
		Provider: child.ClaudeProvider{},
	})
	c.NoError(err, "spawn")
	// Wait for the process to exit and be reaped (Done closes after the supervise
	// loop sets closed), so Interrupt hits the already-closed branch.
	select {
	case <-ch.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
	c.NoError(ch.Interrupt(), "Interrupt() on exited child")
}

// message_update frames are redundant with the message_end that follows them,
// but at streaming volume they evict everything else from the bounded ring —
// which is what attach primes scrollback from. This drives frames through the
// real readStdout path (a spawned child's actual stdout), not ring.Append
// directly, so it proves the filter that's wired into production, not just a
// helper function in isolation.
func TestRingSkipsMessageUpdateButKeepsEverythingElse(t *testing.T) {
	ck := assert.NewAborting(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	body := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"message_update\",\"message\":{\"role\":\"assistant\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"message_update\",\"message\":{\"role\":\"assistant\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"tool_execution_start\",\"toolCallId\":\"t1\"}'\n"
	ck.NoError(os.WriteFile(script, []byte(body), 0o755), "write script")

	// No Provider set: defaults to IdentityProvider, the identity provider, so the
	// raw stdout lines above land in the ring exactly as printed.
	c, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  "c_ring_filter",
		Cwd:      dir,
		PiBinary: script,
	})
	ck.NoError(err, "spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond) })

	// The script never reads stdin and exits after emitting its frames, so
	// Done() closes once readStdout drains EOF and the process is reaped.
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit")
	}

	got := typesOf(c.RingSnapshot())
	want := []string{"message_start", "message_end", "tool_execution_start"}
	ck.EqDiff(want, got, "ring contents")
}

// A frame the type-sniffing json.Unmarshal can't parse must still land in the
// ring — a parse failure is not grounds for silently dropping a child's
// output. Exercises the isMessageUpdate false-on-error path through the real
// readStdout path, not by calling it directly.
func TestRingKeepsUnparseableFrame(t *testing.T) {
	ck := assert.NewAborting(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	body := "#!/bin/bash\n" +
		"printf '%s\\n' 'this is not json'\n" +
		"printf '%s\\n' '{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\"}}'\n"
	ck.NoError(os.WriteFile(script, []byte(body), 0o755), "write script")

	c, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  "c_ring_unparseable",
		Cwd:      dir,
		PiBinary: script,
	})
	ck.NoError(err, "spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond) })

	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit")
	}

	frames := c.RingSnapshot()
	ck.Len(frames, 2, "ring has %d frames, want 2 (unparseable frame must not be dropped): %v", len(frames), typesOf(frames))
	ck.Eq("this is not json", string(frames[0]), "frames[0] = %q, want the unparseable line preserved verbatim", frames[0])
}

// typesOf extracts the top-level "type" field from each ring frame, in order.
func typesOf(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		var hdr struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(f, &hdr); err != nil {
			out = append(out, "<unparseable>")
			continue
		}
		out = append(out, hdr.Type)
	}
	return out
}
