// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
)

// SandboxManager is the narrow slice of the daemon needed to manage sandboxes:
// the three Controller-shaped verbs (Controller.SandboxCreate/SandboxList/
// SandboxRemove, cmd/rafikid). The owner identity and the calling child are
// resolved from the connection by the handlers here and passed IN — List has no
// other source for them — and the wired adapter (cmd/rafikid connect_sandbox.go)
// demotes the owner's admin bit for a child caller through recallOwner, so a
// child of an admin can never reach daemon-wide state.
//
// It carries protocol.SandboxSpec/SandboxInfo rather than proto mirror types
// for the same reason ChildLister carries protocol.ChildSummary: pkg/protocol
// is zero-dependency pure data and the SandboxInfo fields are the daemon's
// canonical answer.
type SandboxManager interface {
	// Create provisions a named sandbox for owner and returns it once its
	// executor has joined the pool. callerChild is the creating child's id
	// ("" for an operator) and bounds what the Controller stamps as CreatedBy.
	Create(ctx context.Context, owner users.Identity, callerChild string, spec protocol.SandboxSpec) (protocol.SandboxInfo, error)
	// List returns owner's live sandboxes.
	List(owner users.Identity) ([]protocol.SandboxInfo, error)
	// Remove tears down one of owner's live sandboxes by name or row id. A
	// child caller may remove only a row it created (or one a descendant
	// created).
	Remove(ctx context.Context, owner users.Identity, callerChild, ref string) error
}

// SetSandboxManager attaches the sandbox backend. Post-construction setter for
// the same reason as SetSkillManager: the Controller is built after this
// Server. A nil manager is refused rather than stored, the same rule as
// SetPymoduleManager: storing &m for a nil interface would defeat
// sandboxManager's Unavailable path and nil-panic the first handler call.
func (s *Server) SetSandboxManager(m SandboxManager) {
	if m == nil {
		return
	}
	s.sandboxes.Store(&m)
}

func (s *Server) sandboxManager() (SandboxManager, error) {
	p := s.sandboxes.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("sandbox backend not yet wired"))
	}
	return *p, nil
}

// sandboxCaller resolves the sandbox owner and the calling child from the
// connection. A per-child credential (ProvenanceChildToken) contributes its
// child id as callerChild and its owner's user id as the owner; every other
// identity — a user credential or the unix socket's nil local trust — passes
// callerChild == "".
//
// The admin bit is carried forward ONLY for a genuine user credential
// (recallOwner's rule, cmd/rafikid/recall.go): a child-token or any other
// non-user credential is demoted to non-admin HERE, so a credential that
// somehow presented an admin bit can never reach the sandbox manager as admin
// — the adapter's recallOwner demotion is then belt-and-braces, not the only
// guard. SandboxList is keyed on the owner's USER ID regardless, so admin
// never widens a fleet read.
func sandboxCaller(ctx context.Context) (users.Identity, string) {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		return users.Identity{}, ""
	}
	owner := users.Identity{
		UserID:   id.UserID,
		Username: id.Username,
		IsAdmin:  id.IsAdmin && id.IsUserCredential(),
	}
	if id.Via == server.ProvenanceChildToken {
		return owner, id.ChildID
	}
	return owner, ""
}

// sandboxSpecFromProto maps the wire spec onto the domain type, nil when the
// request carries none. String enum fields carry the same strings as the Go
// consts and are passed through unchanged — validation (unknown/empty kind, an
// empty spawn-block scope, a negative limit) lives in pkg/sandbox.Validate,
// reached through the Controller.
func sandboxSpecFromProto(m *rafikiv1.SandboxSpec) *protocol.SandboxSpec {
	if m == nil {
		return nil
	}
	out := &protocol.SandboxSpec{
		Name:           m.GetName(),
		Launcher:       m.GetLauncher(),
		Image:          m.GetImage(),
		Workdir:        m.GetWorkdir(),
		Network:        protocol.NetworkMode(m.GetNetwork()),
		ReadOnlyRootfs: m.GetReadOnlyRootfs(),
		Env:            m.GetEnv(),
		User:           m.GetUser(),
		MemoryBytes:    m.GetMemoryBytes(),
		CPUs:           m.GetCpus(),
		PidsLimit:      m.GetPidsLimit(),
		Labels:         m.GetLabels(),
		Scope:          protocol.SandboxScope(m.GetScope()),
	}
	for _, mt := range m.GetMounts() {
		if mt == nil {
			continue
		}
		out.Mounts = append(out.Mounts, protocol.SandboxMount{
			Target:   mt.GetTarget(),
			Kind:     protocol.MountKind(mt.GetKind()),
			HostPath: mt.GetHostPath(),
			Volume:   mt.GetVolume(),
		})
	}
	if ttl := m.GetTtl(); ttl != nil {
		out.TTL = ttl.AsDuration()
	}
	return out
}

