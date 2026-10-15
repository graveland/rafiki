package executor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// executeAutoBackground runs one NON-background bash call through the
// executor's root registry and returns its result text. autoBack is the
// per-call auto_background flag; owner is the child the call is on behalf of.
// A failure event is fatal.
func executeAutoBackground(t *testing.T, client executorpbconnect.ExecutorServiceClient, command string, timeoutMs int, autoBack bool, owner string) string {
	t.Helper()
	return executeAutoBackgroundWS(t, client, "", command, timeoutMs, autoBack, owner)
}

// executeAutoBackgroundWS is executeAutoBackground against a named workspace
// ("" = the executor's root registry).
func executeAutoBackgroundWS(t *testing.T, client executorpbconnect.ExecutorServiceClient, wsID, command string, timeoutMs int, autoBack bool, owner string) string {
	t.Helper()
	out, err := runAutoBackground(client, wsID, command, timeoutMs, autoBack, owner)
	assert.NewAborting(t).NoError(err, "bash")
	return out
}

// runAutoBackground runs one NON-background bash call and returns its result
// text, reporting failures as an error rather than through t so it is safe to
// call from a goroutine.
func runAutoBackground(client executorpbconnect.ExecutorServiceClient, wsID, command string, timeoutMs int, autoBack bool, owner string) (string, error) {
	input := `{"command":` + strconv.Quote(command) + `,"timeout_ms":` + strconv.Itoa(timeoutMs) + `}`
	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:           "bash",
		InputJson:      []byte(input),
		WorkspaceId:    wsID,
		AutoBackground: autoBack,
		Owner:          owner,
	}))
	if err != nil {
		return "", fmt.Errorf("Execute: %w", err)
	}
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			var sb strings.Builder
			for _, b := range ev.Result.Content {
				sb.WriteString(b.GetText())
			}
			return sb.String(), nil
		case *executorpb.ExecuteResponse_Failed:
			return "", fmt.Errorf("bash failed: %s", ev.Failed.Message)
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("bash stream: %w", err)
	}
	return "", errors.New("bash returned no result")
}

// jobHandleFrom extracts the job handle from a bash auto-background handoff.
func jobHandleFrom(t *testing.T, handoff string) string {
	t.Helper()
	m := jobHandleRe.FindStringSubmatch(handoff)
	if m == nil {
		t.Fatalf("the handoff named no job handle: %q", handoff)
	}
	return m[1]
}

// provisionWorkspace provisions a workspace on the executor and returns its id.
func provisionWorkspace(t *testing.T, client executorpbconnect.ExecutorServiceClient, childID string) string {
	t.Helper()
	c := assert.NewAborting(t)
	resp, err := client.Provision(context.Background(), connect.NewRequest(&executorpb.ProvisionRequest{ChildId: childID}))
	c.NoError(err, "Provision")
	c.NotEq("", resp.Msg.WorkspaceId, "provisioned workspace id")
	return resp.Msg.WorkspaceId
}

// waitForFile blocks until path exists, or fails.
func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared within %s", path, within)
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

