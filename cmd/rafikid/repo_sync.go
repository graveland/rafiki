// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// SyncRepo relays one git branch from a source executor to a destination
// executor as a bundle: refs on the destination are the exclusions, the source
// writes the bundle into its scratch directory, the bundle is relayed with the
// same ReadTree/WriteTree path tree sync uses, and the destination fetches it.
// It is git's own fetch semantics on the far side — a checked-out branch and a
// non-fast-forward update are refused there unless force — so nothing the
// source planted can execute at the destination.
//
// Authorization is entirely resolve's: both endpoints come through it, and
// this verb adds no second path to an executor.
func (p *pathSyncer) SyncRepo(ctx context.Context, owner users.Identity, callerChild string, req protocol.SyncRepoRequest) (protocol.SyncRepoResult, error) {
	if req.Branch == "" {
		return protocol.SyncRepoResult{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "branch is required",
		}
	}
	if strings.HasPrefix(req.Branch, "-") {
		return protocol.SyncRepoResult{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "branch must not start with '-'",
		}
	}
	if err := validateSyncEndpoint("source", req.Src); err != nil {
		return protocol.SyncRepoResult{}, err
	}
	if err := validateSyncEndpoint("destination", req.Dst); err != nil {
		return protocol.SyncRepoResult{}, err
	}

	src, err := p.resolve(ctx, owner, callerChild, req.Src.Executor)
	if err != nil {
		return protocol.SyncRepoResult{}, err
	}
	dst, err := p.resolve(ctx, owner, callerChild, req.Dst.Executor)
	if err != nil {
		return protocol.SyncRepoResult{}, err
	}

	dstRefs, err := dst.client.GitRefs(ctx, connect.NewRequest(&executorpb.GitRefsRequest{Repo: req.Dst.Path}))
	if err != nil {
		return protocol.SyncRepoResult{}, executorErr("dst refs", err)
	}
	srcRefs, err := src.client.GitRefs(ctx, connect.NewRequest(&executorpb.GitRefsRequest{Repo: req.Src.Path}))
	if err != nil {
		return protocol.SyncRepoResult{}, executorErr("src refs", err)
	}
	if !srcRefs.Msg.GetExists() {
		return protocol.SyncRepoResult{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: "source is not a git repository",
		}
	}

	// Everything the destination already has is a prerequisite the source need
	// not send: the bundle carries only what is new on the source.
	exclude := make([]string, 0, len(dstRefs.Msg.GetHeads()))
	for _, head := range dstRefs.Msg.GetHeads() {
		exclude = append(exclude, head.GetOid())
	}

	bundle, err := src.client.GitBundle(ctx, connect.NewRequest(&executorpb.GitBundleRequest{
		Repo:        req.Src.Path,
		Branch:      req.Branch,
		ExcludeOids: exclude,
	}))
	if err != nil {
		return protocol.SyncRepoResult{}, executorErr("src bundle", err)
	}

	oldOID := headOID(dstRefs.Msg.GetHeads(), req.Branch)
	if bundle.Msg.GetUpToDate() {
		return protocol.SyncRepoResult{
			UpToDate: true,
			OldOID:   oldOID,
			NewOID:   bundle.Msg.GetTipOid(),
		}, nil
	}

	// The bundle file must land directly in the DESTINATION's scratch
	// directory: that is the only parent GitFetchBundle will accept. path, not
	// filepath — the executor's separator is not necessarily this machine's.
	dstBundle := path.Join(dstRefs.Msg.GetScratchDir(), path.Base(bundle.Msg.GetBundlePath()))
	if _, _, err := p.transfer(ctx, src, dst, bundle.Msg.GetBundlePath(), dstBundle, false, nil); err != nil {
		return protocol.SyncRepoResult{}, err
	}

	fetched, err := dst.client.GitFetchBundle(ctx, connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo:       req.Dst.Path,
		BundlePath: dstBundle,
		Branch:     req.Branch,
		Force:      req.Force,
	}))
	if err != nil {
		return protocol.SyncRepoResult{}, executorErr("dst fetch", err)
	}
	return protocol.SyncRepoResult{
		OldOID:      fetched.Msg.GetOldOid(),
		NewOID:      fetched.Msg.GetNewOid(),
		CreatedRepo: fetched.Msg.GetCreatedRepo(),
	}, nil
}

// headOID returns the object id of the head named branch, or "" when the
// destination has no such branch.
func headOID(heads []*executorpb.GitRef, branch string) string {
	for _, head := range heads {
		if head.GetName() == branch {
			return head.GetOid()
		}
	}
	return ""
}
