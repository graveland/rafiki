package executor

import (
	"context"
	"os/exec"
	"sync"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// Backend provisions and tears down workspaces. There is one implementation:
// native (this file, always pinned).
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
