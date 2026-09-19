// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	snap := childstore.Snapshot{
		Cwd:    "/tmp/proj",
		Labels: map[string]string{"rafiki/executor": "e-1"},
	}
	pool := &fakePymodulePool{
		live:   []execpool.LiveExecutor{{Executor: executors.Executor{ID: "e-1"}}},
		client: &fakePymoduleClient{},
	}
	exec, ok := newMCPPyModuleExecutor(pool, snap)
	if !ok || exec == nil {
		t.Fatalf("newMCPPyModuleExecutor = (%v, %v), want (non-nil, true)", exec, ok)
	}
	proxy := exec.(*mcpPyModuleRunExecutor)
	if proxy.callerCwd != "/tmp/proj" {
		t.Errorf("callerCwd = %q, want /tmp/proj", proxy.callerCwd)
	}
	if proxy.workspaceID != "" {
		t.Errorf("workspaceID = %q, want empty (the snapshot carries no workspace label)", proxy.workspaceID)
	}
}

func TestMCPPyModuleRunExecutorCarriesWorkspaceID(t *testing.T) {
	snap := childstore.Snapshot{
		Labels: map[string]string{
			"rafiki/executor":  "e-1",
			"rafiki/workspace": "ws-9",
		},
	}
	pool := &fakePymodulePool{
		live:   []execpool.LiveExecutor{{Executor: executors.Executor{ID: "e-1"}}},
		client: &fakePymoduleClient{},
	}
	exec, ok := newMCPPyModuleExecutor(pool, snap)
	if !ok || exec == nil {
		t.Fatalf("newMCPPyModuleExecutor = (%v, %v), want (non-nil, true)", exec, ok)
	}
	if got := exec.(*mcpPyModuleRunExecutor).workspaceID; got != "ws-9" {
		t.Errorf("workspaceID = %q, want ws-9", got)
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
// call, and captures the most recent request for assertions on what the proxy
// actually put on the wire.
type fakeExecuteHandler struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	events []*executorpb.ExecuteResponse
	last   *executorpb.ExecuteRequest
}

func (h *fakeExecuteHandler) Execute(
	ctx context.Context,
	req *connect.Request[executorpb.ExecuteRequest],
	stream *connect.ServerStream[executorpb.ExecuteResponse],
) error {
	h.last = req.Msg
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

// wireInput runs one proxied pymodule_run against a capturing handler and
// returns the request that went out. The result event is a fixed "ok": these
// tests pin what the proxy SENDS, not what comes back.
func wireInput(t *testing.T, exec *mcpPyModuleRunExecutor, input string) *executorpb.ExecuteRequest {
	t.Helper()
	h := &fakeExecuteHandler{
		events: []*executorpb.ExecuteResponse{
			{
				Event: &executorpb.ExecuteResponse_Result{
					Result: &executorpb.Result{
						Content: []*executorpb.ContentBlock{
							{Block: &executorpb.ContentBlock_Text{Text: "ok"}},
						},
					},
				},
			},
		},
	}
	mux := http.NewServeMux()
	path, handler := executorpbconnect.NewExecutorServiceHandler(h)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := executorpbconnect.NewExecutorServiceClient(srv.Client(), srv.URL)
	exec.client = client
	if _, err := exec.Run(context.Background(), json.RawMessage(input)); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if h.last == nil {
		t.Fatal("no Execute request reached the handler")
	}
	return h.last
}

// wireCwd extracts the `cwd` the proxy put on the wire.
func wireCwd(t *testing.T, req *executorpb.ExecuteRequest) string {
	t.Helper()
	var fields struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(req.GetInputJson(), &fields); err != nil {
		t.Fatalf("wire input %s does not unmarshal: %v", req.GetInputJson(), err)
	}
	return fields.Cwd
}

// TestMCPPyModuleRunRepoFieldSurvivesBindCwd: repo is required on the
// executor's pymodule_run, and bindCwd rewrites the input through a
// map[string]json.RawMessage round-trip -- exactly the shape that would
// silently drop a field it does not know, if wrong. The repo value must
// arrive on the wire byte-identical while cwd is still rewritten -- one
// proves the other is no accident of a short-circuit.
func TestMCPPyModuleRunRepoFieldSurvivesBindCwd(t *testing.T) {
	caller := filepath.Join(t.TempDir(), "proj")
	exec := &mcpPyModuleRunExecutor{callerCwd: caller}
	req := wireInput(t, exec, `{"repo":"ops_tools","script":"rotate","modules":["ops_tools"],"cwd":"sub"}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.GetInputJson(), &fields); err != nil {
		t.Fatalf("wire input %s does not unmarshal: %v", req.GetInputJson(), err)
	}
	if string(fields["repo"]) != `"ops_tools"` {
		t.Errorf("repo on the wire = %s, want byte-identical \"ops_tools\"", fields["repo"])
	}
	if string(fields["modules"]) != `["ops_tools"]` {
		t.Errorf("modules on the wire = %s, want byte-identical [\"ops_tools\"]", fields["modules"])
	}
	if got, want := wireCwd(t, req), filepath.Join(caller, "sub"); got != want {
		t.Errorf("wire cwd = %q, want %q (bindCwd still rewrote cwd)", got, want)
	}
}

// TestMCPPyModuleRunExecutorBindsCwdToTheCaller pins the MCP pymodule_run
// cwd contract: `your working directory` in the tool description is the
// CALLING CHILD's own cwd, not the executor's root. Before this, a relative
// cwd and the absent default both resolved against the executor's root
// registry (an Execute without a workspace id serves it), so the run landed
// on the executor's own checkout tree — loudly wrong when the name did not
// exist there, silently wrong when it did.
func TestMCPPyModuleRunExecutorBindsCwdToTheCaller(t *testing.T) {
	caller := filepath.Join(t.TempDir(), "supabase")
	t.Run("relative cwd resolves against the caller's cwd", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":"sub/tree"}`)
		if got, want := wireCwd(t, req), filepath.Join(caller, "sub/tree"); got != want {
			t.Errorf("wire cwd = %q, want %q", got, want)
		}
	})
	t.Run("absent cwd defaults to the caller's cwd", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod"}`)
		if got := wireCwd(t, req); got != caller {
			t.Errorf("wire cwd = %q, want %q", got, caller)
		}
	})
	t.Run("explicit empty cwd defaults too", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":""}`)
		if got := wireCwd(t, req); got != caller {
			t.Errorf("wire cwd = %q, want %q", got, caller)
		}
	})
	t.Run("absolute cwd passes through", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":"/somewhere/else"}`)
		if got := wireCwd(t, req); got != "/somewhere/else" {
			t.Errorf("wire cwd = %q, want it untouched", got)
		}
	})
	t.Run("~-prefixed cwd passes through", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":"~/notes"}`)
		if got := wireCwd(t, req); got != "~/notes" {
			t.Errorf("wire cwd = %q, want it untouched (the executor expands ~ against its own home)", got)
		}
	})
	t.Run("unknown caller cwd forwards untouched", func(t *testing.T) {
		// The executor's root stays the resolution base exactly as before the
		// bind existed: a caller with no usable cwd degrades, never corrupts.
		exec := &mcpPyModuleRunExecutor{}
		req := wireInput(t, exec, `{"script":"mod","cwd":"sub/tree"}`)
		if got := wireCwd(t, req); got != "sub/tree" {
			t.Errorf("wire cwd = %q, want it untouched", got)
		}
	})
	t.Run("null cwd defaults like the executor's own unmarshal", func(t *testing.T) {
		// json.Unmarshal of null into a string field is a silent no-op, so the
		// executor-side pymodule_run already treats null as the default; the
		// proxy must read it the same way or the two disagree about "default".
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":null}`)
		if got := wireCwd(t, req); got != caller {
			t.Errorf("wire cwd = %q, want %q", got, caller)
		}
	})
	t.Run("fields the proxy does not know survive byte-exact", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","args":["a","b"],"future":42}`)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(req.GetInputJson(), &fields); err != nil {
			t.Fatalf("wire input %s does not unmarshal: %v", req.GetInputJson(), err)
		}
		if string(fields["future"]) != "42" {
			t.Errorf("future = %s, want 42 (raw round-trip, no re-encoding)", fields["future"])
		}
		if string(fields["args"]) != `["a","b"]` {
			t.Errorf("args = %s, want [\"a\",\"b\"]", fields["args"])
		}
	})
	t.Run("malformed json forwards untouched", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{`)
		if got := string(req.GetInputJson()); got != `{` {
			t.Errorf("wire input = %q, want it untouched for the executor to report", got)
		}
	})
	t.Run("workspace id rides the request when the caller has one", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller, workspaceID: "ws-9"}
		req := wireInput(t, exec, `{"script":"mod"}`)
		if got := req.GetWorkspaceId(); got != "ws-9" {
			t.Errorf("workspace id = %q, want ws-9", got)
		}
	})
}
