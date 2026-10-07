// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// fakeSandboxManager records every call so a test can assert the handler
// passed the resolved owner / child and the converted spec through unchanged.
type fakeSandboxManager struct {
	createdOwner users.Identity
	createdChild string
	createdSpec  protocol.SandboxSpec
	createInfo   protocol.SandboxInfo
	createErr    error

	listOwner users.Identity
	listInfos []protocol.SandboxInfo
	listErr   error

	removedOwner users.Identity
	removedChild string
	removedRef   string
	removeErr    error
}

func (f *fakeSandboxManager) Create(_ context.Context, owner users.Identity, callerChild string, spec protocol.SandboxSpec) (protocol.SandboxInfo, error) {
	f.createdOwner, f.createdChild, f.createdSpec = owner, callerChild, spec
	return f.createInfo, f.createErr
}

func (f *fakeSandboxManager) List(owner users.Identity) ([]protocol.SandboxInfo, error) {
	f.listOwner = owner
	return f.listInfos, f.listErr
}

func (f *fakeSandboxManager) Remove(_ context.Context, owner users.Identity, callerChild, ref string) error {
	f.removedOwner, f.removedChild, f.removedRef = owner, callerChild, ref
	return f.removeErr
}

// TestSetSandboxManagerNilIsRefused: SetSandboxManager(nil) is refused, not
// stored — a stored pointer to a nil interface would defeat the Unavailable
// path and nil-panic the first handler call (the same rule SetSkillManager and
// SetPymoduleManager follow).
func TestSetSandboxManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetSandboxManager(nil)
	_, err := s.ListSandboxes(context.Background(), connect.NewRequest(&rafikiv1.ListSandboxesRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetSandboxManager(nil): got code")
}

// TestSandboxHandlersUnwiredFailClosed: every verb answers Unavailable until the
// daemon wires the manager, rather than panicking or reporting empty success.
func TestSandboxHandlersUnwiredFailClosed(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	_, err := s.CreateSandbox(context.Background(), connect.NewRequest(&rafikiv1.CreateSandboxRequest{}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "CreateSandbox unwired")
	_, err = s.ListSandboxes(context.Background(), connect.NewRequest(&rafikiv1.ListSandboxesRequest{}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "ListSandboxes unwired")
	_, err = s.RemoveSandbox(context.Background(), connect.NewRequest(&rafikiv1.RemoveSandboxRequest{Ref: "x"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "RemoveSandbox unwired")
}

// TestSandboxCreateConvertsSpecAndPassesOwnerThrough pins the proto→domain spec
// mapping and the owner/child resolution: a USER credential passes its user id
// as the owner and callerChild == "".
func TestSandboxCreateConvertsSpecAndPassesOwnerThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeSandboxManager{createInfo: protocol.SandboxInfo{ID: "sbx_1", State: "ready"}}
	s := &Server{}
	s.SetSandboxManager(f)

	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})
	resp, err := s.CreateSandbox(ctx, connect.NewRequest(&rafikiv1.CreateSandboxRequest{
		Spec: &rafikiv1.SandboxSpec{
			Name:    "box",
			Image:   "rafiki/sandbox:test",
			Network: "none",
			Scope:   "self",
			Mounts: []*rafikiv1.SandboxMount{
				{Target: "/work", Kind: "rw", HostPath: "/srv/work"},
			},
			Env:    map[string]string{"K": "V"},
			Cpus:   2.5,
			Ttl:    durationpb.New(3600e9),
			Labels: map[string]string{"team": "a"},
		},
	}))
	c.Require().NoError(err, "CreateSandbox")
	c.Eq("u1", f.createdOwner.UserID, "owner user id")
	c.Eq("", f.createdChild, "a user credential carries no caller child")
	c.Eq("box", f.createdSpec.Name, "spec.Name")
	c.Eq(protocol.NetworkNone, f.createdSpec.Network, "spec.Network")
	c.Eq(protocol.ScopeSelf, f.createdSpec.Scope, "spec.Scope")
	c.Eq("rw", string(f.createdSpec.Mounts[0].Kind), "mount kind")
	c.Eq("/srv/work", f.createdSpec.Mounts[0].HostPath, "mount host path")
	c.Eq(2.5, f.createdSpec.CPUs, "cpus")
	c.Eq("V", f.createdSpec.Env["K"], "env")
	c.Eq("sbx_1", resp.Msg.GetSandbox().GetId(), "response maps the manager's info")
	c.Eq("ready", resp.Msg.GetSandbox().GetState(), "response state")
}

// TestSandboxCreateRefusesNegativeTTL: a negative TTL is refused before the
// manager is reached.
func TestSandboxCreateRefusesNegativeTTL(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeSandboxManager{}
	s := &Server{}
	s.SetSandboxManager(f)
	_, err := s.CreateSandbox(context.Background(), connect.NewRequest(&rafikiv1.CreateSandboxRequest{
		Spec: &rafikiv1.SandboxSpec{Image: "i", Ttl: durationpb.New(-1)},
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "negative ttl")
	c.Eq("", f.createdSpec.Image, "manager was not reached")
}

// TestSandboxCreateRequiresSpec: a missing spec is a caller error, not a nil
// deref.
func TestSandboxCreateRequiresSpec(t *testing.T) {
	s := &Server{}
	s.SetSandboxManager(&fakeSandboxManager{})
	_, err := s.CreateSandbox(context.Background(), connect.NewRequest(&rafikiv1.CreateSandboxRequest{}))
	assert.NewAborting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "no spec")
}

// TestSandboxListResolvesOwnerFromCredential pins the ownerScoped rule: a
// per-child credential's owner is the child's OWNER's user id (the wire
// carries no owner), and a UDS nil identity resolves the zero identity.
func TestSandboxListResolvesOwnerFromCredential(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeSandboxManager{listInfos: []protocol.SandboxInfo{{ID: "sbx_1"}}}
	s := &Server{}
	s.SetSandboxManager(f)

	childCtx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "owner-1", ChildID: "c_1", Via: server.ProvenanceChildToken})
	resp, err := s.ListSandboxes(childCtx, connect.NewRequest(&rafikiv1.ListSandboxesRequest{}))
	c.Require().NoError(err, "ListSandboxes (child)")
	c.Eq("owner-1", f.listOwner.UserID, "child resolves its owner's user id")
	c.Eq("sbx_1", resp.Msg.GetSandboxes()[0].GetId(), "rows mapped")

	udsCtx := context.Background() // no identity: the unix socket's local trust
	_, err = s.ListSandboxes(udsCtx, connect.NewRequest(&rafikiv1.ListSandboxesRequest{}))
	c.Require().NoError(err, "ListSandboxes (UDS)")
	c.Eq(users.Identity{}, f.listOwner, "UDS caller resolves the zero identity")
}

// TestSandboxRemovePassesChildAndRef: Remove passes the caller child and the
// ref through unchanged, and requires a ref.
func TestSandboxRemovePassesChildAndRef(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeSandboxManager{}
	s := &Server{}
	s.SetSandboxManager(f)

	_, err := s.RemoveSandbox(context.Background(), connect.NewRequest(&rafikiv1.RemoveSandboxRequest{}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty ref")
	c.Eq("", f.removedRef, "manager was not reached for an empty ref")

	childCtx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "owner-1", ChildID: "c_1", Via: server.ProvenanceChildToken})
	_, err = s.RemoveSandbox(childCtx, connect.NewRequest(&rafikiv1.RemoveSandboxRequest{Ref: "box"}))
	c.Require().NoError(err, "RemoveSandbox")
	c.Eq("c_1", f.removedChild, "caller child passed through")
	c.Eq("box", f.removedRef, "ref passed through")
}

// TestSandboxErrorMapsThroughConnectErr: the manager's ControllerError reaches
// the caller with its protocol code, not a redacted Internal.
func TestSandboxErrorMapsThroughConnectErr(t *testing.T) {
	f := &fakeSandboxManager{createErr: &ControllerError{Code: protocol.ErrInvalidArgs, Message: "bad image"}}
	s := &Server{}
	s.SetSandboxManager(f)
	_, err := s.CreateSandbox(context.Background(), connect.NewRequest(&rafikiv1.CreateSandboxRequest{
		Spec: &rafikiv1.SandboxSpec{Image: "i"},
	}))
	c := assert.NewCollecting(t)
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "ControllerError ErrInvalidArgs")
	c.StrContains(err.Error(), "bad image", "authored message survives")

	// An uncoded error is redacted to Internal.
	f.createErr = errors.New("pgx: connection refused")
	_, err = s.CreateSandbox(context.Background(), connect.NewRequest(&rafikiv1.CreateSandboxRequest{
		Spec: &rafikiv1.SandboxSpec{Image: "i"},
	}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "uncoded error -> Internal")
	c.False(strings.Contains(err.Error(), "pgx"), "raw cause must be redacted: %v", err)
}
