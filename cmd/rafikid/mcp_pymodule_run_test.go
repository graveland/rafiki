// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// fatalPymodulePool is a pymodulePool whose methods fail the test if called
// at all — used to pin that newMCPPyModuleExecutor short-circuits on an
// empty "rafiki/executor" label before touching the pool.
type fatalPymodulePool struct {
	t *testing.T
}

func (f fatalPymodulePool) Live() []execpool.LiveExecutor {
	f.t.Fatal("Live called despite missing rafiki/executor label")
	return nil
}

func (f fatalPymodulePool) ConnectClientFor(string) (executorpbconnect.ExecutorServiceClient, error) {
	f.t.Fatal("ConnectClientFor called despite missing rafiki/executor label")
	return nil, nil
}

func TestMCPPyModuleRunExecutorDeclinesWithoutLabel(t *testing.T) {
	snap := childstore.Snapshot{Labels: map[string]string{}}
	exec, ok := newMCPPyModuleExecutor(fatalPymodulePool{t: t}, snap)
	if ok || exec != nil {
		t.Fatalf("newMCPPyModuleExecutor = (%v, %v), want (nil, false)", exec, ok)
	}
}

func TestMCPPyModuleRunExecutorDeclinesWhenExecutorNotLive(t *testing.T) {
	snap := childstore.Snapshot{Labels: map[string]string{"rafiki/executor": "e-1"}}
	pool := &fakePymodulePool{live: nil}
	exec, ok := newMCPPyModuleExecutor(pool, snap)
	if ok || exec != nil {
		t.Fatalf("newMCPPyModuleExecutor = (%v, %v), want (nil, false)", exec, ok)
	}
}

func TestMCPPyModuleRunExecutorBindsWhenLive(t *testing.T) {
	snap := childstore.Snapshot{Labels: map[string]string{"rafiki/executor": "e-1"}}
	pool := &fakePymodulePool{
		live:   []execpool.LiveExecutor{{Executor: executors.Executor{ID: "e-1"}}},
		client: &fakePymoduleClient{},
	}
	exec, ok := newMCPPyModuleExecutor(pool, snap)
	if !ok || exec == nil {
		t.Fatalf("newMCPPyModuleExecutor = (%v, %v), want (non-nil, true)", exec, ok)
	}
}

func TestMCPPyModuleRunBlueprintDeclines(t *testing.T) {
	tool, err := mcpPyModuleRunBlueprint{}.Materialize(tools.ToolOpts{})
	if err != nil {
		t.Fatalf("Materialize error = %v, want nil", err)
	}
	if tool != nil {
		t.Fatalf("Materialize tool = %v, want nil", tool)
	}
}

// fakeExecuteHandler serves executorpbconnect.ExecutorServiceHandler,
// replaying a scripted sequence of ExecuteResponse events for every Execute
// call.
type fakeExecuteHandler struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	events []*executorpb.ExecuteResponse
}

func (h *fakeExecuteHandler) Execute(
	ctx context.Context,
	req *connect.Request[executorpb.ExecuteRequest],
	stream *connect.ServerStream[executorpb.ExecuteResponse],
) error {
	for _, ev := range h.events {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

func TestMCPPyModuleRunExecutorDrainsResultText(t *testing.T) {
	t.Run("result text", func(t *testing.T) {
		mux := http.NewServeMux()
		path, handler := executorpbconnect.NewExecutorServiceHandler(&fakeExecuteHandler{
			events: []*executorpb.ExecuteResponse{
				{
					Event: &executorpb.ExecuteResponse_Result{
						Result: &executorpb.Result{
							Content: []*executorpb.ContentBlock{
								{Block: &executorpb.ContentBlock_Text{Text: "hello\n"}},
							},
						},
					},
				},
			},
		})
		mux.Handle(path, handler)
		srv := httptest.NewServer(mux)
		defer srv.Close()

		client := executorpbconnect.NewExecutorServiceClient(srv.Client(), srv.URL)
		exec := &mcpPyModuleRunExecutor{client: client}

		out, err := exec.Run(context.Background(), json.RawMessage(`{"script":"x.py"}`))
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}
		if out != "hello\n" {
			t.Fatalf("Run out = %q, want %q", out, "hello\n")
		}
	})

	t.Run("failure", func(t *testing.T) {
		mux := http.NewServeMux()
		path, handler := executorpbconnect.NewExecutorServiceHandler(&fakeExecuteHandler{
			events: []*executorpb.ExecuteResponse{
				{
					Event: &executorpb.ExecuteResponse_Failed{
						Failed: &executorpb.Failure{Message: "boom"},
					},
				},
			},
		})
		mux.Handle(path, handler)
		srv := httptest.NewServer(mux)
		defer srv.Close()

		client := executorpbconnect.NewExecutorServiceClient(srv.Client(), srv.URL)
		exec := &mcpPyModuleRunExecutor{client: client}

		_, err := exec.Run(context.Background(), json.RawMessage(`{"script":"x.py"}`))
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("Run error = %v, want error containing %q", err, "boom")
		}
	})
}
