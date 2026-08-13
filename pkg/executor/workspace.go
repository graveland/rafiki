package executor

import (
	"context"
	"fmt"
	"os/exec"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// Backend provisions and tears down workspaces. Two implementations: native
// (this file) and container (task 4).
type Backend interface {
	Provision(ctx context.Context, req *executorpb.ProvisionRequest) (*workspace, error)
	Release(ctx context.Context, ws *workspace) error
}

// workspace is one provisioned place to run tools.
type workspace struct {
	id        string
	workdir   string // host path for native; container path for container
	roots     []string
	isolation string
	// exec runs a command IN this workspace. Native shells out directly;
	// container goes through `docker exec`.
	exec func(ctx context.Context, argv []string) *exec.Cmd
	// childID is the daemon's id, carried for labelling.
	childID string
}

// nativeBackend implements Backend for a local filesystem. It is always pinned.
type nativeBackend struct {
	root string
}

func newNativeBackend(root string) *nativeBackend {
	return &nativeBackend{root: root}
}

func (b *nativeBackend) Provision(_ context.Context, req *executorpb.ProvisionRequest) (*workspace, error) {
	if req.WorkspaceMode != "pinned" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("provision: native executor cannot honour workspace_mode=%q (it is always pinned)", req.WorkspaceMode))
	}
	id := randomID()
	ws := &workspace{
		id:        id,
		workdir:   b.root,
		roots:     []string{b.root},
		isolation: "none",
		childID:   req.ChildId,
		exec: func(ctx context.Context, argv []string) *exec.Cmd {
			c := exec.CommandContext(ctx, argv[0], argv[1:]...)
			c.Dir = b.root
			return c
		},
	}
	return ws, nil
}

func (b *nativeBackend) Release(_ context.Context, _ *workspace) error {
	return nil
}

// workspaceRegistry holds provisioned workspaces by id.
type workspaceRegistry struct {
	entries map[string]*workspace
}

func newWorkspaceRegistry() *workspaceRegistry {
	return &workspaceRegistry{entries: make(map[string]*workspace)}
}

func (r *workspaceRegistry) put(ws *workspace) {
	r.entries[ws.id] = ws
}

func (r *workspaceRegistry) get(id string) (*workspace, bool) {
	ws, ok := r.entries[id]
	return ws, ok
}

func (r *workspaceRegistry) remove(id string) {
	delete(r.entries, id)
}
