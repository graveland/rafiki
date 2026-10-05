// SPDX-License-Identifier: Apache-2.0

package executor_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

// TestToolSubprocessRunsUnderPinnedEnvNotProcessDrift is the end-to-end pin
// of the failure the executor's pinned environment exists to prevent. An
// executor serves children for hours; this machine's executor was observed
// with its process environment drifting after startup, and every child
// spawned after the drift inherited the drifted PATH. With Options.Env pinned
// at startup, a later mutation of the process environment — however it
// happens — must not reach a tool subprocess, neither as the child's
// environment nor as the interpreter lookup (exec.Command resolves a bare
// name against the process PATH at call time, so an unpinned lookup breaks
// spawning outright).
func TestToolSubprocessRunsUnderPinnedEnvNotProcessDrift(t *testing.T) {
	root := t.TempDir()

	// Startup: the environment is still clean. The executor snapshots it.
	srv := executor.NewServer(executor.Options{
		Root:        root,
		Concurrency: 2,
		Version:     "test",
		Env:         []string{"PATH=/usr/bin:/bin", "PINNED_PROBE=pinned"},
	})
	client := newTestClient(t, srv)

	// Hours later, the process environment has drifted. Real mutation, not a
	// mock: whatever sets variables on the process must not reach children.
	// t.Setenv (not os.Setenv) so the drift is cleaned up — the poisoned PATH
	// must not outlive this test and fail every test after it in the package.
	t.Setenv("PATH", "/drifted/bin")
	t.Setenv("PINNED_PROBE", "drifted")

	stream, err := client.Execute(context.Background(),
		connect.NewRequest(&executorpb.ExecuteRequest{
			CallId:    "t1",
			Tool:      "bash",
			InputJson: []byte(`{"command":"printf '%s|%s' \"$PINNED_PROBE\" \"${PATH%%:*}\""}`),
			Timeout:   durationpb.New(10 * time.Second),
		}))
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			for _, block := range r.GetContent() {
				out += block.GetText()
			}
		}
		if f := stream.Msg().GetFailed(); f != nil {
			t.Fatalf("bash failed: %s", f.GetMessage())
		}
	}
	c := assert.NewAborting(t)
	c.StrContains(out, "pinned|/usr/bin", "child must run under the startup snapshot, got")
	c.NotStrContains(out, "drifted", "child must not see drifted process values, got")
}
