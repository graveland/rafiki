// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
)

// mcpSandboxManager adapts *Controller to tools.SandboxManager for the MCP
// face, bound to ONE caller at construction -- the same binding rule as
// mcpPyModuleStore and controllerSpawner: no method takes a caller identity, so
// a tool argument can never name a different owner or act as an operator.
//
// owner is the caller's user identity and callerChild is its child id ("" for
// an interactive user). The identity handed to the Controller is resolved
// through sandboxOwnerIdentity, the SAME function the Connect adapter
// (connect_sandbox.go) uses, so a per-child caller acts as its OWNER's
// NON-admin identity with its own child id -- a child of an admin can never
// reach daemon-wide sandbox state, and the two faces grant the same set
// (TestMCPSandboxMatchesConnectChildPolicy).
type mcpSandboxManager struct {
	ctrl        *Controller
	owner       users.Identity
	callerChild string
}

var _ tools.SandboxManager = (*mcpSandboxManager)(nil)

// newMCPSandboxes binds the caller's owner and child id. It mirrors the Connect
// handler's sandboxCaller: a ProvenanceChildToken caller contributes its child
// id as callerChild; a user credential contributes "". An empty child id on a
// child-token credential is the degenerate shape the Connect gate also admits,
// and it resolves here exactly as it does there -- the two faces must not
// diverge on who may call a sandbox verb.
func newMCPSandboxes(ctrl *Controller, owner users.Identity, callerChild string) *mcpSandboxManager {
	return &mcpSandboxManager{ctrl: ctrl, owner: owner, callerChild: callerChild}
}

// mcpSandboxOwner builds the owner identity the sandbox binding receives,
// carrying the admin bit forward ONLY for a genuine user credential -- the same
// rule connectapi's sandboxCaller applies on the Connect plane. A per-child
// credential that somehow presents the admin bit (a hand-built identity; a real
// child token carries none) must never reach the sandbox manager as admin, so a
// child of an admin can never act with admin authority on a sandbox verb on
// EITHER face (CLAUDE.md: the two faces change together). This matters for the
// empty-ChildID child-token shape in particular: it carries no child id, so the
// manager cannot demote it via callerChild, and the admin bit must already be
// gone here.
func mcpSandboxOwner(id *server.Identity) users.Identity {
	return users.Identity{
		UserID:   id.UserID,
		Username: id.Username,
		IsAdmin:  id.IsAdmin && id.IsUserCredential(),
	}
}

func (m *mcpSandboxManager) Create(ctx context.Context, spec protocol.SandboxSpec) (protocol.SandboxInfo, error) {
	return m.ctrl.SandboxCreate(ctx, sandboxOwnerIdentity(m.owner, m.callerChild), m.callerChild, spec)
}

func (m *mcpSandboxManager) List(ctx context.Context) ([]protocol.SandboxInfo, error) {
	return m.ctrl.SandboxList(sandboxOwnerIdentity(m.owner, m.callerChild))
}

func (m *mcpSandboxManager) Remove(ctx context.Context, ref string) error {
	return m.ctrl.SandboxRemove(ctx, sandboxOwnerIdentity(m.owner, m.callerChild), m.callerChild, ref)
}

// Sync relays a file or directory between two executors this caller may reach.
// It passes the SAME owner and caller child Create does — the owner resolved
// through sandboxOwnerIdentity, so a child of an admin acts as its owner's
// NON-admin identity — and never a tool argument.
func (m *mcpSandboxManager) Sync(ctx context.Context, req protocol.SyncPathRequest) (protocol.SyncPathResult, error) {
	s := m.ctrl.syncer()
	if s == nil {
		return protocol.SyncPathResult{}, errPathSyncUnavailable()
	}
	return s.SyncPath(ctx, sandboxOwnerIdentity(m.owner, m.callerChild), m.callerChild, req)
}

// SyncRepo relays one git branch between two executors this caller may reach,
// with the same construction-time owner and caller child as Create.
func (m *mcpSandboxManager) SyncRepo(ctx context.Context, req protocol.SyncRepoRequest) (protocol.SyncRepoResult, error) {
	s := m.ctrl.syncer()
	if s == nil {
		return protocol.SyncRepoResult{}, errPathSyncUnavailable()
	}
	return s.SyncRepo(ctx, sandboxOwnerIdentity(m.owner, m.callerChild), m.callerChild, req)
}
