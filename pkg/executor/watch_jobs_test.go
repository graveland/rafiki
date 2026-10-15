package executor_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"

	"github.com/multigres/testkit/assert"
)

// startBackgroundJob runs a background bash job through the client and returns
// its handle.
func startBackgroundJob(t *testing.T, client executorpbconnect.ExecutorServiceClient, callID, command, owner string) string {
	t.Helper()
	c := assert.NewCollecting(t)
	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		CallId: callID, Tool: "bash", Background: true, Owner: owner,
		InputJson: []byte(`{"command":` + strconv.Quote(command) + `}`),
	}))
	c.Require().NoError(err, "Execute background")
	handle := ""
	for stream.Receive() {
		if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
			handle = ev.Handle
			break
		}
	}
	c.Require().NoError(stream.Err(), "Execute stream")
	c.Require().NotEq("", handle, "a background job yielded no handle")
	return handle
}

// waitJobExited polls JobOutput until the job reports exited.
func waitJobExited(t *testing.T, client executorpbconnect.ExecutorServiceClient, handle string, within time.Duration) {
	t.Helper()
	c := assert.NewCollecting(t)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := client.JobOutput(context.Background(), connect.NewRequest(&executorpb.JobOutputRequest{Handle: handle}))
		c.Require().NoError(err, "JobOutput")
		if resp.Msg.Exited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never exited within %s", handle, within)
}

// drainReplay reads a WatchJobs stream until ReplayComplete.
func drainReplay(t *testing.T, stream *connect.ServerStreamForClient[executorpb.WatchJobsResponse]) {
	t.Helper()
	for stream.Receive() {
		if _, ok := stream.Msg().Event.(*executorpb.WatchJobsResponse_ReplayComplete); ok {
			return
		}
	}
	t.Fatalf("WatchJobs stream ended before ReplayComplete: %v", stream.Err())
}

// TestWatchJobsReplaysRunningAndUnackedThenCompletes: on subscribe the executor
// replays a running job and a finished UNACKED job, skips a finished ACKED one,
// and ends the replay with ReplayComplete.
func TestWatchJobsReplaysRunningAndUnackedThenCompletes(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(), Concurrency: 6, Version: "test", SpillDir: t.TempDir(),
	})
	client := newTestClient(t, srv)
	ctx := context.Background()

	running := startBackgroundJob(t, client, "h-run", "sleep 30", "c_a")
	unacked := startBackgroundJob(t, client, "h-unacked", "echo done", "c_b")
	acked := startBackgroundJob(t, client, "h-acked", "echo done", "c_c")
	waitJobExited(t, client, unacked, 15*time.Second)
	waitJobExited(t, client, acked, 15*time.Second)
	_, err := client.AckJobExits(ctx, connect.NewRequest(&executorpb.AckJobExitsRequest{Handles: []string{acked}}))
	c.Require().NoError(err, "AckJobExits")

	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := client.WatchJobs(wctx, connect.NewRequest(&executorpb.WatchJobsRequest{}))
	c.Require().NoError(err, "WatchJobs")

	gotRunning := map[string]bool{}
	gotExited := map[string]bool{}
	replayDone := false
	for !replayDone && stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.WatchJobsResponse_Running:
			gotRunning[ev.Running.Handle] = true
		case *executorpb.WatchJobsResponse_Exited:
			gotExited[ev.Exited.Handle] = true
		case *executorpb.WatchJobsResponse_ReplayComplete:
			replayDone = true
		}
	}
	_ = stream.Err()
	c.True(replayDone, "the replay did not end with ReplayComplete")
	c.True(gotRunning[running], "the running job was not replayed")
	c.True(gotExited[unacked], "the unacked finished job was not replayed")
	c.False(gotExited[acked], "the acked finished job was replayed")
}

// TestWatchJobsStreamsLiveExit: after the replay, a start streams a running
// event and its exit streams live.
func TestWatchJobsStreamsLiveExit(t *testing.T) {
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

	handle := startBackgroundJob(t, client, "h-live", "echo hi", "c_x")

	sawRunning, sawExited := false, false
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.WatchJobsResponse_Running:
			if ev.Running.Handle == handle {
				sawRunning = true
			}
		case *executorpb.WatchJobsResponse_Exited:
			if ev.Exited.Handle == handle {
				sawExited = true
			}
		}
		if sawExited {
			break
		}
	}
	_ = stream.Err()
	c.True(sawRunning, "no live running event for a started job")
	c.True(sawExited, "no live exited event for a finished job")
}

// TestAckJobExitsStopsReplay: an exit replayed on subscribe stops replaying
// once acked.
func TestAckJobExitsStopsReplay(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{
		Root: t.TempDir(), Concurrency: 6, Version: "test", SpillDir: t.TempDir(),
	})
	client := newTestClient(t, srv)
	ctx := context.Background()

	handle := startBackgroundJob(t, client, "h-ack", "echo done", "c_x")
	waitJobExited(t, client, handle, 15*time.Second)

	replayedExited := func() bool {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		stream, err := client.WatchJobs(wctx, connect.NewRequest(&executorpb.WatchJobsRequest{}))
		c.Require().NoError(err, "WatchJobs")
		for stream.Receive() {
			switch ev := stream.Msg().Event.(type) {
			case *executorpb.WatchJobsResponse_Exited:
				if ev.Exited.Handle == handle {
					return true
				}
			case *executorpb.WatchJobsResponse_ReplayComplete:
				return false
			}
		}
		t.Fatalf("WatchJobs ended before ReplayComplete: %v", stream.Err())
		return false
	}

	c.True(replayedExited(), "an unacked exit was not replayed on subscribe")
	_, err := client.AckJobExits(ctx, connect.NewRequest(&executorpb.AckJobExitsRequest{Handles: []string{handle}}))
	c.Require().NoError(err, "AckJobExits")
	c.False(replayedExited(), "an acked exit was still replayed")
}

// TestWatchJobsCarriesOwner: the owner put on an ExecuteRequest rides both the
// running and the exited event.
func TestWatchJobsCarriesOwner(t *testing.T) {
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

	handle := startBackgroundJob(t, client, "h-owner", "echo hi", "c_x")

	runningOwner, exitedOwner := "", ""
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.WatchJobsResponse_Running:
			if ev.Running.Handle == handle {
				runningOwner = ev.Running.Owner
			}
		case *executorpb.WatchJobsResponse_Exited:
			if ev.Exited.Handle == handle {
				exitedOwner = ev.Exited.Owner
			}
		}
		if exitedOwner != "" {
			break
		}
	}
	_ = stream.Err()
	c.Eq("c_x", runningOwner, "running event owner")
	c.Eq("c_x", exitedOwner, "exited event owner")
}
