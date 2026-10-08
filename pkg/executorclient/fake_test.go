package executorclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

func TestFakeExecutorRecordsCalls(t *testing.T) {
	c := assert.NewAborting(t)
	f := executorclient.NewFake()
	f.SetResult("read", "file contents here")
	outRes, err := f.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"/x"}`))
	out := outRes.Text
	c.NoError(err)
	c.Eq("file contents here", out, "got")
	if len(f.Calls()) != 1 || f.Calls()[0].Tool != "read" {
		t.Fatalf("calls = %v", f.Calls())
	}
}

func TestFakeExecutorCanFail(t *testing.T) {
	c := assert.NewCollecting(t)
	f := executorclient.NewFake()
	f.SetFailure("bash", executorpb.Failure_CODE_EXECUTOR_LOST, "gone")
	_, err := f.Execute(context.Background(), "bash", json.RawMessage(`{"command":"x"}`))
	c.Require().Error(err, "expected an error")
	// Typed, never collapsed to a string — the parent must be able to tell
	// executor_lost (retryable elsewhere) from denied (never retry).
	var fe *executorclient.FailureError
	c.Require().True(errors.As(err, &fe), "err = %v; want a typed FailureError", err)
	c.Eq(executorpb.Failure_CODE_EXECUTOR_LOST, fe.Failure.Code, "code")
}

func TestFakeImplementsTheJobSurface(t *testing.T) {
	c := assert.NewAborting(t)
	f := executorclient.NewFake()
	ctx := context.Background()

	handle, err := f.StartJob(ctx, "echo hi")
	c.NoError(err)
	c.NotEq("", handle, "StartJob returned an empty handle")

	f.SetJobOutput(handle, "hi\n", true, 0)

	snap, err := f.JobOutput(ctx, handle, 0)
	c.NoError(err)
	c.False(!snap.Found || !snap.Exited || snap.Data != "hi\n", "snapshot = %+v", snap)

	c.NoError(f.KillJob(ctx, handle))

	missing, err := f.JobOutput(ctx, "nope", 0)
	c.NoError(err)
	c.False(missing.Found, "Found=true for an unknown handle")
}
