package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"

	"github.com/multigres/testkit/assert"
)

// RunConformance is the contract every executor implementation must satisfy,
// run against a real client over a real socket.
func RunConformance(t *testing.T, client executorpbconnect.ExecutorServiceClient, root string) {
	t.Helper()
	ctx := context.Background()

	call := func(t *testing.T, tool string, input string) (*executorpb.Result, *executorpb.Failure) {
		t.Helper()
		c := assert.NewAborting(t)
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "call-1", Tool: tool, InputJson: []byte(input), Timeout: durationpb.New(30 * time.Second),
		}))
		c.NoError(err, "Execute(%s)", tool)
		var result *executorpb.Result
		var failure *executorpb.Failure
		for stream.Receive() {
			switch ev := stream.Msg().Event.(type) {
			case *executorpb.ExecuteResponse_Result:
				result = ev.Result
			case *executorpb.ExecuteResponse_Failed:
				failure = ev.Failed
			}
		}
		c.NoError(stream.Err(), "stream error")
		return result, failure
	}

	t.Run("read returns file contents", func(t *testing.T) {
		c := assert.NewAborting(t)
		p := filepath.Join(root, "hello.txt")
		c.NoError(os.WriteFile(p, []byte("hello world"), 0o644))
		res, fail := call(t, "read", `{"file_path":"`+p+`"}`)
		c.Nil(fail, "unexpected failure")
		c.StrContains(textOf(res), "hello world", "got")
	})

	t.Run("read of a missing file is a Failure, not a crash", func(t *testing.T) {
		_, fail := call(t, "read", `{"file_path":"`+filepath.Join(root, "nope")+`"}`)
		assert.NewAborting(t).NotNil(fail, "expected a Failure for a missing file")
	})

	t.Run("unknown tool is a typed Failure", func(t *testing.T) {
		_, fail := call(t, "no_such_tool", `{}`)
		assert.NewAborting(t).NotNil(fail, "expected a Failure for an unknown tool")
	})

	t.Run("a failed call does not kill the server", func(t *testing.T) {
		// Prove the server is still alive after a failed call: issue a
		// Describe following the bad call.
		_, _ = call(t, "no_such_tool", `{}`)
		_, err := client.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{}))
		assert.NewAborting(t).NoError(err, "server died after a failed call")
	})

	t.Run("grep honours gitignore", func(t *testing.T) {
		c := assert.NewCollecting(t)
		c.Require().NoError(os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.txt\n"), 0o644))
		c.Require().NoError(os.WriteFile(filepath.Join(root, "secret.txt"), []byte("NEEDLE"), 0o644))
		c.Require().NoError(os.WriteFile(filepath.Join(root, "visible.txt"), []byte("NEEDLE"), 0o644))
		res, fail := call(t, "grep", `{"pattern":"NEEDLE","path":"`+root+`"}`)
		c.Require().Nil(fail, "unexpected failure")
		out := textOf(res)
		c.NotStrContains(out, "secret.txt", "gitignored file appeared — is --no-require-git still passed to rg?")
		c.StrContains(out, "visible.txt", "non-ignored file missing")
	})

	t.Run("edit refuses when the file changed under us", func(t *testing.T) {
		c := assert.NewAborting(t)
		p := filepath.Join(root, "race.txt")
		c.NoError(os.WriteFile(p, []byte("original"), 0o644))
		res, fail := call(t, "read", `{"file_path":"`+p+`"}`)
		c.Nil(fail, "read failed")
		stale := res.ObservedMtime[p]
		c.NotEq(0, stale, "read must report observed_mtime; without it the parent has nothing to send back")

		// Someone else writes.
		time.Sleep(10 * time.Millisecond) // filesystem mtime granularity
		c.NoError(os.WriteFile(p, []byte("changed by someone else"), 0o644))

		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "c", Tool: "edit", Timeout: durationpb.New(30 * time.Second),
			InputJson:   []byte(`{"file_path":"` + p + `","old_string":"original","new_string":"mine"}`),
			ExpectMtime: map[string]int64{p: stale},
		}))
		c.NoError(err)
		var failure *executorpb.Failure
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
				failure = ev.Failed
			}
		}
		_ = stream.Err()
		c.NotNil(failure, "edit against a stale mtime must fail — this is the TOCTOU guard")
		got, _ := os.ReadFile(p)
		c.Eq("changed by someone else", string(got), "the refused edit still wrote: %q", got)
	})

	t.Run("background execute returns a handle immediately", func(t *testing.T) {
		c := assert.NewCollecting(t)
		start := time.Now()
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-1", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"sleep 2; echo done"}`),
		}))
		c.Require().NoError(err)
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		c.Require().NotEq("", handle, "background execute must yield a handle")
		c.Require().LessOrEqual(2*time.Second, time.Since(start), "returned after")

		// Attach and wait for completion.
		attachStream, err := client.Attach(ctx, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		c.Require().NoError(err)
		var exitCode int32 = -1
		for attachStream.Receive() {
			if ev, ok := attachStream.Msg().Event.(*executorpb.AttachResponse_ExitCode); ok {
				exitCode = ev.ExitCode
			}
		}
		_ = attachStream.Err()
		c.Eq(0, exitCode, "exit code")
	})

	t.Run("a dropped Attach does not kill the job", func(t *testing.T) {
		c := assert.NewAborting(t)
		// Start a background job.
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-2", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"sleep 2; echo survived"}`),
		}))
		c.NoError(err)
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		c.NotEq("", handle, "no handle")

		// Attach, cancel it, then attache again — the job must survive.
		ctx1, cancel1 := context.WithCancel(ctx)
		attachStream1, err := client.Attach(ctx1, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		c.NoError(err)
		// Read one event then cancel.
		attachStream1.Receive()
		cancel1()

		// Wait past the job's natural completion.
		time.Sleep(3 * time.Second)

		// Attach again and confirm completion.
		attachStream2, err := client.Attach(ctx, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		c.NoError(err)
		var exited bool
		for attachStream2.Receive() {
			if _, ok := attachStream2.Msg().Event.(*executorpb.AttachResponse_ExitCode); ok {
				exited = true
			}
		}
		_ = attachStream2.Err()
		c.True(exited, "job did not complete — a dropped Attach must not kill the job")
	})

	t.Run("Attach streams output while the job is still running", func(t *testing.T) {
		c := assert.NewAborting(t)
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-live", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"echo first; sleep 3; echo second"}`),
		}))
		c.NoError(err)
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		c.NotEq("", handle, "no handle")

		attachCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		attach, err := client.Attach(attachCtx, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		c.NoError(err)
		var got []byte
		for attach.Receive() {
			if ev, ok := attach.Msg().Event.(*executorpb.AttachResponse_Output); ok {
				got = append(got, ev.Output.Data...)
			}
		}
		_ = attach.Err()

		// The 2s deadline expires while the job is still sleeping, so this
		// asserts output arrived BEFORE exit — which is the entire point of
		// a background handle.
		c.StrContains(string(got), "first", "Attach delivered %q before the job exited; want it to contain", got)
		c.NotStrContains(string(got), "second", "the job cannot have finished yet; got %q", got)
	})

	t.Run("Health lists running handles", func(t *testing.T) {
		c := assert.NewCollecting(t)
		stream, _ := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-3", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"sleep 10"}`),
		}))
		for stream.Receive() {
		}
		_ = stream.Err()

		resp, err := client.Health(ctx, connect.NewRequest(&executorpb.HealthRequest{}))
		c.Require().NoError(err)
		found := false
		for _, h := range resp.Msg.RunningHandles {
			if h == "bg-3" {
				found = true
				break
			}
		}
		c.True(found, "Health must list the running background job")
	})

	t.Run("JobOutput polls a running job without blocking", func(t *testing.T) {
		c := assert.NewAborting(t)
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-poll", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"echo hello; sleep 3"}`),
		}))
		c.NoError(err)
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()

		time.Sleep(500 * time.Millisecond)
		start := time.Now()
		resp, err := client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{Handle: handle}))
		c.NoError(err)
		c.LessOrEqual(time.Second, time.Since(start), "JobOutput blocked for")
		c.True(resp.Msg.Found, "Found=false for a live handle")
		c.False(resp.Msg.Exited, "Exited=true while the job is still sleeping")
		c.StrContains(string(resp.Msg.Data), "hello", "data = %q, want it to contain", resp.Msg.Data)

		// A second poll from the returned offset must return nothing new.
		resp2, err := client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{
			Handle: handle, Since: resp.Msg.Total,
		}))
		c.NoError(err)
		c.Empty(resp2.Msg.Data, "polling from the previous total returned")

		unknown, err := client.JobOutput(ctx, connect.NewRequest(&executorpb.JobOutputRequest{Handle: "nope"}))
		c.NoError(err)
		c.False(unknown.Msg.Found, "Found=true for an unknown handle")
	})

	t.Run("Cancel kills the whole process group", func(t *testing.T) {
		c := assert.NewAborting(t)
		marker := filepath.Join(root, "grandchild-alive")
		// The outer bash exits immediately; the backgrounded subshell keeps
		// touching a file. A kill that signals only the direct child leaves
		// it running.
		cmd := `bash -c 'while true; do touch ` + marker + `; sleep 0.2; done' & sleep 30`
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-group", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":` + strconv.Quote(cmd) + `}`),
		}))
		c.NoError(err)
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		c.NotEq("", handle, "no handle")
		time.Sleep(600 * time.Millisecond)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("the grandchild never ran: %v", err)
		}

		if _, err := client.Cancel(ctx, connect.NewRequest(&executorpb.CancelRequest{CallId: handle})); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		c.NoError(os.Remove(marker), "remove marker")
		time.Sleep(800 * time.Millisecond)
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the grandchild survived Cancel — kill signalled only the direct child")
		}
	})
}

// textOf extracts the concatenated text from a Result's content blocks.
func textOf(r *executorpb.Result) string {
	var b strings.Builder
	for _, c := range r.GetContent() {
		if t := c.GetText(); t != "" {
			b.WriteString(t)
		}
	}
	return b.String()
}
