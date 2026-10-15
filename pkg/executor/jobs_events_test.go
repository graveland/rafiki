package executor

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// recvEvent reads one event from ch, failing if none arrives within within.
func recvEvent(t *testing.T, ch <-chan jobEvent, within time.Duration) jobEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("event channel closed while waiting for an event")
		}
		return ev
	case <-time.After(within):
		t.Fatalf("no event within %s", within)
		return jobEvent{}
	}
}

// assertNoEvent fails if any event arrives within within.
func assertNoEvent(t *testing.T, ch <-chan jobEvent, within time.Duration, msg string) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		t.Fatalf("%s: got event %+v (open=%v)", msg, ev, ok)
	case <-time.After(within):
	}
}

// A job's exit is published exactly once: a running event on start, an exited
// event on exit, nothing more.
func TestJobExitPublishesOnce(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	ch, cancel := r.subscribe()
	defer cancel()

	handle, err := r.start("true", "h1", "ws-1", "", "owner-1")
	c.NoError(err, "start")

	running := recvEvent(t, ch, 5*time.Second)
	c.True(running.running, "first event is not a running event: %+v", running)
	c.Eq(handle, running.handle, "running event handle")
	c.Eq("owner-1", running.owner, "running event owner")

	exited := recvEvent(t, ch, 15*time.Second)
	c.False(exited.running, "second event is not an exited event: %+v", exited)
	c.Eq(handle, exited.handle, "exited event handle")
	c.Eq(0, exited.exitCode, "exit code")

	assertNoEvent(t, ch, 200*time.Millisecond, "an exit must be published exactly once")
}

// A killed job publishes no exit: kill acks before signalling, so the exit it
// causes is suppressed.
func TestKilledJobPublishesNoExit(t *testing.T) {
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	ch, cancel := r.subscribe()
	defer cancel()

	handle, err := r.start("sleep 30", "h-kill", "ws-1", "", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	running := recvEvent(t, ch, 5*time.Second)
	if !running.running {
		t.Fatalf("expected a running event, got %+v", running)
	}

	r.mu.Lock()
	j := r.jobs[handle]
	r.mu.Unlock()
	if j == nil {
		t.Fatalf("job %s vanished", handle)
	}
	if err := r.kill(handle); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-j.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("killed job never exited")
	}

	assertNoEvent(t, ch, 200*time.Millisecond, "a killed job must publish no exit")
}

// An unacked finished job is never evicted; acked ones are, oldest first.
func TestUnackedFinishedJobSurvivesEviction(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	r := newJobRegistry(dir, dir, 1) // 1-byte budget forces immediate eviction pressure
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	startExit := func(cmd string) string {
		h, err := r.start(cmd, "", "ws-1", "", "")
		c.NoError(err, "start")
		waitForExit(t, r, h, 15*time.Second)
		return h
	}
	h1 := startExit("echo aaaaaaaaaa")
	h2 := startExit("echo bbbbbbbbbb")

	if _, _, _, _, found := r.output(h1, 0); !found {
		t.Fatal("an unacked finished job was evicted before any ack")
	}
	if _, _, _, _, found := r.output(h2, 0); !found {
		t.Fatal("an unacked finished job was evicted before any ack")
	}

	// Ack h1, then run another job to re-enforce the budget.
	r.ack([]string{h1})
	h3 := startExit("echo cccccccccc")

	if _, _, _, _, found := r.output(h1, 0); found {
		t.Error("an acked finished job survived eviction under a 1-byte budget")
	}
	if _, _, _, _, found := r.output(h2, 0); !found {
		t.Error("an UNACKED finished job was evicted")
	}
	_, _, _, _, _ = r.output(h3, 0)
}

// A finished job's file descriptor is closed at exit — only the bytes stay.
func TestFinishedJobClosesItsFile(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	handle, err := r.start("echo hello", "h1", "ws-1", "", "")
	c.NoError(err, "start")
	waitForExit(t, r, handle, 15*time.Second)

	r.mu.Lock()
	j := r.jobs[handle]
	r.mu.Unlock()
	c.NotNil(j, "job vanished before the assertions")
	j.out.mu.Lock()
	f := j.out.f
	j.out.mu.Unlock()
	c.Nil(f, "the output file descriptor was not closed at exit")

	data, _, _, _, found := r.output(handle, 0)
	c.Require().True(found, "output after exit")
	c.StrContains(string(data), "hello", "output after closing the file = %q", data)

	// lastWriteAt is the timestamp the handoff's "last output N ago" line is
	// built from; a job that wrote output must report a non-zero one.
	c.False(j.out.lastWriteAt().IsZero(), "a job that wrote output reports a zero lastWriteAt")
}

// A long-lived executor with many small finished jobs must not hold a file
// descriptor for each.
func TestFinishedJobsDoNotLeakFds(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("fd counting is Linux/darwin only")
	}
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	countFds := func() int {
		entries, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Fatalf("read /dev/fd: %v", err)
		}
		return len(entries)
	}
	before := countFds()
	for i := range 100 {
		h, err := r.start("true", "", "ws-1", "", "")
		c.NoError(err, "start %d", i)
		waitForExit(t, r, h, 15*time.Second)
	}
	after := countFds()
	if after-before >= 10 {
		t.Errorf("100 finished jobs leaked %d file descriptors (before %d, after %d)", after-before, before, after)
	}
}

