// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// PathSyncer is the narrow slice of the daemon needed to relay a tree or a git
// branch between two executors (the pathSyncer in cmd/rafikid). The owner
// identity and the calling child are resolved from the connection by the
// handlers here and passed IN — the wire carries neither — and the wired
// adapter demotes the owner's admin bit for a child caller, so a child of an
// admin never reaches another owner's executors.
//
// It carries protocol.SyncPathRequest/SyncRepoRequest rather than proto mirror
// types for the same reason SandboxManager carries protocol.SandboxSpec:
// pkg/protocol is zero-dependency pure data and these are the daemon's
// canonical shapes.
type PathSyncer interface {
	// SyncPath copies req.Src's tree (or a single file) onto req.Dst. owner
	// and callerChild bound which executors and destinations are reachable; a
	// nil MaxBytes means no caller cap and a present value is validated by the
	// syncer, not here.
	SyncPath(ctx context.Context, owner users.Identity, callerChild string, req protocol.SyncPathRequest) (protocol.SyncPathResult, error)
	// SyncRepo moves req.Branch between the two executors as a bundle.
	SyncRepo(ctx context.Context, owner users.Identity, callerChild string, req protocol.SyncRepoRequest) (protocol.SyncRepoResult, error)
}

// SetPathSyncer attaches the path-sync backend. Post-construction setter for
// the same reason as SetSandboxManager: the Controller is built after this
// Server. A nil syncer is refused rather than stored, the same rule as
// SetSandboxManager: storing &m for a nil interface would defeat
// pathSyncer's Unavailable path and nil-panic the first handler call.
func (s *Server) SetPathSyncer(m PathSyncer) {
	if m == nil {
		return
	}
	s.syncers.Store(&m)
}

func (s *Server) pathSyncer() (PathSyncer, error) {
	p := s.syncers.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("path sync backend not yet wired"))
	}
	return *p, nil
}

// syncEndpointFromProto maps one wire endpoint onto the domain type. A nil
// message (an omitted field) maps to the zero endpoint, which the syncer
// refuses — an empty executor is a caller error, never "the sole executor".
func syncEndpointFromProto(m *rafikiv1.SyncEndpoint) protocol.SyncEndpoint {
	if m == nil {
		return protocol.SyncEndpoint{}
	}
	return protocol.SyncEndpoint{Executor: m.GetExecutor(), Path: m.GetPath()}
}

// SyncPath relays a tree between two executors (childScoped at the gate: a
// per-child credential may call it, and the syncer bounds it to executors the
// child can reach and sandboxes it or a descendant created). The owner and
// caller child are resolved from the credential exactly as CreateSandbox
// resolves them — a child caller's owner is its owner's NON-admin identity.
func (s *Server) SyncPath(
	ctx context.Context, req *connect.Request[rafikiv1.SyncPathRequest],
) (*connect.Response[rafikiv1.SyncPathResponse], error) {
	m, err := s.pathSyncer()
	if err != nil {
		return nil, err
	}
	out := protocol.SyncPathRequest{
		Src:       syncEndpointFromProto(req.Msg.GetSrc()),
		Dst:       syncEndpointFromProto(req.Msg.GetDst()),
		Overwrite: req.Msg.GetOverwrite(),
	}
	// max_bytes is optional on the wire: present (even as 0) is a caller cap
	// the syncer must see and refuse; absent is nil, meaning no caller cap.
	if req.Msg.MaxBytes != nil {
		v := req.Msg.GetMaxBytes()
		out.MaxBytes = &v
	}
	owner, callerChild := sandboxCaller(ctx)
	res, err := m.SyncPath(ctx, owner, callerChild, out)
	if err != nil {
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.SyncPathResponse{
		Files: res.Files,
		Bytes: res.Bytes,
	}), nil
}

// SyncRepo moves a git branch between two executors as a bundle (childScoped
// at the gate, bounded by the syncer exactly as SyncPath). There is no
// overwrite: re-seeding an existing repo is an incremental fetch, and a clean
// slate is a SyncPath --overwrite of the directory.
func (s *Server) SyncRepo(
	ctx context.Context, req *connect.Request[rafikiv1.SyncRepoRequest],
) (*connect.Response[rafikiv1.SyncRepoResponse], error) {
	m, err := s.pathSyncer()
	if err != nil {
		return nil, err
	}
	out := protocol.SyncRepoRequest{
		Src:    syncEndpointFromProto(req.Msg.GetSrc()),
		Dst:    syncEndpointFromProto(req.Msg.GetDst()),
		Branch: req.Msg.GetBranch(),
		Force:  req.Msg.GetForce(),
	}
	owner, callerChild := sandboxCaller(ctx)
	res, err := m.SyncRepo(ctx, owner, callerChild, out)
	if err != nil {
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.SyncRepoResponse{
		OldOid:      res.OldOID,
		NewOid:      res.NewOID,
		CreatedRepo: res.CreatedRepo,
		UpToDate:    res.UpToDate,
	}), nil
}
