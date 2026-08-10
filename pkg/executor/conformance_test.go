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

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

// RunConformance is the contract every executor implementation must satisfy,
// run against a real client over a real socket.
func RunConformance(t *testing.T, client executorpbconnect.ExecutorServiceClient, root string) {
	t.Helper()
	ctx := context.Background()

	call := func(t *testing.T, tool string, input string) (*executorpb.Result, *executorpb.Failure) {
		t.Helper()
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "call-1", Tool: tool, InputJson: []byte(input), TimeoutMs: 30000,
		}))
		if err != nil {
			t.Fatalf("Execute(%s): %v", tool, err)
		}
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
		if err := stream.Err(); err != nil {
			t.Fatalf("stream error: %v", err)
		}
		return result, failure
	}

	t.Run("read returns file contents", func(t *testing.T) {
		p := filepath.Join(root, "hello.txt")
		if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, fail := call(t, "read", `{"file_path":"`+p+`"}`)
		if fail != nil {
			t.Fatalf("unexpected failure: %v", fail)
		}
		if !strings.Contains(textOf(res), "hello world") {
			t.Fatalf("got %q", textOf(res))
		}
	})

	t.Run("read of a missing file is a Failure, not a crash", func(t *testing.T) {
		_, fail := call(t, "read", `{"file_path":"`+filepath.Join(root, "nope")+`"}`)
		if fail == nil {
			t.Fatal("expected a Failure for a missing file")
		}
	})

	t.Run("unknown tool is a typed Failure", func(t *testing.T) {
		_, fail := call(t, "no_such_tool", `{}`)
		if fail == nil {
			t.Fatal("expected a Failure for an unknown tool")
		}
	})

	t.Run("a failed call does not kill the server", func(t *testing.T) {
		// Prove the server is still alive after a failed call: issue a
		// Describe following the bad call.
		_, _ = call(t, "no_such_tool", `{}`)
		if _, err := client.Describe(ctx, connect.NewRequest(&executorpb.DescribeRequest{})); err != nil {
			t.Fatalf("server died after a failed call: %v", err)
		}
	})

	t.Run("grep honours gitignore", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("secret.txt\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("NEEDLE"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("NEEDLE"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, fail := call(t, "grep", `{"pattern":"NEEDLE","path":"`+root+`"}`)
		if fail != nil {
			t.Fatalf("unexpected failure: %v", fail)
		}
		out := textOf(res)
		if strings.Contains(out, "secret.txt") {
			t.Error("gitignored file appeared — is --no-require-git still passed to rg?")
		}
		if !strings.Contains(out, "visible.txt") {
			t.Error("non-ignored file missing")
		}
	})

	t.Run("edit refuses when the file changed under us", func(t *testing.T) {
		p := filepath.Join(root, "race.txt")
		if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, fail := call(t, "read", `{"file_path":"`+p+`"}`)
		if fail != nil {
			t.Fatalf("read failed: %v", fail)
		}
		stale := res.ObservedMtime[p]
		if stale == 0 {
			t.Fatal("read must report observed_mtime; without it the parent has nothing to send back")
		}

		// Someone else writes.
		time.Sleep(10 * time.Millisecond) // filesystem mtime granularity
		if err := os.WriteFile(p, []byte("changed by someone else"), 0o644); err != nil {
			t.Fatal(err)
		}

		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "c", Tool: "edit", TimeoutMs: 30000,
			InputJson:   []byte(`{"file_path":"` + p + `","old_string":"original","new_string":"mine"}`),
			ExpectMtime: map[string]int64{p: stale},
		}))
		if err != nil {
			t.Fatal(err)
		}
		var failure *executorpb.Failure
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
				failure = ev.Failed
			}
		}
		_ = stream.Err()
		if failure == nil {
			t.Fatal("edit against a stale mtime must fail — this is the TOCTOU guard")
		}
		got, _ := os.ReadFile(p)
		if string(got) != "changed by someone else" {
			t.Fatalf("the refused edit still wrote: %q", got)
		}
	})

	t.Run("background execute returns a handle immediately", func(t *testing.T) {
		start := time.Now()
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-1", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"sleep 2; echo done"}`),
		}))
		if err != nil {
			t.Fatal(err)
		}
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		if handle == "" {
			t.Fatal("background execute must yield a handle")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("returned after %v; a background start must not block on the command", elapsed)
		}

		// Attach and wait for completion.
		attachStream, err := client.Attach(ctx, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		if err != nil {
			t.Fatal(err)
		}
		var exitCode int32 = -1
		for attachStream.Receive() {
			if ev, ok := attachStream.Msg().Event.(*executorpb.AttachResponse_ExitCode); ok {
				exitCode = ev.ExitCode
			}
		}
		_ = attachStream.Err()
		if exitCode != 0 {
			t.Errorf("exit code = %d, want 0", exitCode)
		}
	})

	t.Run("a dropped Attach does not kill the job", func(t *testing.T) {
		// Start a background job.
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-2", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"sleep 2; echo survived"}`),
		}))
		if err != nil {
			t.Fatal(err)
		}
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		if handle == "" {
			t.Fatal("no handle")
		}

		// Attach, cancel it, then attache again — the job must survive.
		ctx1, cancel1 := context.WithCancel(ctx)
		attachStream1, err := client.Attach(ctx1, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		if err != nil {
			t.Fatal(err)
		}
		// Read one event then cancel.
		attachStream1.Receive()
		cancel1()

		// Wait past the job's natural completion.
		time.Sleep(3 * time.Second)

		// Attach again and confirm completion.
		attachStream2, err := client.Attach(ctx, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		if err != nil {
			t.Fatal(err)
		}
		var exited bool
		for attachStream2.Receive() {
			if _, ok := attachStream2.Msg().Event.(*executorpb.AttachResponse_ExitCode); ok {
				exited = true
			}
		}
		_ = attachStream2.Err()
		if !exited {
			t.Fatal("job did not complete — a dropped Attach must not kill the job")
		}
	})

	t.Run("Attach streams output while the job is still running", func(t *testing.T) {
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-live", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"echo first; sleep 3; echo second"}`),
		}))
		if err != nil {
			t.Fatal(err)
		}
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		if handle == "" {
			t.Fatal("no handle")
		}

		attachCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		attach, err := client.Attach(attachCtx, connect.NewRequest(&executorpb.AttachRequest{Handle: handle}))
		if err != nil {
			t.Fatal(err)
		}
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
		if !strings.Contains(string(got), "first") {
			t.Fatalf("Attach delivered %q before the job exited; want it to contain %q", got, "first")
		}
		if strings.Contains(string(got), "second") {
			t.Fatalf("the job cannot have finished yet; got %q", got)
		}
	})

	t.Run("Health lists running handles", func(t *testing.T) {
		stream, _ := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-3", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":"sleep 10"}`),
		}))
		for stream.Receive() {
		}
		_ = stream.Err()

		resp, err := client.Health(ctx, connect.NewRequest(&executorpb.HealthRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, h := range resp.Msg.RunningHandles {
			if h == "bg-3" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Health must list the running background job")
		}
	})

	t.Run("Cancel kills the whole process group", func(t *testing.T) {
		marker := filepath.Join(root, "grandchild-alive")
		// The outer bash exits immediately; the backgrounded subshell keeps
		// touching a file. A kill that signals only the direct child leaves
		// it running.
		cmd := `bash -c 'while true; do touch ` + marker + `; sleep 0.2; done' & sleep 30`
		stream, err := client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
			CallId: "bg-group", Tool: "bash", Background: true,
			InputJson: []byte(`{"command":` + strconv.Quote(cmd) + `}`),
		}))
		if err != nil {
			t.Fatal(err)
		}
		var handle string
		for stream.Receive() {
			if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Handle); ok {
				handle = ev.Handle
				break
			}
		}
		_ = stream.Err()
		if handle == "" {
			t.Fatal("no handle")
		}
		time.Sleep(600 * time.Millisecond)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("the grandchild never ran: %v", err)
		}

		if _, err := client.Cancel(ctx, connect.NewRequest(&executorpb.CancelRequest{CallId: handle})); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		if err := os.Remove(marker); err != nil {
			t.Fatalf("remove marker: %v", err)
		}
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
