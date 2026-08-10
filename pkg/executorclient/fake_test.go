package executorclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

func TestFakeExecutorRecordsCalls(t *testing.T) {
	f := executorclient.NewFake()
	f.SetResult("read", "file contents here")
	out, err := f.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "file contents here" {
		t.Fatalf("got %q", out)
	}
	if len(f.Calls()) != 1 || f.Calls()[0].Tool != "read" {
		t.Fatalf("calls = %v", f.Calls())
	}
}

func TestFakeExecutorCanFail(t *testing.T) {
	f := executorclient.NewFake()
	f.SetFailure("bash", executorpb.Failure_CODE_EXECUTOR_LOST, "gone")
	_, err := f.Execute(context.Background(), "bash", json.RawMessage(`{"command":"x"}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	// Typed, never collapsed to a string — the parent must be able to tell
	// executor_lost (retryable elsewhere) from denied (never retry).
	var fe *executorclient.FailureError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v; want a typed FailureError", err)
	}
	if fe.Failure.Code != executorpb.Failure_CODE_EXECUTOR_LOST {
		t.Errorf("code = %v, want CODE_EXECUTOR_LOST", fe.Failure.Code)
	}
}
