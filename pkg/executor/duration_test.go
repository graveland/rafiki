// SPDX-License-Identifier: Apache-2.0

package executor_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

// A Duration timeout round-trips to the handler: a 150ms timeout on a command
// that sleeps five seconds yields CODE_TIMEOUT, so the wire field — not a bare
// int — is what set the deadline.
func TestExecuteTimeoutIsCarriedAsADuration(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)

	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		CallId:    "round-trip",
		Tool:      "bash",
		InputJson: []byte(`{"command":"sleep 5"}`),
		Timeout:   durationpb.New(150 * time.Millisecond),
	}))
	c.NoError(err, "Execute")
	var code executorpb.Failure_Code
	for stream.Receive() {
		if ev, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
			code = ev.Failed.Code
		}
	}
	c.NoError(stream.Err(), "stream error")
	c.Eq(executorpb.Failure_CODE_TIMEOUT, code, "a 150ms Duration timeout must produce CODE_TIMEOUT")
}

// An unset timeout — and a present zero — mean no deadline, exactly as the old
// timeout_ms=0 did.
func TestExecuteUnsetOrZeroTimeoutRunsUnbounded(t *testing.T) {
	c := assert.NewAborting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)

	for _, tc := range []struct {
		name    string
		timeout *durationpb.Duration
	}{
		{"unset", nil},
		{"zero", durationpb.New(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
				CallId:    "unbounded",
				Tool:      "bash",
				InputJson: []byte(`{"command":"sleep 0.3"}`),
				Timeout:   tc.timeout,
			}))
			c.NoError(err, "Execute")
			var failed bool
			for stream.Receive() {
				if _, ok := stream.Msg().Event.(*executorpb.ExecuteResponse_Failed); ok {
					failed = true
				}
			}
			c.NoError(stream.Err(), "stream error")
			c.False(failed, "%s timeout must not impose a deadline", tc.name)
		})
	}
}

// A Duration the server cannot represent is refused InvalidArgument, per
// design rule 4, rather than silently clamped or accepted.
func TestExecuteRejectsOutOfRangeTimeout(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)

	stream, err := client.Execute(context.Background(), connect.NewRequest(&executorpb.ExecuteRequest{
		CallId:    "bad",
		Tool:      "bash",
		InputJson: []byte(`{"command":"true"}`),
		Timeout:   &durationpb.Duration{Seconds: 1 << 62},
	}))
	if err == nil {
		// A server-streaming refusal may surface on the first Receive.
		stream.Receive()
		err = stream.Err()
	}
	c.Require().NotNil(err, "an out-of-range Duration must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for an out-of-range Duration")
}