// A workspace's auto-background job is scoped to that workspace: it carries the
// workspace's id, so Release ends it — the handle answers unknown and no exit
// event is ever published for it. This is the wiring that would silently leak a
// job (and its process) without the id minted before the registry is built.
func TestAutoBackgroundWorkspaceJobEndsOnRelease(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(), Concurrency: 6, Version: "test", SpillDir: t.TempDir(),
	})
	client := newTestClient(t, srv)
	ctx := context.Background()

	wsID := provisionWorkspace(t, client, "c_ws")

	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.WatchJobs(wctx, connect.NewRequest(&executorpb.WatchJobsRequest{}))
	c.Require().NoError(err, "WatchJobs")
	drainReplay(t, stream)

	// Collect events off the stream.
	type jobEv struct {
		handle string
		kind   string
	}
	events := make(chan jobEv, 16)
	go func() {
		for stream.Receive() {
			switch msg := stream.Msg().Event.(type) {
			case *executorpb.WatchJobsResponse_Running:
				events <- jobEv{handle: msg.Running.Handle, kind: "running"}
			case *executorpb.WatchJobsResponse_Exited:
				events <- jobEv{handle: msg.Exited.Handle, kind: "exited"}
			}
		}
		close(events)
	}()
	waitForEvent := func(match func(jobEv) bool, within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case e, ok := <-events:
				if !ok {
					return false
				}
				if match(e) {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}

	out := executeAutoBackgroundWS(t, client, wsID, "sleep 30; echo ws-done", 300, true, "c_ws")
	handle := jobHandleFrom(t, out)

	// The promote's Running event is expected and confirms the job became
	// visible; what must NEVER arrive is an exit.
	c.True(waitForEvent(func(e jobEv) bool { return e.handle == handle && e.kind == "running" }, 5*time.Second),
		"the promoted workspace job streamed no running event")

	resp, err := client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{Handle: handle}))
	c.Require().NoError(err, "JobOutput before Release")
	c.True(resp.Msg.Found, "the workspace's job must be found before Release")

	_, err = client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{WorkspaceId: wsID}))
	c.Require().NoError(err, "Release")

	resp, err = client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{Handle: handle}))
	c.Require().NoError(err, "JobOutput after Release")
	c.False(resp.Msg.Found, "a released workspace's job must answer unknown")

	sawExit := waitForEvent(func(e jobEv) bool { return e.handle == handle && e.kind == "exited" }, 500*time.Millisecond)
	c.False(sawExit, "a released job must publish no exit event")
}

// A root-registry auto-background job carries no workspace id, so releasing a
// DIFFERENT workspace must not touch it — only the workspace that owns a job
// ends it.
func TestAutoBackgroundRootJobSurvivesOtherWorkspaceRelease(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(), Concurrency: 6, Version: "test", SpillDir: t.TempDir(),
	})
	client := newTestClient(t, srv)
	ctx := context.Background()

	otherWS := provisionWorkspace(t, client, "c_other")

	// No workspace id: this job belongs to the executor's own root registry.
	out := executeAutoBackgroundWS(t, client, "", "sleep 30; echo root-done", 300, true, "c_root")
	handle := jobHandleFrom(t, out)

	_, err := client.Release(ctx, connect.NewRequest(&executorpb.ReleaseRequest{WorkspaceId: otherWS}))
	c.Require().NoError(err, "Release")

	resp, err := client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{Handle: handle}))
	c.Require().NoError(err, "JobOutput after another workspace's Release")
	c.True(resp.Msg.Found, "a root job must survive a different workspace's Release")

	// Stop the stray so the test does not leave a 30s process behind.
	_, err = client.Cancel(ctx, connect.NewRequest(&executorpb.CancelRequest{CallId: handle}))
	c.Require().NoError(err, "Cancel")
}

// A provisional job is running but not yet a job, so Health must not list it as
// a running handle — it would otherwise be reported as an orphan for the whole
// grace window of every short call.
func TestAutoBackgroundProvisionalInvisibleInHealth(t *testing.T) {
	c := assert.NewCollecting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{
		Root: root, Concurrency: 6, Version: "test", SpillDir: t.TempDir(),
	})
	client := newTestClient(t, srv)
	ctx := context.Background()

	started := filepath.Join(root, "started")
	type runResult struct {
		out string
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		out, err := runAutoBackground(client, "", fmt.Sprintf("touch %q; sleep 3; echo hi", started), 1500, true, "c_x")
		done <- runResult{out: out, err: err}
	}()

	// The marker proves the provisional process is alive and inside its
	// timeout, so the Health read below is genuinely mid-flight.
	waitForFile(t, started, 5*time.Second)
	resp, err := client.Health(ctx, connect.NewRequest(&executorpb.HealthRequest{}))
	c.Require().NoError(err, "Health")
	c.Empty(resp.Msg.RunningHandles, "a provisional job must not be listed as running")

	res := <-done
	c.Require().NoError(res.err, "bash")
	handle := jobHandleFrom(t, res.out)

	// Stop the still-running job now that it has been promoted.
	_, err = client.Cancel(ctx, connect.NewRequest(&executorpb.CancelRequest{CallId: handle}))
	c.Require().NoError(err, "Cancel")
}
