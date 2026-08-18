package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForExit polls a job until it reports exited, or fails.
func waitForExit(t *testing.T, r *jobRegistry, handle string, within time.Duration) (exitCode int) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		_, _, exited, code, found := r.output(handle, 0)
		if !found {
			t.Fatalf("job %s vanished from the registry while waiting for it to exit", handle)
		}
		if exited {
			return code
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never reported exited within %s", handle, within)
	return 0
}

// A job whose command leaves a grandchild holding the output pipe must still
// report exited.
//
// exec.Cmd.Wait waits for the process AND for the output pipes to reach EOF. A
// backgrounded grandchild inherits those pipes and holds them open for as long
// as it runs, so without a WaitDelay the wait NEVER returns: the job reports
// "still running" forever, Health counts it forever, the retention sweep is
// never scheduled, and the goroutine leaks. `npm run dev` is the case the job
// registry's own comments cite, and it is exactly this shape.
func TestAJobLeavingAGrandchildOnThePipeStillReportsExited(t *testing.T) {
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	handle, err := r.start("sleep 300 & echo started; exit 0", "h1", "ws-1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if code := waitForExit(t, r, handle, 15*time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// ...and its exit code must be the one the process actually returned.
//
// When WaitDelay expires, Wait returns exec.ErrWaitDelay rather than an
// *exec.ExitError, so unwrapping the error records -1 for a job that SUCCEEDED.
// ProcessState carries the real code in every case.
func TestALingeringGrandchildDoesNotCorruptTheExitCode(t *testing.T) {
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	for _, tc := range []struct {
		name   string
		script string
		want   int
	}{
		{"success with a lingering grandchild", "sleep 300 & echo hi; exit 0", 0},
		{"failure with a lingering grandchild", "sleep 300 & echo hi; exit 7", 7},
		{"clean success", "echo hi; exit 0", 0},
		{"clean failure", "echo hi; exit 3", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle, err := r.start(tc.script, "", "ws-1")
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if code := waitForExit(t, r, handle, 15*time.Second); code != tc.want {
				t.Errorf("exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

// Output beyond what a read returns is NOT destroyed: it is on disk, and the
// reader is told where.
//
// "Spill, never destroy" is the rule every other tool result follows
// (OutputPolicy.Clip). The background path used to be the one place it did not:
// a 100 KB in-memory ring dropped the oldest bytes permanently, so a build that
// printed 5 MB and failed early lost the failure.
func TestOutputBeyondTheReadCapIsStillOnDisk(t *testing.T) {
	dir := t.TempDir()
	r := newJobRegistry(dir, dir, defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	// One distinctive early line, then enough noise to push it past the cap.
	script := fmt.Sprintf("echo NEEDLE_AT_THE_START; for i in $(seq 1 %d); do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; done", (maxJobResponse/49)+200)
	handle, err := r.start(script, "", "ws-1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForExit(t, r, handle, 20*time.Second)

	data, total, _, _, _ := r.output(handle, 0)
	if int64(len(data)) > maxJobResponse {
		t.Errorf("a single read returned %d bytes; the cap is %d", len(data), maxJobResponse)
	}
	if strings.Contains(string(data), "NEEDLE_AT_THE_START") {
		t.Fatal("the fixture did not exceed the read cap; the test proves nothing")
	}
	if total <= maxJobResponse {
		t.Fatalf("total = %d, expected more than the read cap", total)
	}

	// The dropped bytes must be recoverable, and the reader must be told where.
	path := r.outputPath(handle)
	if path == "" {
		t.Fatal("no spill path for a job whose output was clipped")
	}
	if !strings.Contains(string(data), filepath.Base(path)) {
		t.Errorf("the clipped read does not name the file holding the rest:\n%s", string(data)[:200])
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the spill file: %v", err)
	}
	if !strings.Contains(string(full), "NEEDLE_AT_THE_START") {
		t.Error("the early output was destroyed rather than spilled")
	}
}

// Retention is bounded by BYTES per workspace and by nothing else — no timers.
//
// A wall-clock retention window is meaningless for an async agent: a turn can
// end and resume hours later, so a job that finished 30 seconds in would be
// unreadable by the time anyone came back for it. Finished jobs are evicted
// oldest-first only when the workspace exceeds its byte budget.
func TestFinishedJobsAreEvictedByByteBudgetOldestFirst(t *testing.T) {
	const budget = 64 << 10
	r := newJobRegistry(t.TempDir(), t.TempDir(), budget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	var handles []string
	for i := range 8 {
		h, err := r.start(fmt.Sprintf("for i in $(seq 1 400); do echo job%d-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; done", i), "", "ws-1")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		waitForExit(t, r, h, 15*time.Second)
		handles = append(handles, h)
	}

	if _, _, _, _, found := r.output(handles[len(handles)-1], 0); !found {
		t.Error("the most recent finished job was evicted; eviction must drop the OLDEST first")
	}
	if _, _, _, _, found := r.output(handles[0], 0); found {
		t.Error("the oldest finished job survived a budget that cannot hold every job")
	}
	if got := r.workspaceBytes("ws-1"); got > budget {
		t.Errorf("workspace holds %d bytes, over its %d budget", got, budget)
	}
}

// A running job is never evicted: its output is live, and dropping it would
// lose the stream rather than an archive.
func TestARunningJobIsNeverEvicted(t *testing.T) {
	const budget = 32 << 10
	r := newJobRegistry(t.TempDir(), t.TempDir(), budget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	live, err := r.start("echo live-marker; sleep 300", "", "ws-1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for range 6 {
		h, err := r.start("for i in $(seq 1 400); do echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb; done", "", "ws-1")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		waitForExit(t, r, h, 15*time.Second)
	}

	data, _, exited, _, found := r.output(live, 0)
	if !found {
		t.Fatal("the running job was evicted to make room for finished ones")
	}
	if exited {
		t.Fatal("fixture: the live job exited early")
	}
	if !strings.Contains(string(data), "live-marker") {
		t.Error("the running job's output was discarded")
	}
}

// Releasing a workspace takes its jobs AND their files with it — that is the
// only lifecycle event that ends retention.
func TestReleasingAWorkspaceRemovesItsJobsAndFiles(t *testing.T) {
	dir := t.TempDir()
	r := newJobRegistry(dir, dir, defaultJobBudget)

	h, err := r.start("echo done", "", "ws-1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForExit(t, r, h, 15*time.Second)
	path := r.outputPath(h)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill file missing before release: %v", err)
	}

	r.releaseWorkspace("ws-1")

	if _, _, _, _, found := r.output(h, 0); found {
		t.Error("a released workspace's job is still readable")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the spill file outlived its workspace: %v", err)
	}
}

// Releasing one workspace must not touch another's jobs. This was finding D4,
// caught once and easy to reintroduce while rewriting the sweep.
func TestReleasingOneWorkspaceLeavesAnothersJobsAlone(t *testing.T) {
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-2") })

	a, err := r.start("echo a", "", "ws-1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	b, err := r.start("echo b", "", "ws-2")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForExit(t, r, a, 15*time.Second)
	waitForExit(t, r, b, 15*time.Second)

	r.releaseWorkspace("ws-1")

	if _, _, _, _, found := r.output(b, 0); !found {
		t.Fatal("releasing ws-1 destroyed ws-2's job")
	}
}
