// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// maxExcludeOids bounds what the destination's head list contributes to one
// bundle request. A destination with more heads than this gets a full bundle
// instead: correct, merely larger, and never an unbounded argv.
const maxExcludeOids = 256

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
	if src.exec.ID == dst.exec.ID {
		// A repo relayed onto itself is meaningless, and the two endpoints
		// would share one scratch directory: refuse before any RPC.
		return protocol.SyncRepoResult{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "source and destination are the same executor",
		}
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
	exclude := excludesFromHeads(dstRefs.Msg.GetHeads())

	bundle, err := src.client.GitBundle(ctx, connect.NewRequest(&executorpb.GitBundleRequest{
		Repo:        req.Src.Path,
		Branch:      req.Branch,
		ExcludeOids: exclude,
	}))
	if err != nil {
		return protocol.SyncRepoResult{}, executorErr("src bundle", err)
	}
	tip, err := bundleTip(bundle.Msg)
	if err != nil {
		return protocol.SyncRepoResult{}, err
	}

	oldOID := headOID(dstRefs.Msg.GetHeads(), req.Branch)
	if bundle.Msg.GetUpToDate() {
		if oldOID == tip {
			return protocol.SyncRepoResult{
				UpToDate: true,
				OldOID:   oldOID,
				NewOID:   tip,
			}, nil
		}
		// git refused an empty bundle because the tip was already in the
		// destination's exclude set — but that tip belongs to a DIFFERENT
		// branch. The requested branch is not there yet, so ask again with no
		// exclusions: a full bundle is always non-empty, and the rare extra
		// bytes are the price of actually creating the branch.
		bundle, err = src.client.GitBundle(ctx, connect.NewRequest(&executorpb.GitBundleRequest{
			Repo:   req.Src.Path,
			Branch: req.Branch,
		}))
		if err != nil {
			return protocol.SyncRepoResult{}, executorErr("src bundle", err)
		}
		if _, err := bundleTip(bundle.Msg); err != nil {
			return protocol.SyncRepoResult{}, err
		}
	}

	// The bundle file must land directly in the DESTINATION's scratch
	// directory: that is the only parent GitFetchBundle will accept. The
	// basename comes from the SOURCE, so it is validated — a value that escapes
	// the join would aim the relay at a path of the source's choosing. path,
	// not filepath: the executor's separator is not necessarily this machine's.
	base := path.Base(bundle.Msg.GetBundlePath())
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, "/\x00") {
		return protocol.SyncRepoResult{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "source returned an unusable bundle path",
		}
	}
	dstBundle := path.Join(dstRefs.Msg.GetScratchDir(), base)
	if _, _, err := p.transfer(ctx, src, dst, bundle.Msg.GetBundlePath(), dstBundle, false, nil, transferOptions{requireFile: true}); err != nil {
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

// excludesFromHeads is the destination's head list reduced to what may be sent
// as bundle prerequisites: full lowercase-hex object ids only, and none at all
// once the list grows past maxExcludeOids (a full bundle is correct, merely
// larger). Anything else is dropped rather than forwarded to the source.
func excludesFromHeads(heads []*executorpb.GitRef) []string {
	out := make([]string, 0, len(heads))
	for _, head := range heads {
		if !validOID(head.GetOid()) {
			continue
		}
		out = append(out, head.GetOid())
	}
	if len(out) > maxExcludeOids {
		return nil
	}
	return out
}

// bundleTip returns the source's reported tip, refusing anything that is not a
// full lowercase-hex object id. The value comes from the source, so a malformed
// one is attributed to it rather than trusted.
func bundleTip(resp *executorpb.GitBundleResponse) (string, error) {
	tip := resp.GetTipOid()
	if !validOID(tip) {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("src bundle: source returned an unusable tip oid %q", tip),
		}
	}
	return tip, nil
}

// validOID reports whether s is a full lowercase-hex object id: 40 characters
// (SHA-1) or 64 (SHA-256). Nothing shorter, uppercase or otherwise encoded is
// a git object id.
func validOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