// sandboxInfoToProto maps a sandbox onto the wire type. Times are Timestamps;
// created_at is omitted for a zero time, expires_at for a sandbox with no TTL.
func sandboxInfoToProto(s protocol.SandboxInfo) *rafikiv1.SandboxInfo {
	out := &rafikiv1.SandboxInfo{
		Id:          s.ID,
		Name:        s.Name,
		ExecutorId:  s.ExecutorID,
		Launcher:    s.Launcher,
		ContainerId: s.ContainerID,
		Image:       s.Image,
		Network:     string(s.Network),
		State:       s.State,
		Connected:   s.Connected,
		CreatedBy:   s.CreatedBy,
		OwnerChild:  s.OwnerChild,
		Scope:       string(s.Scope),
		Labels:      s.Labels,
	}
	if !s.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(s.CreatedAt)
	}
	if s.ExpiresAt != nil {
		out.ExpiresAt = timestamppb.New(*s.ExpiresAt)
	}
	return out
}

// CreateSandbox provisions a named sandbox (childScoped at the gate: a
// per-child credential may call it, and the Controller clamps and bounds what
// it may create). The sandbox's row — isolation, workspace_mode, owner and
// every rafiki/ label — is written by the daemon from what it verified, never
// from this request.
func (s *Server) CreateSandbox(
	ctx context.Context, req *connect.Request[rafikiv1.CreateSandboxRequest],
) (*connect.Response[rafikiv1.CreateSandboxResponse], error) {
	m, err := s.sandboxManager()
	if err != nil {
		return nil, err
	}
	specMsg := req.Msg.GetSpec()
	spec := sandboxSpecFromProto(specMsg)
	if spec == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("spec is required"))
	}
	if ttl := specMsg.GetTtl(); ttl != nil {
		if err := ttl.CheckValid(); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if ttl.AsDuration() < 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("ttl must not be negative"))
		}
	}
	owner, callerChild := sandboxCaller(ctx)
	info, err := m.Create(ctx, owner, callerChild, *spec)
	if err != nil {
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.CreateSandboxResponse{Sandbox: sandboxInfoToProto(info)}), nil
}

// ListSandboxes returns the caller's OWNER's live sandboxes (ownerScoped at the
// gate). The wire carries no owner — it is resolved from the credential, so a
// caller can never ask for someone else's fleet.
func (s *Server) ListSandboxes(
	ctx context.Context, _ *connect.Request[rafikiv1.ListSandboxesRequest],
) (*connect.Response[rafikiv1.ListSandboxesResponse], error) {
	m, err := s.sandboxManager()
	if err != nil {
		return nil, err
	}
	owner, _ := sandboxCaller(ctx)
	infos, err := m.List(owner)
	if err != nil {
		return nil, ConnectErr(err)
	}
	out := make([]*rafikiv1.SandboxInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, sandboxInfoToProto(i))
	}
	return connect.NewResponse(&rafikiv1.ListSandboxesResponse{Sandboxes: out}), nil
}

// RemoveSandbox tears down one of the caller's owner's sandboxes by name or id
// (childScoped at the gate; a child caller may remove only a row it or a
// descendant created, enforced in the Controller).
func (s *Server) RemoveSandbox(
	ctx context.Context, req *connect.Request[rafikiv1.RemoveSandboxRequest],
) (*connect.Response[rafikiv1.RemoveSandboxResponse], error) {
	m, err := s.sandboxManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetRef() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("ref is required"))
	}
	owner, callerChild := sandboxCaller(ctx)
	if err := m.Remove(ctx, owner, callerChild, req.Msg.GetRef()); err != nil {
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.RemoveSandboxResponse{}), nil
}
