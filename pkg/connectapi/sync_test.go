// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// fakePathSyncer records every call so a test can assert the handler passed
// the resolved owner / caller child and the mapped request through unchanged.
type fakePathSyncer struct {
	syncPathOwner users.Identity
	syncPathChild string
	syncPathReq   protocol.SyncPathRequest
	syncPathRes   protocol.SyncPathResult
	syncPathErr   error

	syncRepoOwner users.Identity
	syncRepoChild string
	syncRepoReq   protocol.SyncRepoRequest
	syncRepoRes   protocol.SyncRepoResult
	syncRepoErr   error
}

func (f *fakePathSyncer) SyncPath(_ context.Context, owner users.Identity, callerChild string, req protocol.SyncPathRequest) (protocol.SyncPathResult, error) {
	f.syncPathOwner, f.syncPathChild, f.syncPathReq = owner, callerChild, req
	return f.syncPathRes, f.syncPathErr
}

func (f *fakePathSyncer) SyncRepo(_ context.Context, owner users.Identity, callerChild string, req protocol.SyncRepoRequest) (protocol.SyncRepoResult, error) {
	f.syncRepoOwner, f.syncRepoChild, f.syncRepoReq = owner, callerChild, req
	return f.syncRepoRes, f.syncRepoErr
}

// TestSetPathSyncerNilIsRefused: SetPathSyncer(nil) is refused, not stored — a
// stored pointer to a nil interface would defeat the Unavailable path and
// nil-panic the first handler call (the same rule SetSandboxManager follows).
func TestSetPathSyncerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetPathSyncer(nil)
	_, err := s.SyncPath(context.Background(), connect.NewRequest(&rafikiv1.SyncPathRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetPathSyncer(nil): got code")
}

// TestPathSyncerNilIsRefused is the shim the task's -run 'TestPathSyncer'
// verify pattern matches: go test -run is an unanchored substring match, and
// the pinned name TestSetPathSyncerNilIsRefused does not contain the pattern.
func TestPathSyncerNilIsRefused(t *testing.T) {
	t.Run("SetPathSyncerNilIsRefused", TestSetPathSyncerNilIsRefused)
}

// TestSyncPathUnavailableWhenUnwired: the verb answers Unavailable until the
// daemon wires the syncer, rather than panicking or reporting empty success.
func TestSyncPathUnavailableWhenUnwired(t *testing.T) {
	s := &Server{}
	_, err := s.SyncPath(context.Background(), connect.NewRequest(&rafikiv1.SyncPathRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "SyncPath unwired")
}

// TestSyncRepoUnavailableWhenUnwired: the twin of
// TestSyncPathUnavailableWhenUnwired for SyncRepo.
func TestSyncRepoUnavailableWhenUnwired(t *testing.T) {
	s := &Server{}
	_, err := s.SyncRepo(context.Background(), connect.NewRequest(&rafikiv1.SyncRepoRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "SyncRepo unwired")
}

// TestSyncPathMapsRequestAndAbsentMaxBytes pins the proto→domain mapping and
// the optional max_bytes contract: absent reaches the syncer as a nil pointer
// (no caller cap), present 5 as a pointer to 5, and present 0 is passed
// through as a pointer to 0 — the syncer, not the handler, refuses zero.
func TestSyncPathMapsRequestAndAbsentMaxBytes(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakePathSyncer{syncPathRes: protocol.SyncPathResult{Files: 3, Bytes: 42}}
	s := &Server{}
	s.SetPathSyncer(f)

	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})

	// Absent max_bytes: the syncer sees nil.
	resp, err := s.SyncPath(ctx, connect.NewRequest(&rafikiv1.SyncPathRequest{
		Src:       &rafikiv1.SyncEndpoint{Executor: "exec-a", Path: "/work/src"},
		Dst:       &rafikiv1.SyncEndpoint{Executor: "exec-b", Path: "/work/dst"},
		Overwrite: true,
	}))
	c.Require().NoError(err, "SyncPath")
	c.Eq("exec-a", f.syncPathReq.Src.Executor, "src.executor")
	c.Eq("/work/src", f.syncPathReq.Src.Path, "src.path")
	c.Eq("exec-b", f.syncPathReq.Dst.Executor, "dst.executor")
	c.Eq("/work/dst", f.syncPathReq.Dst.Path, "dst.path")
	c.True(f.syncPathReq.Overwrite, "overwrite")
	c.Nil(f.syncPathReq.MaxBytes, "absent max_bytes is a nil pointer")
	c.Eq(int64(3), resp.Msg.GetFiles(), "response files")
	c.Eq(int64(42), resp.Msg.GetBytes(), "response bytes")

	// Present 5: a pointer to 5.
	five := int64(5)
	_, err = s.SyncPath(ctx, connect.NewRequest(&rafikiv1.SyncPathRequest{MaxBytes: &five}))
	c.Require().NoError(err, "SyncPath max_bytes=5")
	c.Require().NotNil(f.syncPathReq.MaxBytes, "present max_bytes is a pointer")
	c.Eq(int64(5), *f.syncPathReq.MaxBytes, "max_bytes value")

	// Present 0: still a pointer — the zero-value trap is the syncer's to
	// refuse, and collapsing it to nil here would silently mean "no cap".
	zero := int64(0)
	_, err = s.SyncPath(ctx, connect.NewRequest(&rafikiv1.SyncPathRequest{MaxBytes: &zero}))
	c.Require().NoError(err, "SyncPath max_bytes=0")
	c.Require().NotNil(f.syncPathReq.MaxBytes, "present zero max_bytes is a pointer, not nil")
	c.Eq(int64(0), *f.syncPathReq.MaxBytes, "zero passed through")
}

// TestSyncRepoMapsRequestAndResult pins the SyncRepo proto→domain mapping and
// the result mapping back onto the wire.
func TestSyncRepoMapsRequestAndResult(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakePathSyncer{syncRepoRes: protocol.SyncRepoResult{
		OldOID:      "aaaa",
		NewOID:      "bbbb",
		CreatedRepo: true,
		UpToDate:    true,
	}}
	s := &Server{}
	s.SetPathSyncer(f)

	ctx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser})

	resp, err := s.SyncRepo(ctx, connect.NewRequest(&rafikiv1.SyncRepoRequest{
		Src:    &rafikiv1.SyncEndpoint{Executor: "exec-a", Path: "/work/repo"},
		Dst:    &rafikiv1.SyncEndpoint{Executor: "exec-b", Path: "/srv/repo"},
		Branch: "main",
		Force:  true,
	}))
	c.Require().NoError(err, "SyncRepo")
	c.Eq("exec-a", f.syncRepoReq.Src.Executor, "src.executor")
	c.Eq("/work/repo", f.syncRepoReq.Src.Path, "src.path")
	c.Eq("exec-b", f.syncRepoReq.Dst.Executor, "dst.executor")
	c.Eq("/srv/repo", f.syncRepoReq.Dst.Path, "dst.path")
	c.Eq("main", f.syncRepoReq.Branch, "branch")
	c.True(f.syncRepoReq.Force, "force")
	c.Eq("aaaa", resp.Msg.GetOldOid(), "old_oid")
	c.Eq("bbbb", resp.Msg.GetNewOid(), "new_oid")
	c.True(resp.Msg.GetCreatedRepo(), "created_repo")
	c.True(resp.Msg.GetUpToDate(), "up_to_date")
}

