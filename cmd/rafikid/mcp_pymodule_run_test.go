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

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).False(ok || exec != nil, "newMCPPyModuleExecutor = (%v, %v), want (nil, false)", exec, ok)
}

func TestMCPPyModuleRunExecutorDeclinesWhenExecutorNotLive(t *testing.T) {
	snap := childstore.Snapshot{Labels: map[string]string{"rafiki/executor": "e-1"}}
	pool := &fakePymodulePool{live: nil}
	exec, ok := newMCPPyModuleExecutor(pool, snap)
	assert.NewAborting(t).False(ok || exec != nil, "newMCPPyModuleExecutor = (%v, %v), want (nil, false)", exec, ok)
}

func TestMCPPyModuleRunExecutorBindsWhenLive(t *testing.T) {
	c := assert.NewCollecting(t)
	snap := childstore.Snapshot{
		Cwd:    "/tmp/proj",
		Labels: map[string]string{"rafiki/executor": "e-1"},
	}
	pool := &fakePymodulePool{
		live:   []execpool.LiveExecutor{{Executor: executors.Executor{ID: "e-1"}}},
		client: &fakePymoduleClient{},
	}
	exec, ok := newMCPPyModuleExecutor(pool, snap)
	c.Require().False(!ok || exec == nil, "newMCPPyModuleExecutor = (%v, %v), want (non-nil, true)", exec, ok)
	proxy := exec.(*mcpPyModuleRunExecutor)
	c.Eq("/tmp/proj", proxy.callerCwd, "callerCwd")
	c.Eq("", proxy.workspaceID, "workspaceID")
}

func TestMCPPyModuleRunExecutorCarriesWorkspaceID(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().False(!ok || exec == nil, "newMCPPyModuleExecutor = (%v, %v), want (non-nil, true)", exec, ok)
	c.Eq("ws-9", exec.(*mcpPyModuleRunExecutor).workspaceID, "workspaceID")
}

func TestMCPPyModuleRunBlueprintDeclines(t *testing.T) {
	c := assert.NewAborting(t)
	tool, err := mcpPyModuleRunBlueprint{}.Materialize(tools.ToolOpts{})
	c.NoError(err, "Materialize error")
	c.Nil(tool, "Materialize tool")
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
		c := assert.NewAborting(t)
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
		c.NoError(err, "Run error")
		c.Eq("hello\n", out, "Run out")
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
		assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "boom"), "Run error = %v, want error containing %q", err, "boom")
	})
}

// wireInput runs one proxied pymodule_run against a capturing handler and
// returns the request that went out. The result event is a fixed "ok": these
// tests pin what the proxy SENDS, not what comes back.
func wireInput(t *testing.T, exec *mcpPyModuleRunExecutor, input string) *executorpb.ExecuteRequest {
	t.Helper()
	c := assert.NewAborting(t)
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
	_, err := exec.Run(context.Background(), json.RawMessage(input))
	c.NoError(err, "Run error")
	c.NotNil(h.last, "no Execute request reached the handler")
	return h.last
}

// wireCwd extracts the `cwd` the proxy put on the wire.
func wireCwd(t *testing.T, req *executorpb.ExecuteRequest) string {
	t.Helper()
	var fields struct {
		Cwd string `json:"cwd"`
	}
	assert.NewAborting(t).NoError(json.Unmarshal(req.GetInputJson(), &fields), "wire input %s does not unmarshal", req.GetInputJson())
	return fields.Cwd
}