// A provisional job is invisible until promoted: its handle answers like an
// unknown one and it publishes no event until promote.
func TestProvisionalJobIsInvisibleUntilPromoted(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	ch, cancel := r.subscribe()
	defer cancel()

	j, err := r.startProvisional([]string{"bash", "-c", "echo hi; sleep 30"}, nil, "", "ws-1", "owner-1", "echo hi; sleep 30")
	c.NoError(err, "startProvisional")

	if _, _, _, _, found := r.output(j.handle, 0); found {
		t.Error("a provisional job is visible to output")
	}
	if p := r.outputPath(j.handle); p != "" {
		t.Errorf("a provisional job reports an output path %q", p)
	}
	for _, h := range r.running() {
		if h == j.handle {
			t.Error("a provisional job appears in running()")
		}
	}
	assertNoEvent(t, ch, 150*time.Millisecond, "a provisional job published an event before promote")

	handle := r.promote(j)
	c.Eq(j.handle, handle, "promote handle")

	running := recvEvent(t, ch, 5*time.Second)
	c.True(running.running, "promote must publish a running event: %+v", running)
	c.Eq(handle, running.handle, "running event handle")

	_, _, _, _, found := r.output(handle, 0)
	c.True(found, "a promoted job must be visible to output")
}

// Promoting a job that already exited publishes BOTH the running and the
// exited event, since the Wait goroutine could not publish the exit while the
// job was provisional.
func TestPromoteAfterExitPublishesBoth(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	j, err := r.startProvisional([]string{"true"}, nil, "", "ws-1", "owner-1", "true")
	c.NoError(err, "startProvisional")
	select {
	case <-j.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("provisional job never exited")
	}

	ch, cancel := r.subscribe()
	defer cancel()
	r.promote(j)

	first := recvEvent(t, ch, 5*time.Second)
	c.True(first.running, "first event is not running: %+v", first)
	second := recvEvent(t, ch, 5*time.Second)
	c.False(second.running, "second event is not exited: %+v", second)
	c.Eq(j.handle, second.handle, "exited event handle")
	assertNoEvent(t, ch, 200*time.Millisecond, "promote-after-exit published a third event")
}

// Discard removes a provisional job entirely: entry, file and any event.
func TestDiscardRemovesFileAndEntry(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	ch, cancel := r.subscribe()
	defer cancel()

	j, err := r.startProvisional([]string{"bash", "-c", "sleep 30"}, nil, "", "ws-1", "owner-1", "sleep 30")
	c.NoError(err, "startProvisional")
	path := j.out.path
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill file missing before discard: %v", err)
	}

	r.discard(j)

	r.mu.Lock()
	_, present := r.jobs[j.handle]
	r.mu.Unlock()
	c.False(present, "a discarded job is still in the registry")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a discarded job's spill file outlived it: %v", err)
	}
	if _, _, _, _, found := r.output(j.handle, 0); found {
		t.Error("a discarded job is still readable")
	}
	// Nothing was published: not a running event (provisional), and not the
	// exit the discard caused (it is acked).
	assertNoEvent(t, ch, 200*time.Millisecond, "a discarded job published an event")
}

// A subscriber that cannot keep up is ENDED (its channel closed), never
// silently skipped: its resubscribe replays everything unacked.
func TestSlowSubscriberIsEndedNotSkipped(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)

	ch, cancel := r.subscribe()
	defer cancel()

	// Fill the buffer, then one more to trip the overflow.
	for i := 0; i <= subscriberCapacity; i++ {
		r.publish(jobEvent{handle: fmt.Sprintf("h%d", i)})
	}

	n := 0
	for range ch {
		n++
	}
	c.Eq(subscriberCapacity, n, "events buffered before the subscriber was ended")
}

// A provisional job tees stderr into a head buffer that keeps the FIRST
// stderrHeadSize bytes and no more.
func TestProvisionalCapturesStderrHead(t *testing.T) {
	c := assert.NewAborting(t)
	r := newJobRegistry(t.TempDir(), t.TempDir(), defaultJobBudget)
	t.Cleanup(func() { r.releaseWorkspace("ws-1") })

	script := `echo on-stdout; for i in $(seq 1 2000); do printf 'abc' >&2; done`
	j, err := r.startProvisional([]string{"bash", "-c", script}, nil, "", "ws-1", "owner-1", script)
	c.NoError(err, "startProvisional")
	select {
	case <-j.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("provisional job never exited")
	}

	head := j.stderrHead.String()
	c.Eq(stderrHeadSize, len(head), "stderr head length")
	c.Eq(strings.Repeat("abc", stderrHeadSize/3), head[:stderrHeadSize-stderrHeadSize%3], "stderr head content")

	combined, err := j.out.all()
	c.NoError(err, "out.all")
	c.StrContains(string(combined), "on-stdout", "stdout was not captured into the output file")
}
