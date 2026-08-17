package executor

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
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
	// inner is the tool server running INSIDE this workspace. Non-nil for
	// container isolation, nil for native. It is what makes the mounts the grant
	// for every tool rather than for bash alone.
	inner *innerServer
}

// innerServer is a tool server running inside a container workspace, reached
// over the stdio of a `docker exec -i` process.
//
// There is no TCP option and adding one would undo part of the grant:
// workspace.Derive sets Network: "none" for both grant shapes, so the container
// has no network at all.
type innerServer struct {
	client executorpbconnect.ExecutorServiceClient
	// cmd is the `docker exec -i` process whose stdio carries the wire.
	// Deliberately NOT built with exec.CommandContext — see startInnerServer.
	cmd  *exec.Cmd
	conn net.Conn

	closeOnce sync.Once
}

// Close tears the inner server down, once. Teardown arrives from two
// directions — Release, and a Provision that failed after starting it — so the
// second caller must not be handed an error for the first one's work.
func (s *innerServer) Close() {
	s.closeOnce.Do(func() {
		// Closing the connection closes the exec process's stdin, which is how a
		// healthy inner server learns to exit. Kill anyway rather than waiting on
		// that: a wedged one would otherwise outlive `docker rm -f`.
		_ = s.conn.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.cmd.Wait()
	})
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
//
// The mutex is load-bearing, not defensive: Provision, Release and Execute are
// one goroutine per Connect request, and Execute reads the registry before it
// acquires the server's concurrency semaphore, so nothing else serialises them.
// An unguarded map here is a Go fatal error — it takes down the executor
// process and every child running on it, not just the racing call.
type workspaceRegistry struct {
	mu      sync.RWMutex
	entries map[string]*workspace
}

func newWorkspaceRegistry() *workspaceRegistry {
	return &workspaceRegistry{entries: make(map[string]*workspace)}
}

func (r *workspaceRegistry) put(ws *workspace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[ws.id] = ws
}

func (r *workspaceRegistry) get(id string) (*workspace, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ws, ok := r.entries[id]
	return ws, ok
}

func (r *workspaceRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}
