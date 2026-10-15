package executor_test

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"

	"github.com/multigres/testkit/assert"
)

// jobHandleRe extracts the job handle from a bash auto-background handoff:
// "[still running after 300ms; moved to the background as job <handle>]".
var jobHandleRe = regexp.MustCompile(`moved to the background as job ([0-9a-f]+)\]`)

// executeAutoBackground runs one NON-background bash call through the executor
// and returns its result text. autoBack is the per-call auto_background flag;
// owner is the child the call is on behalf of. A failure event is fatal.
func executeAutoBackground(t *testing.T, client executorpbconnect.ExecutorServiceClient, command string, timeoutMs int, autoBack bool, owner string) string {
	t.Helper()
	c := assert.NewAborting(t)
	input := `{"command":` + strconv.Quote(command) + `,"timeout_ms":` + strconv.Itoa(timeoutMs) + `}`
	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:           "bash",
		InputJson:      []byte(input),
		AutoBackground: autoBack,
		Owner:          owner,
	}))
	c.NoError(err, "Execute")
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			var sb strings.Builder
			for _, b := range ev.Result.Content {
				sb.WriteString(b.GetText())
			}
			return sb.String()
		case *executorpb.ExecuteResponse_Failed:
			t.Fatalf("bash failed: %s", ev.Failed.Message)
		}
	}
	t.Fatalf("bash returned no result: %v", stream.Err())
	return ""
}

// noJobLogs asserts the spill dir holds no job output file, so an
// auto-backgrounded call that never became a job left no trace on disk.
func noJobLogs(t *testing.T, spillDir string) {
	t.Helper()
	c := assert.NewCollecting(t)
	entries, err := os.ReadDir(spillDir)
	c.Require().NoError(err, "read spill dir")
	for _, e := range entries {
		c.False(strings.HasPrefix(e.Name(), "job-"),
			"an auto-backgrounded call that never became a job left a job log: %s", e.Name())
	}
}

// A command that finishes under its timeout is an ordinary result: no job is
// created, no event is published, and no job output file is left behind. This
// is the whole point of the provisional-then-promote design — the common case
// costs exactly what it always did.
func TestAutoBackgroundShortCommandLeavesNoJob(t *testing.T) {
	c := assert.NewCollecting(t)
	spill := t.TempDir()
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(), Concurrency: 6, Version: "test", SpillDir: spill,
	})
	client := newTestClient(t, srv)

	// Subscribe BEFORE the call: if the short command wrongly created a job,
	// it would appear on the live channel.
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.WatchJobs(wctx, connect.NewRequest(&executorpb.WatchJobsRequest{}))
	c.Require().NoError(err, "WatchJobs")
	drainReplay(t, stream)

	out := executeAutoBackground(t, client, "echo hi", 5000, true, "c_x")
	c.Eq("hi\n", out, "a short command's result must be the ordinary one")

	gotEvent := make(chan struct{}, 1)
	go func() {
		if stream.Receive() {
			gotEvent <- struct{}{}
		}
	}()
	select {
	case <-gotEvent:
		t.Fatal("an auto-backgrounded short command published a job event")
	case <-time.After(500 * time.Millisecond):
	}

	noJobLogs(t, spill)
}

// A command still running at its timeout is promoted to a visible job: the
// caller gets a handoff naming the handle, a WatchJobs subscriber sees the
// exit with the owner set, and the output stays readable.
func TestAutoBackgroundLongCommandBecomesAJob(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(), Concurrency: 6, Version: "test", SpillDir: t.TempDir(),
	})
	client := newTestClient(t, srv)

	wctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := client.WatchJobs(wctx, connect.NewRequest(&executorpb.WatchJobsRequest{}))
	c.Require().NoError(err, "WatchJobs")
	drainReplay(t, stream)

	out := executeAutoBackground(t, client, "sleep 2; echo done", 300, true, "c_owner")
	m := jobHandleRe.FindStringSubmatch(out)
	c.Require().True(m != nil, "the handoff named no job handle: %q", out)
	handle := m[1]

	// Collect events off the stream, then wait for this handle's exit.
	type ev struct {
		handle string
		owner  string
		exited bool
	}
	events := make(chan ev, 16)
	go func() {
		for stream.Receive() {
			switch msg := stream.Msg().Event.(type) {
			case *executorpb.WatchJobsResponse_Running:
				events <- ev{handle: msg.Running.Handle, owner: msg.Running.Owner}
			case *executorpb.WatchJobsResponse_Exited:
				events <- ev{handle: msg.Exited.Handle, owner: msg.Exited.Owner, exited: true}
			}
		}
		close(events)
	}()

	sawRunning, gotOwner := false, ""
	waitForExit := func() bool {
		deadline := time.After(5 * time.Second)
		for {
			select {
			case e, ok := <-events:
				if !ok {
					return false
				}
				if e.handle != handle {
					continue
				}
				if e.exited {
					gotOwner = e.owner
				} else {
					sawRunning = true
				}
				if gotOwner != "" {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	c.True(waitForExit(), "the promoted job's exit event did not arrive within 5s")
	c.True(sawRunning, "the promoted job streamed no running event")
	c.Eq("c_owner", gotOwner, "the exit event must carry the owner")

	resp, err := client.JobOutput(context.Background(), connect.NewRequest(&executorpb.JobOutputRequest{Handle: handle}))
	c.Require().NoError(err, "JobOutput")
	c.True(resp.Msg.Found, "the promoted job's handle must be found")
	c.True(resp.Msg.Exited, "the promoted job must report exited")
	c.StrContains(string(resp.Msg.Data), "done", "the promoted job's output must be readable after promote")
}

// With the per-call flag off, a command still running at its timeout is killed
// and folded into the result exactly as it always was — the flag is the only
// thing that turns on the job path.
func TestAutoBackgroundFlagOffKeepsKillAtTimeout(t *testing.T) {
	c := assert.NewCollecting(t)
	spill := t.TempDir()
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(), Concurrency: 6, Version: "test", SpillDir: spill,
	})
	client := newTestClient(t, srv)

	out := executeAutoBackground(t, client, "sleep 2; echo done", 300, false, "c_x")
	c.StrContains(out, "command timed out", "the flag-off path must still kill at timeout")
	c.NotStrContains(out, "moved to the background", "the flag-off path must not background the command")
	noJobLogs(t, spill)
}