// TestMCPPyModuleRunRepoFieldSurvivesBindCwd: repo is required on the
// executor's pymodule_run, and bindCwd rewrites the input through a
// map[string]json.RawMessage round-trip -- exactly the shape that would
// silently drop a field it does not know, if wrong. The repo value must
// arrive on the wire byte-identical while cwd is still rewritten -- one
// proves the other is no accident of a short-circuit.
func TestMCPPyModuleRunRepoFieldSurvivesBindCwd(t *testing.T) {
	c := assert.NewCollecting(t)
	caller := filepath.Join(t.TempDir(), "proj")
	exec := &mcpPyModuleRunExecutor{callerCwd: caller}
	req := wireInput(t, exec, `{"repo":"ops_tools","script":"rotate","modules":["ops_tools"],"cwd":"sub"}`)
	var fields map[string]json.RawMessage
	c.Require().NoError(json.Unmarshal(req.GetInputJson(), &fields), "wire input %s does not unmarshal", req.GetInputJson())
	c.Eq(`"ops_tools"`, string(fields["repo"]), "repo on the wire = %s, want byte-identical \"ops_tools\"", fields["repo"])
	c.Eq(`["ops_tools"]`, string(fields["modules"]), "modules on the wire = %s, want byte-identical [\"ops_tools\"]", fields["modules"])
	got, want := wireCwd(t, req), filepath.Join(caller, "sub")
	c.Eq(want, got, "wire cwd")
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
		got, want := wireCwd(t, req), filepath.Join(caller, "sub/tree")
		assert.NewCollecting(t).Eq(want, got, "wire cwd")
	})
	t.Run("absent cwd defaults to the caller's cwd", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod"}`)
		assert.NewCollecting(t).Eq(caller, wireCwd(t, req), "wire cwd")
	})
	t.Run("explicit empty cwd defaults too", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":""}`)
		assert.NewCollecting(t).Eq(caller, wireCwd(t, req), "wire cwd")
	})
	t.Run("absolute cwd passes through", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":"/somewhere/else"}`)
		assert.NewCollecting(t).Eq("/somewhere/else", wireCwd(t, req), "wire cwd")
	})
	t.Run("~-prefixed cwd passes through", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":"~/notes"}`)
		assert.NewCollecting(t).Eq("~/notes", wireCwd(t, req), "wire cwd")
	})
	t.Run("unknown caller cwd forwards untouched", func(t *testing.T) {
		// The executor's root stays the resolution base exactly as before the
		// bind existed: a caller with no usable cwd degrades, never corrupts.
		exec := &mcpPyModuleRunExecutor{}
		req := wireInput(t, exec, `{"script":"mod","cwd":"sub/tree"}`)
		assert.NewCollecting(t).Eq("sub/tree", wireCwd(t, req), "wire cwd")
	})
	t.Run("null cwd defaults like the executor's own unmarshal", func(t *testing.T) {
		// json.Unmarshal of null into a string field is a silent no-op, so the
		// executor-side pymodule_run already treats null as the default; the
		// proxy must read it the same way or the two disagree about "default".
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","cwd":null}`)
		assert.NewCollecting(t).Eq(caller, wireCwd(t, req), "wire cwd")
	})
	t.Run("fields the proxy does not know survive byte-exact", func(t *testing.T) {
		c := assert.NewCollecting(t)
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{"script":"mod","args":["a","b"],"future":42}`)
		var fields map[string]json.RawMessage
		c.Require().NoError(json.Unmarshal(req.GetInputJson(), &fields), "wire input %s does not unmarshal", req.GetInputJson())
		c.Eq("42", string(fields["future"]), "future = %s, want 42 (raw round-trip, no re-encoding)", fields["future"])
		c.Eq(`["a","b"]`, string(fields["args"]), "args = %s, want [\"a\",\"b\"]", fields["args"])
	})
	t.Run("malformed json forwards untouched", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller}
		req := wireInput(t, exec, `{`)
		assert.NewCollecting(t).Eq(`{`, string(req.GetInputJson()), "wire input")
	})
	t.Run("workspace id rides the request when the caller has one", func(t *testing.T) {
		exec := &mcpPyModuleRunExecutor{callerCwd: caller, workspaceID: "ws-9"}
		req := wireInput(t, exec, `{"script":"mod"}`)
		assert.NewCollecting(t).Eq("ws-9", req.GetWorkspaceId(), "workspace id")
	})
}
