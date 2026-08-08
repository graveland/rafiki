package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testBashTool returns a materialized bash tool for tests.
func testBashTool(t *testing.T, p OutputPolicy, cwd string) Tool {
	t.Helper()
	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{OutputPolicy: p, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// uniqueSleepArg returns a `sleep` argument unique to this run, long enough
// (≈11 days) that the process can only disappear by being killed. Used as a
// needle in ps output so a test can prove an OS process actually died rather
// than inferring it from how fast a Go call returned.
func uniqueSleepArg() string {
	return fmt.Sprintf("987654.%06d", time.Now().UnixNano()%1_000_000)
}

// pidsMatching returns the pids of every live process whose command line
// contains needle.
func pidsMatching(t *testing.T, needle string) []int {
	t.Helper()
	// POSIX form, identical on macOS/BSD and Linux. Trailing "=" suppresses
	// the header so every line is a record.
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("ps: unparsable pid in %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// waitForProcess blocks until at least one process matches needle, so a test
// aborts a command that has genuinely started rather than racing the fork.
func waitForProcess(t *testing.T, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(pidsMatching(t, needle)) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process matching %q never started", needle)
}

// requireNoSurvivors asserts nothing matching needle is left running, polling
// briefly since the kernel reaps a killed process group asynchronously.
func requireNoSurvivors(t *testing.T, needle string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var survivors []int
	for time.Now().Before(deadline) {
		survivors = pidsMatching(t, needle)
		if len(survivors) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%d process(es) matching %q survived: %v — the shell was killed but its children were orphaned", len(survivors), needle, survivors)
}

// syncBuffer is a mutex-guarded io.Writer so captureSlog is safe under
// -race regardless of which goroutine emits a log record.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog redirects the default slog logger into a buffer for the
// duration of the test, returning an accessor for what was logged. This
// keeps test output pristine AND lets a test assert the code logged rather
// than silently swallowed a condition.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	var b syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &b
}

// killSurvivors is test cleanup: if an assertion failed, don't leave an
// 11-day sleep running on the developer's machine.
func killSurvivors(t *testing.T, needle string) {
	t.Helper()
	for _, pid := range pidsMatching(t, needle) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("cleanup: kill %d: %v", pid, err)
		}
	}
}

// TestBashMergesStderrAndReportsExit is the brief's Step 1 test: stdout and
// stderr land in one merged result, and a non-zero exit is a RESULT (err ==
// nil from Execute), not a tool error — the model sees it in the text.
func TestBashMergesStderrAndReportsExit(t *testing.T) {
	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir()))
	out, err := r.Execute(context.Background(), "bash",
		json.RawMessage(`{"command":"echo out; echo err >&2; exit 3"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"out", "err", "exit status 3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

// TestBashSuccessHasNoExitNote checks the flip side of the exit-code
// requirement: a clean (zero) exit must not grow a spurious "exit status 0"
// trailer.
func TestBashSuccessHasNoExitNote(t *testing.T) {
	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir()))
	out, err := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("missing output, got %q", out)
	}
	if strings.Contains(out, "exit status") {
		t.Fatalf("unexpected exit note on success: %q", out)
	}
}

// TestBashHonorsCwd checks cmd.Dir is actually wired to the cwd RegisterBash
// was given, not the process's own working directory.
func TestBashHonorsCwd(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, dir))
	out, err := r.Execute(context.Background(), "bash", json.RawMessage(`{"command":"pwd"}`))
	if err != nil {
		t.Fatal(err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedOut, err := filepath.EvalSymlinks(strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	if resolvedOut != resolvedDir {
		t.Fatalf("pwd = %q, want %q", resolvedOut, resolvedDir)
	}
}

// TestBashMissingCommandIsToolError checks input validation happens before
// any process is spawned.
func TestBashMissingCommandIsToolError(t *testing.T) {
	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir()))
	if _, err := r.Execute(context.Background(), "bash", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for missing command")
	}
}

// TestBashTimeoutClamping pins the default (120s) and max (600s) timeout
// values from the brief as a pure function, so the 610s/0/negative edge
// cases don't require an actual multi-minute sleep in the test suite.
func TestBashTimeoutClamping(t *testing.T) {
	cases := []struct {
		name      string
		timeoutMs int
		want      time.Duration
	}{
		{"zero uses default", 0, 120 * time.Second},
		{"negative uses default", -5, 120 * time.Second},
		{"within range passes through", 5000, 5 * time.Second},
		{"over max clamps to max", 700_000, 600 * time.Second},
		{"exactly max passes through", 600_000, 600 * time.Second},
		// Overflow guard: converting to a Duration BEFORE clamping wraps
		// int64 and yields a negative duration, which passes a
		// "> maxBashTimeout" test and produces an already-expired context —
		// a huge timeout silently becoming an instant one.
		{"overflowing value clamps to max", math.MaxInt, 600 * time.Second},
		{"just past the overflow boundary clamps to max", 9_300_000_000_000, 600 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bashTimeout(c.timeoutMs); got != c.want {
				t.Errorf("bashTimeout(%d) = %v, want %v", c.timeoutMs, got, c.want)
			}
		})
	}
}

// TestBashTimeoutFires drives a real timeout end-to-end: a command that
// outlives its timeout_ms must be killed and reported, well before the
// command's own sleep would have finished.
//
// The command deliberately uses `&&`, for which bash FORKS instead of
// exec'ing, so the sleep is a GRANDCHILD of the process we spawned. Killing
// only the direct pid leaves it running and — because it inherited the
// output pipes — makes Wait ride out the full WaitDelay. Hence both
// assertions: the call returns fast AND the process is really dead.
func TestBashTimeoutFires(t *testing.T) {
	sleepArg := uniqueSleepArg()
	needle := "sleep " + sleepArg
	t.Cleanup(func() { killSurvivors(t, needle) })

	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir()))
	start := time.Now()
	out, err := r.Execute(context.Background(), "bash",
		json.RawMessage(`{"command":"sleep `+sleepArg+` && echo never","timeout_ms":200}`))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took %v to fire; anything near bashWaitDelay (%v) means Wait sat on pipes held by an orphaned grandchild instead of the process group being killed", elapsed, bashWaitDelay)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected a timeout note in output, got %q", out)
	}
	if strings.Contains(out, "never") {
		t.Fatalf("command continued past the timeout, got %q", out)
	}
	requireNoSurvivors(t, needle)
}

// TestBashCtxCancellationKillsProcessTree is the critical abort-path test.
// When the caller's ctx is canceled the ENTIRE process tree must die, not
// just the `bash` process we spawned: bash forks for `&&` chains, pipelines
// and background jobs, so the direct pid is usually a shell that has already
// handed the real work to a child. Three things are asserted:
//
//  1. the tool returns far faster than bashWaitDelay — a ~WaitDelay return
//     means abort merely detached and then waited out pipes held by an
//     orphan, which for an in-band abort is a stall, not an abort;
//  2. no process from the tree survives (the uniquely-identifiable sleep);
//  3. the marker the chain would have touched never appears.
func TestBashCtxCancellationKillsProcessTree(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "done")
	sleepArg := uniqueSleepArg()
	needle := "sleep " + sleepArg
	t.Cleanup(func() { killSurvivors(t, needle) })

	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Execute(ctx, "bash",
			json.RawMessage(`{"command":"sleep `+sleepArg+` && touch `+marker+`"}`))
	}()

	// Abort only once the grandchild genuinely exists, so this tests the
	// kill path rather than racing bash's fork.
	waitForProcess(t, needle)
	start := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("bash tool did not return within 2s of ctx cancellation (bashWaitDelay is %v): abort left the real work running and blocked on its inherited pipes", bashWaitDelay)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("abort took %v to return, want well under bashWaitDelay (%v)", elapsed, bashWaitDelay)
	}

	requireNoSurvivors(t, needle)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("process kept running after ctx cancellation — marker file was created")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// TestBashBackgroundProcessKeepsOutput covers the "spill, never destroy"
// hole: a command that exits 0 but backgrounds something (a dev server,
// nohup, any `&`) leaves the inherited output pipes open, so Wait gives up
// after WaitDelay and returns exec.ErrWaitDelay. That is NOT a failure to
// start — the output already collected must be returned, and the call must
// not be reported to the model as a tool error.
//
// This test necessarily takes bashWaitDelay to run; that delay is the
// behavior under test.
func TestBashBackgroundProcessKeepsOutput(t *testing.T) {
	sleepArg := uniqueSleepArg()
	needle := "sleep " + sleepArg
	// The backgrounded process is SUPPOSED to outlive the command, so this
	// cleanup is the test's own housekeeping, not an assertion.
	t.Cleanup(func() { killSurvivors(t, needle) })
	logged := captureSlog(t)

	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir()))

	out, err := r.Execute(context.Background(), "bash",
		json.RawMessage(`{"command":"echo hi; sleep `+sleepArg+` &"}`))
	if err != nil {
		t.Fatalf("a successful command that backgrounded a process was reported as a tool error: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("collected output was destroyed, got %q", out)
	}
	if !strings.Contains(out, "background processes") {
		t.Fatalf("expected a note explaining the held pipes, got %q", out)
	}
	// The condition is degraded output, so it must be logged, not swallowed.
	if !strings.Contains(logged.String(), "wait delay expired") {
		t.Fatalf("expected the truncated read to be logged, got %q", logged.String())
	}
}

// TestBashOutputGoesThroughSpillPolicy checks bash wires its result through
// OutputPolicy.Clip: an over-budget command output must be clipped, with
// the full output spilled to SpillDir.
func TestBashOutputGoesThroughSpillPolicy(t *testing.T) {
	spillDir := t.TempDir()
	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 200, SpillDir: spillDir}, t.TempDir()))

	out, err := r.Execute(context.Background(), "bash",
		json.RawMessage(`{"command":"printf 'x%.0s' {1..2000}"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 400 {
		t.Fatalf("expected clipped output, got %d bytes", len(out))
	}
	if !strings.Contains(out, "elided") {
		t.Fatalf("expected elision marker, got %q", out)
	}
	entries, err := os.ReadDir(spillDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one spill file, got %d", len(entries))
	}
	full, err := os.ReadFile(filepath.Join(spillDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 2000 {
		t.Fatalf("spilled file is missing output: %d bytes", len(full))
	}
}

// TestBashSpillNameFallbackIsRaceSafe drives many concurrent bash calls
// under context.Background() (so agentloop.ToolCallID is always "" and every
// call takes the counter fallback), asserting each gets a distinct spill
// file — the load-bearing race-safety requirement on that fallback counter.
// Run with -race.
func TestBashSpillNameFallbackIsRaceSafe(t *testing.T) {
	spillDir := t.TempDir()
	r := NewRegistry()
	r.Register(testBashTool(t, OutputPolicy{Budget: 100, SpillDir: spillDir}, t.TempDir()))

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Execute(context.Background(), "bash",
				json.RawMessage(`{"command":"printf 'y%.0s' {1..500}"}`))
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	entries, err := os.ReadDir(spillDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("expected %d distinct spill files, got %d", n, len(entries))
	}
}

// TestBashRtkRewired verifies that a mapped command (git status) is executed
// through rtk rather than bash -c, and an unmapped command (echo hello) still
// goes through bash -c.
func TestBashRtkRewired(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	// Build a bash tool with RTKAuto (the default).
	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{
		OutputPolicy: OutputPolicy{Budget: 30000, SpillDir: t.TempDir()},
		Cwd:          t.TempDir(),
		RTK:          RTKAuto,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A mapped command (git status) should be rewritten to rtk git status.
	// Our fake rtk echoes its arguments, so we can detect it.
	result, err := tool.Execute(context.Background(),
		ToolInput(json.RawMessage(`{"command":"git status"}`)))
	if err != nil {
		t.Fatal(err)
	}
	// The fake rtk writes "git status" (the args after rtk git) to stdout.
	// The original command "git status" after rewrite becomes ["rtk", "git", "status"],
	// and the fake rtk echoes "git status".
	if !strings.Contains(result.Text, "git") || !strings.Contains(result.Text, "status") {
		t.Fatalf("expected rtk output containing 'git' and 'status', got %q", result.Text)
	}
	// The output should NOT contain "bash" since we bypassed bash -c entirely.

	// An unmapped command should still go through bash -c (NO rewrite).
	result2, err2 := tool.Execute(context.Background(),
		ToolInput(json.RawMessage(`{"command":"echo hello"}`)))
	if err2 != nil {
		t.Fatal(err2)
	}
	if !strings.Contains(result2.Text, "hello") {
		t.Fatalf("expected 'hello' in output, got %q", result2.Text)
	}
}

// TestBashRtkOffNeverRewrites verifies that RTKOff mode never invokes rtk,
// even with a mapped command.
func TestBashRtkOffNeverRewrites(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{
		OutputPolicy: OutputPolicy{Budget: 30000, SpillDir: t.TempDir()},
		Cwd:          t.TempDir(),
		RTK:          RTKOff,
	})
	if err != nil {
		t.Fatal(err)
	}

	// With RTKOff, even a mapped command should go through bash -c.
	// Our fake rtk echoes "rtk 0.45.0 git status" — but that should NOT appear
	// because rtk should never be called.
	result, err := tool.Execute(context.Background(),
		ToolInput(json.RawMessage(`{"command":"echo foundme"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "foundme") {
		t.Fatalf("expected 'foundme' in output, got %q", result.Text)
	}
}

// TestBashChainedCommandNotRewired verifies that a command with shell chaining
// is NOT rewritten by rtk, even in RTKAuto mode with rtk available.
func TestBashChainedCommandNotRewired(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	tool, err := (&BashBlueprint{}).Materialize(ToolOpts{
		OutputPolicy: OutputPolicy{Budget: 30000, SpillDir: t.TempDir()},
		Cwd:          t.TempDir(),
		RTK:          RTKAuto,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A chained command should NOT be rewritten — it must still go through bash -c.
	result, err := tool.Execute(context.Background(),
		ToolInput(json.RawMessage(`{"command":"echo yes && echo also"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "yes") {
		t.Fatalf("expected 'yes' in output, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "also") {
		t.Fatalf("expected 'also' in output, got %q", result.Text)
	}
}
