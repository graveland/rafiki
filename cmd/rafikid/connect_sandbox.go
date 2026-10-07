// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
)

// connectSandbox adapts *Controller to connectapi.SandboxManager. A distinct
// type for the same reason connectLifecycle is one: Controller.SandboxCreate
// and friends already exist with their own signatures and are called from the
// spawn flow, so the Connect seam is a thin convert-and-delegate shell.
type connectSandbox struct{ c *Controller }

var _ connectapi.SandboxManager = connectSandbox{}

// sandboxOwner applies recallOwner's admin rule (cmd/rafikid/recall.go) at the
// adapter boundary: a child-provenance caller (callerChild != "") is bound to
// its OWNER's NON-admin identity, so a child of an admin can never reach
// daemon-wide state on a sandbox verb — the same non-admin binding the recall
// and MCP faces give a child. A user credential keeps its own admin bit, and
// the unix socket's zero identity stays zero. recallOwner grants admin only to
// a ProvenanceUser identity, which is exactly the callerChild == "" case.
func sandboxOwnerIdentity(owner users.Identity, callerChild string) users.Identity {
	id := &server.Identity{UserID: owner.UserID, Username: owner.Username, IsAdmin: owner.IsAdmin}
	if callerChild != "" {
		id.Via = server.ProvenanceChildToken
	} else {
		id.Via = server.ProvenanceUser
	}
	return recallOwner(id)
}

func (a connectSandbox) Create(ctx context.Context, owner users.Identity, callerChild string, spec protocol.SandboxSpec) (protocol.SandboxInfo, error) {
	return a.c.SandboxCreate(ctx, sandboxOwnerIdentity(owner, callerChild), callerChild, spec)
}

func (a connectSandbox) List(owner users.Identity) ([]protocol.SandboxInfo, error) {
	// ListSandboxes takes no child id, so the callerChild passed to the
	// Controller by SandboxCreate/SandboxRemove is absent here. A per-child
	// credential never carries IsAdmin in the first place (pkg/server /
	// usertoken.go builds it {UserID, ChildID, Via}), so the owner this
	// receives for a child is already non-admin; and SandboxList is keyed on
	// the owner's USER ID, so admin never widens the read.
	return a.c.SandboxList(sandboxOwnerIdentity(owner, ""))
}

func (a connectSandbox) Remove(ctx context.Context, owner users.Identity, callerChild, ref string) error {
	return a.c.SandboxRemove(ctx, sandboxOwnerIdentity(owner, callerChild), callerChild, ref)
}
