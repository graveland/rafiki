package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBashMergesStderrAndReportsExit is the brief's Step 1 test: stdout and
// stderr land in one merged result, and a non-zero exit is a RESULT (err ==
// nil from Execute), not a tool error — the model sees it in the text.
func TestBashMergesStderrAndReportsExit(t *testing.T) {
	r := NewRegistry()
	RegisterBash(r, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir())
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
	RegisterBash(r, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir())
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
	RegisterBash(r, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, dir)
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
	RegisterBash(r, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir())
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
func TestBashTimeoutFires(t *testing.T) {
	r := NewRegistry()
	RegisterBash(r, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir())
	start := time.Now()
	out, err := r.Execute(context.Background(), "bash",
		json.RawMessage(`{"command":"sleep 5","timeout_ms":200}`))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took too long to fire: %v", elapsed)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected a timeout note in output, got %q", out)
	}
}

// TestBashCtxCancellationKillsProcess is the critical abort-path test: when
// the caller's ctx is canceled, the underlying process must actually die,
// not just have Execute return early and leave it running. Proven by racing
// a "sleep then touch a marker" command against cancellation and confirming
// the marker never appears.
func TestBashCtxCancellationKillsProcess(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "done")
	r := NewRegistry()
	RegisterBash(r, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Execute(ctx, "bash",
			json.RawMessage(`{"command":"sleep 2 && touch `+marker+`"}`))
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bash tool did not return promptly after ctx cancellation")
	}

	// Wait past when "sleep 2" would have completed if it had NOT been
	// killed, then confirm the marker it would have left behind is absent.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("process kept running after ctx cancellation — marker file was created")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// TestBashOutputGoesThroughSpillPolicy checks bash wires its result through
// OutputPolicy.Clip: an over-budget command output must be clipped, with
// the full output spilled to SpillDir.
func TestBashOutputGoesThroughSpillPolicy(t *testing.T) {
	spillDir := t.TempDir()
	r := NewRegistry()
	RegisterBash(r, OutputPolicy{Budget: 200, SpillDir: spillDir}, t.TempDir())

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
	RegisterBash(r, OutputPolicy{Budget: 100, SpillDir: spillDir}, t.TempDir())

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
