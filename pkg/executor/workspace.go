package executor

import (
	"sync"
)

// workspace is one provisioned place to run tools.
//
// There is one kind. An executor serves the filesystem it can see, working from
// the --root it was started with, and a workspace is a handle bound to that
// root for one child's lifetime. It used to be an interface with a native and a
// container implementation; rafiki does not launch containers any more, because
// a container running `rafiki executor serve` IS a container executor and
// docker already knows how to start one.
//
// Nothing here describes isolation. Whether this process's filesystem view is a
// container is a fact about the machine, and the authoritative copy lives on the
// executor's database row where the operator put it — not in a field this
// process fills in about itself.
type workspace struct {
	id      string
	workdir string
	roots   []string
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
