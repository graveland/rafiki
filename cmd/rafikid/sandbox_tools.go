// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// sandboxController is the slice of *Controller the child-bound sandbox manager
// drives. *Controller satisfies it directly (pinned below); the interface exists
// so a test can substitute a recording fake and observe the identity and caller
// child every verb receives — the point of this binding is that both are closed
// over at construction and never arrive as a tool argument, and that property is
// only visible by watching the calls go out.
type sandboxController interface {
	SandboxCreate(ctx context.Context, owner users.Identity, callerChild string, spec protocol.SandboxSpec) (protocol.SandboxInfo, error)
	SandboxList(owner users.Identity) ([]protocol.SandboxInfo, error)
	SandboxRemove(ctx context.Context, owner users.Identity, callerChild, ref string) error
	// syncer is the daemon's path-sync backend, nil until the executor pool is
	// wired at boot. The child-bound manager takes it through this accessor so
	// the binding stays the same object it uses for the other sandbox verbs.
	syncer() *pathSyncer
}

var _ sandboxController = (*Controller)(nil)

// controllerSandboxes implements tools.SandboxManager for one fundi child.
//
// It follows controllerSpawner's binding rule: the daemon-stamped child id and
// the owner's user id are closed over at construction and appear in no method
// signature. That is the design, not an implementation detail — a caller id
// passed as an argument is a tool argument, and a tool argument is produced by
// an LLM that can be prompt-injected into naming somebody else. The identity is
// the owner's NON-admin one, exactly as the fundi recall binding builds it
// (agentRuntimeOptions) and the MCP/Connect faces resolve it (recallOwner): a
// child of an admin can never reach daemon-wide sandbox state.
//
// The two faces change together: this binding must grant a child the same
// sandbox verbs the Connect gate (CreateSandbox/RemoveSandbox/SyncPath/SyncRepo
// childScoped, ListSandboxes ownerScoped) and the MCP face (newMCPSandboxes)
// grant it.
type controllerSandboxes struct {
	c           sandboxController
	childID     string
	ownerUserID string
}

var _ tools.SandboxManager = (*controllerSandboxes)(nil)

// newControllerSandboxes binds one fundi child to the Controller's sandbox
// verbs, or returns nil when the binding would be unsafe.
//
// A fundi child ALWAYS carries a daemon-stamped child id, so an empty one means
// this binding is being built for something that is not a child. With
// callerChild == "" the Controller treats a call as an operator's (SandboxRemove
// would let it tear down ANY of the owner's rows), so rather than degrade into
// that, construction is REFUSED: nil leaves ToolsOpts.Sandboxes nil and the
// three sandbox tools simply do not materialize. This mirrors the MCP face's
// decline and is the "never act as an operator" rule, enforced at the one place
// the binding is created.
func newControllerSandboxes(c *Controller, childID, ownerUserID string) tools.SandboxManager {
	if childID == "" {
		return nil
	}
	return &controllerSandboxes{c: c, childID: childID, ownerUserID: ownerUserID}
}

// owner is the identity every verb passes to the Controller: the owner's user id
// with NO admin bit. Non-admin is structural — the literal never sets IsAdmin —
// the same shape agentRuntimeOptions uses for the recall binding.
func (m *controllerSandboxes) owner() users.Identity {
	return users.Identity{UserID: m.ownerUserID}
}

func (m *controllerSandboxes) Create(ctx context.Context, spec protocol.SandboxSpec) (protocol.SandboxInfo, error) {
	return m.c.SandboxCreate(ctx, m.owner(), m.childID, spec)
}

func (m *controllerSandboxes) List(ctx context.Context) ([]protocol.SandboxInfo, error) {
	return m.c.SandboxList(m.owner())
}

func (m *controllerSandboxes) Remove(ctx context.Context, ref string) error {
	return m.c.SandboxRemove(ctx, m.owner(), m.childID, ref)
}

// Sync relays a file or directory between two executors the bound child may
// reach. The owner and the caller's child id are the SAME values Create passes,
// closed over at construction and never taken from a tool argument.
func (m *controllerSandboxes) Sync(ctx context.Context, req protocol.SyncPathRequest) (protocol.SyncPathResult, error) {
	s := m.c.syncer()
	if s == nil {
		return protocol.SyncPathResult{}, errPathSyncUnavailable()
	}
	return s.SyncPath(ctx, m.owner(), m.childID, req)
}

// SyncRepo relays one git branch between two executors the bound child may
// reach, with the same construction-time owner and child id as Create.
func (m *controllerSandboxes) SyncRepo(ctx context.Context, req protocol.SyncRepoRequest) (protocol.SyncRepoResult, error) {
	s := m.c.syncer()
	if s == nil {
		return protocol.SyncRepoResult{}, errPathSyncUnavailable()
	}
	return s.SyncRepo(ctx, m.owner(), m.childID, req)
}

// errPathSyncUnavailable is what a sandbox tool face returns when this daemon
// has no path-sync backend (no executor pool at boot, so wirePathSync installed
// nothing). It is a *connectapi.ControllerError with the same Code/Message on
// both faces, so a caller learns the daemon cannot sync rather than that its
// request was malformed.
func errPathSyncUnavailable() error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrInternal,
		Message: "path sync is not available on this daemon",
	}
}
