package executor_test

import (
	"context"
	"os"
	"path/filepath"
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