// TestSyncPathResolvesOwnerAndCallerChild pins the handler-side identity
// resolution, copied from CreateSandbox: a per-child credential passes its
// owner's user id as the owner, its own id as callerChild, and a NON-admin
// identity (a child-token admin bit is demoted before the syncer sees it); a
// user credential carries no caller child and keeps its admin bit.
func TestSyncPathResolvesOwnerAndCallerChild(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakePathSyncer{}
	s := &Server{}
	s.SetPathSyncer(f)

	// A child token with the admin bit set (a shape pkg/server does not
	// produce today) must arrive non-admin.
	childCtx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "owner-1", ChildID: "c_1", IsAdmin: true, Via: server.ProvenanceChildToken})
	_, err := s.SyncPath(childCtx, connect.NewRequest(&rafikiv1.SyncPathRequest{}))
	c.Require().NoError(err, "SyncPath (child)")
	c.Eq("owner-1", f.syncPathOwner.UserID, "child resolves its owner's user id")
	c.Eq("c_1", f.syncPathChild, "caller child passed through")
	c.False(f.syncPathOwner.IsAdmin, "a child-token admin bit must be demoted")

	// The same resolution on SyncRepo.
	_, err = s.SyncRepo(childCtx, connect.NewRequest(&rafikiv1.SyncRepoRequest{}))
	c.Require().NoError(err, "SyncRepo (child)")
	c.Eq("owner-1", f.syncRepoOwner.UserID, "child resolves its owner's user id")
	c.Eq("c_1", f.syncRepoChild, "caller child passed through")
	c.False(f.syncRepoOwner.IsAdmin, "a child-token admin bit must be demoted")

	// A genuine user credential carries no caller child and keeps admin.
	userCtx := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", IsAdmin: true, Via: server.ProvenanceUser})
	_, err = s.SyncPath(userCtx, connect.NewRequest(&rafikiv1.SyncPathRequest{}))
	c.Require().NoError(err, "SyncPath (admin user)")
	c.Eq("u1", f.syncPathOwner.UserID, "user owner user id")
	c.Eq("", f.syncPathChild, "a user credential carries no caller child")
	c.True(f.syncPathOwner.IsAdmin, "a user credential keeps its admin bit")
}

// TestSyncPathErrorMapsThroughConnectErr: the syncer's ControllerError reaches
// the caller through the same errCodeTable path CreateSandbox uses.
func TestSyncPathErrorMapsThroughConnectErr(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakePathSyncer{syncPathErr: &ControllerError{
		Code:    protocol.ErrPermissionDenied,
		Message: "overwrite is only allowed on container executors",
	}}
	s := &Server{}
	s.SetPathSyncer(f)

	_, err := s.SyncPath(context.Background(), connect.NewRequest(&rafikiv1.SyncPathRequest{}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "ControllerError code maps")

	// An uncoded error is redacted to CodeInternal, never forwarded.
	f.syncPathErr = errBoom{}
	_, err = s.SyncPath(context.Background(), connect.NewRequest(&rafikiv1.SyncPathRequest{}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "uncoded error maps to internal")
}

type errBoom struct{}

func (errBoom) Error() string { return "boom: secret host" }
