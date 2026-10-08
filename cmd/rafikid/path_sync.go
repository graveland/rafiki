// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/users"
)

// pathSyncer brokers a tree sync between two executors. It is a separate type
// from the Controller so the two endpoints' authority can be reasoned about in
// one place: it decides which executors a caller may name (resolve), gates
// overwrite on the destination's ROW, and relays ReadTree into WriteTree
// without ever buffering the tree.
//
// It is constructed once at boot and wired into the Connect and MCP faces
// (task 5.1); nothing here reaches into the Controller's own fields except the
// read-only selection helpers it already exposes.
type pathSyncer struct {
	c    *Controller
	pool treeSyncExecutors
}

// treeSyncExecutors is the slice of *execpool.Pool the syncer needs: the live
// set (the authority on what is connected and on each executor's ROW) and the
// dialer for the tree-sync RPCs. An interface so the resolution guards are
// testable without a listener or a database.
type treeSyncExecutors interface {
	Live() []execpool.LiveExecutor
	ConnectClientFor(executorID string) (executorpbconnect.ExecutorServiceClient, error)
}

// syncTarget is a resolved endpoint: the executor's ROW (never its
// self-report) plus a connected client for the tree-sync RPCs.
type syncTarget struct {
	exec   executors.Executor
	client executorpbconnect.ExecutorServiceClient
}

func newPathSyncer(c *Controller, pool treeSyncExecutors) *pathSyncer {
	return &pathSyncer{c: c, pool: pool}
}

// notReachableErr is the ONE error a caller gets for an executor it may not
// name — whether that executor does not exist, belongs to another owner, or
// sits outside a child's lineage and sandbox reach. Missing and forbidden are
// deliberately indistinguishable so a caller cannot probe the fleet through
// this verb.
func notReachableErr(ref string) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: fmt.Sprintf("executor %q is not reachable by this caller", ref),
	}
}

// ambiguousRefErr is the not-reachable error for a ref that matched more than
// one executor the caller may reach. The count is named — it describes the
// caller's OWN reachable set, which the caller can already enumerate, so it
// leaks nothing about the rest of the fleet.
func ambiguousRefErr(ref string, matches int) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: fmt.Sprintf("executor %q is not reachable by this caller (matches %d executors)", ref, matches),
	}
}

// resolve returns the live, tree-sync-capable executor ref names, or an error.
// Every refusal it authors is a *connectapi.ControllerError; an error from the
// sandbox store or the executor pool is returned raw, and ConnectErr redacts it
// on the wire.
func (p *pathSyncer) resolve(ctx context.Context, owner users.Identity, callerChild, ref string) (syncTarget, error) {
	if p.c == nil {
		return syncTarget{}, &connectapi.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "path sync is not wired to a controller",
		}
	}
	candidates, rows, err := p.reachableExecutors(ctx, owner, callerChild)
	if err != nil {
		return syncTarget{}, err
	}
	chosen, matches := p.matchRef(ctx, ref, candidates, rows, owner.UserID)
	switch {
	case matches == 0:
		return syncTarget{}, notReachableErr(ref)
	case matches > 1:
		return syncTarget{}, ambiguousRefErr(ref, matches)
	}

	live, ok := p.liveExecutor(chosen.ID)
	if !ok {
		// The row may still exist (a permitted sandbox whose container is down),
		// but a connection is what a relay needs. This is a DIFFERENT error from
		// not-reachable: the caller has already proved it may name this executor,
		// so telling it the executor is offline leaks nothing.
		return syncTarget{}, &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: fmt.Sprintf("executor %q is not connected", ref),
		}
	}
	if live.Describe == nil || !live.Describe.GetTreeSync() {
		// A capability self-report: it only ever narrows what this executor will
		// do, so trusting it costs a refused sync, never an admitted one.
		return syncTarget{}, &connectapi.ControllerError{
			Code:    protocol.ErrFailedPrecondition,
			Message: fmt.Sprintf("executor %q does not support tree sync; upgrade it", ref),
		}
	}
	client, err := p.pool.ConnectClientFor(live.Executor.ID)
	if err != nil {
		return syncTarget{}, executorErr("executor", err)
	}
	// The live row is the authority for every gating fact (Isolation included),
	// never anything the executor self-reported.
	return syncTarget{exec: live.Executor, client: client}, nil
}

// reachableExecutors is the set of executor ROWS the caller may name, plus the
// sandbox rows it may reach by name/id.
//
// An operator or user (callerChild == "") gets every enabled live executor
// whose row OwnerUserID equals its own (NULL/empty on both sides counts as
// equal).
//
// A child gets its effective executor set — the parent's set intersected with
// its own selector, via effectiveExecutorSetFor.
//
// Either way, SANDBOX executors are stripped from that generic set and enter the
// candidate list ONLY through the permitted rows below. A sandbox executor
// admits "" and so sits in every empty-selector effective set; leaving it there
// would let a child address a sibling's or an unrelated child's sandbox by
// machine label or id. A sandbox row is reached by ownership — the caller
// created it (directly or through a descendant), the caller owns it, or it is
// subtree-scoped beneath a child the caller descends from — never by selection,
// and a person reaches their own named sandboxes the same way rather than
// through "every executor of mine".
func (p *pathSyncer) reachableExecutors(ctx context.Context, owner users.Identity, callerChild string) ([]executors.Executor, []sandbox.Row, error) {
	rows, err := p.permittedSandboxRows(ctx, owner.UserID, callerChild)
	if err != nil {
		return nil, nil, err
	}

	var generic []executors.Executor
	if callerChild == "" {
		for _, le := range p.pool.Live() {
			if !le.Executor.Enabled || le.Executor.OwnerUserID != owner.UserID {
				continue
			}
			generic = append(generic, le.Executor)
		}
	} else {
		labels := map[string]string{}
		if p.c.st != nil {
			if snap, ok := p.c.st.Get(callerChild); ok {
				labels = snap.Labels
			}
		}
		set, err := p.c.effectiveExecutorSetFor(callerChild, labels, owner.UserID)
		if err != nil {
			return nil, nil, err
		}
		generic = set
	}
	generic = dropSandboxCandidates(generic)

	seen := make(map[string]bool, len(generic)+len(rows))
	out := make([]executors.Executor, 0, len(generic)+len(rows))
	for _, e := range generic {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	for _, r := range rows {
		if seen[r.ExecutorID] {
			continue
		}
		e, ok := p.sandboxTargetExecutor(ctx, r, owner.UserID)
		if !ok {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	return out, rows, nil
}

// sandboxTargetExecutor resolves the executor a permitted sandbox row names,
// and reports whether this caller may use it. The executor ROW is the
// authority, so it is re-checked here rather than trusted from the sandbox row:
// the executor must be enabled and owned by the same owner (NULL/empty on both
// sides counts as equal). A row whose executor cannot be resolved is refused
// (fail closed), which is also what a stale or foreign executor row gets.
func (p *pathSyncer) sandboxTargetExecutor(ctx context.Context, r sandbox.Row, ownerUserID string) (executors.Executor, bool) {
	if r.ExecutorID == "" {
		return executors.Executor{}, false
	}
	e := p.executorRow(ctx, r.ExecutorID)
	if !e.Enabled || e.OwnerUserID != ownerUserID {
		return executors.Executor{}, false
	}
	return e, true
}

// permittedSandboxRows is the owner's live sandbox rows the caller may reach.
// An operator may reach all of the owner's rows; a child only those it created
// (directly or through a descendant), those it owns, or a subtree-scoped block
// owned by an ancestor.
func (p *pathSyncer) permittedSandboxRows(ctx context.Context, ownerUserID, callerChild string) ([]sandbox.Row, error) {
	if p.c == nil || p.c.sandboxStore == nil {
		return nil, nil
	}
	rows, err := p.c.sandboxStore.ListLive(ctx, ownerUserID)
	if err != nil {
		return nil, err
	}
	if callerChild == "" {
		return rows, nil
	}
	out := rows[:0:0]
	for _, r := range rows {
		if p.sandboxRowReachable(callerChild, r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// sandboxRowReachable is the child arm of permittedSandboxRows: the four
// predicates of the brief, and nothing else.
func (p *pathSyncer) sandboxRowReachable(callerChild string, r sandbox.Row) bool {
	if r.CreatedBy == callerChild {
		return true
	}
	if p.c.st != nil && p.c.st.IsDescendant(callerChild, r.CreatedBy) {
		return true
	}
	if r.OwnerChild == callerChild {
		return true
	}
	return r.Scope == protocol.ScopeSubtree && p.c.st != nil && p.c.st.IsDescendant(r.OwnerChild, callerChild)
}

// matchRef finds the executor ref names among candidates, or reports how many
// DISTINCT executors matched. Every pass contributes — a sandbox row's name or
// id, an exact machine label, an exact id, and resolveExecutorRef's trailing-id
// fragment — and the matches are deduped by executor id. No pass has priority,
// so a sandbox named like a real machine (or like another executor's id) cannot
// capture a request addressed to that machine: both matches surface and the ref
// is ambiguous. matches is 0 for no match and the distinct-executor count when
// it is ambiguous.
func (p *pathSyncer) matchRef(ctx context.Context, ref string, candidates []executors.Executor, rows []sandbox.Row, ownerUserID string) (executors.Executor, int) {
	matched := make(map[string]executors.Executor)
	add := func(e executors.Executor) {
		if e.ID == "" {
			return
		}
		matched[e.ID] = e
	}
	for _, r := range rows {
		if (r.Name != "" && r.Name == ref) || r.ID == ref {
			if e, ok := p.sandboxTargetExecutor(ctx, r, ownerUserID); ok {
				add(e)
			}
		}
	}
	for _, e := range candidates {
		if e.Labels["machine"] == ref {
			add(e)
		}
	}
	for _, e := range candidates {
		if e.ID == ref {
			add(e)
		}
	}
	if len(ref) >= executorRefMinLen {
		for _, e := range candidates {
			if strings.HasSuffix(e.ID, ref) {
				add(e)
			}
		}
	}
	switch len(matched) {
	case 0:
		return executors.Executor{}, 0
	case 1:
		for _, e := range matched {
			return e, 1
		}
	}
	return executors.Executor{}, len(matched)
}

// liveExecutor returns the pool's live view of id, if any.
func (p *pathSyncer) liveExecutor(id string) (execpool.LiveExecutor, bool) {
	if id == "" {
		return execpool.LiveExecutor{}, false
	}
	for _, le := range p.pool.Live() {
		if le.Executor.ID == id {
			return le, true
		}
	}
	return execpool.LiveExecutor{}, false
}

// executorRow returns id's row: the live pool's view when it is connected, the
// executor store's row otherwise, and a bare id when neither can answer. A
// disconnected executor still has a row — and the row is what carries the
// isolation the overwrite gate reads — so a miss here must not invent one.
func (p *pathSyncer) executorRow(ctx context.Context, id string) executors.Executor {
	if id == "" {
		return executors.Executor{}
	}
	if le, ok := p.liveExecutor(id); ok {
		return le.Executor
	}
	if p.c != nil && p.c.execStore != nil {
		if e, err := p.c.execStore.Get(ctx, id); err == nil {
			return e
		}
	}
	return executors.Executor{ID: id}
}

// SyncPath relays a file or directory from one executor to another.
//
// Both endpoints are validated, then resolved; overwrite is gated on the
// DESTINATION executor's row (never its self-report) because overwrite is what
// lets a sync replace a tree on a person's machine.
func (p *pathSyncer) SyncPath(ctx context.Context, owner users.Identity, callerChild string, req protocol.SyncPathRequest) (protocol.SyncPathResult, error) {
	if err := validateSyncEndpoint("source", req.Src); err != nil {
		return protocol.SyncPathResult{}, err
	}
	if err := validateSyncEndpoint("destination", req.Dst); err != nil {
		return protocol.SyncPathResult{}, err
	}
	if req.MaxBytes != nil && *req.MaxBytes <= 0 {
		return protocol.SyncPathResult{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "max_bytes must be greater than zero",
		}
	}

	src, err := p.resolve(ctx, owner, callerChild, req.Src.Executor)
	if err != nil {
		return protocol.SyncPathResult{}, err
	}
	dst, err := p.resolve(ctx, owner, callerChild, req.Dst.Executor)
	if err != nil {
		return protocol.SyncPathResult{}, err
	}

	if req.Overwrite && dst.exec.Isolation != "container" {
		return protocol.SyncPathResult{}, &connectapi.ControllerError{
			Code:    protocol.ErrPermissionDenied,
			Message: "overwrite is only allowed on container executors",
		}
	}

	files, bytes, err := p.transfer(ctx, src, dst, req.Src.Path, req.Dst.Path, req.Overwrite, req.MaxBytes)
	if err != nil {
		return protocol.SyncPathResult{}, err
	}
	return protocol.SyncPathResult{Files: files, Bytes: bytes}, nil
}

// validateSyncEndpoint refuses an endpoint that cannot be relayed: a named
// executor, and an absolute, already-clean path (an executor's --root is a
// working directory, not a scope, so a relative path has no single meaning).
func validateSyncEndpoint(side string, ep protocol.SyncEndpoint) error {
	if ep.Executor == "" {
		return &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("%s executor is required", side),
		}
	}
	if !filepath.IsAbs(ep.Path) || filepath.Clean(ep.Path) != ep.Path {
		return &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("%s path must be absolute and clean: %q", side, ep.Path),
		}
	}
	return nil
}

// transfer opens ReadTree on src and WriteTree on dst and relays the chunks
// with backpressure — nothing is buffered whole. Any error on either side
// cancels the derived context so both streams unwind, and the error is
// attributed to the side that produced it.
func (p *pathSyncer) transfer(ctx context.Context, src, dst syncTarget, srcPath, dstPath string, overwrite bool, maxBytes *int64) (files, bytes int64, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := src.client.ReadTree(ctx, connect.NewRequest(&executorpb.ReadTreeRequest{Path: srcPath}))
	if err != nil {
		return 0, 0, executorErr("source", err)
	}
	defer stream.Close()
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return 0, 0, executorErr("source", err)
		}
		return 0, 0, executorErr("source", errors.New("ReadTree stream carried no header"))
	}
	header := stream.Msg().GetHeader()
	if header == nil {
		return 0, 0, executorErr("source", errors.New("ReadTree stream did not start with a header"))
	}

	write := dst.client.WriteTree(ctx)
	start := &executorpb.WriteTreeRequest{Msg: &executorpb.WriteTreeRequest_Start{Start: &executorpb.WriteTreeStart{
		Path:      dstPath,
		IsDir:     header.GetIsDir(),
		Overwrite: overwrite,
		MaxBytes:  maxBytes,
	}}}
	if err := write.Send(start); err != nil {
		return 0, 0, destinationErr(write, err)
	}

	for stream.Receive() {
		chunk, ok := stream.Msg().Msg.(*executorpb.ReadTreeResponse_Chunk)
		if !ok {
			// A header may only appear first; the server never sends another.
			continue
		}
		if err := write.Send(&executorpb.WriteTreeRequest{Msg: &executorpb.WriteTreeRequest_Chunk{Chunk: chunk.Chunk}}); err != nil {
			return 0, 0, destinationErr(write, err)
		}
	}
	if err := stream.Err(); err != nil {
		return 0, 0, executorErr("source", err)
	}

	resp, err := write.CloseAndReceive()
	if err != nil {
		return 0, 0, executorErr("destination", err)
	}
	return resp.Msg.GetFiles(), resp.Msg.GetBytes(), nil
}

// destinationErr resolves what a failed destination Send stands for. A
// client-streaming Send that fails because the server answered an error and
// closed the stream surfaces only io.EOF, so CloseAndReceive is asked for the
// server's own error; the opaque fallback is used only when that yields no
// connect error either.
func destinationErr(write *connect.ClientStreamForClient[executorpb.WriteTreeRequest, executorpb.WriteTreeResponse], sendErr error) error {
	if _, recvErr := write.CloseAndReceive(); recvErr != nil {
		var ce *connect.Error
		if errors.As(recvErr, &ce) {
			return executorErr("destination", recvErr)
		}
	}
	return executorErr("destination", sendErr)
}

// executorErr converts an error from an executor RPC into a
// *connectapi.ControllerError. side is "source" or "destination" (or a step
// name) and prefixes the message.
//
// A connect error's code is mapped onto the daemon's own vocabulary. Only the
// codes whose messages this codebase authored (they come from pkg/executor) are
// forwarded; every other code, and every non-connect error (a dropped
// connection, whose text names infrastructure), becomes a fixed internal
// message so raw transport text is never forwarded.
func executorErr(side string, err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return &connectapi.ControllerError{Code: protocol.ErrInternal, Message: side + ": executor request failed"}
	}
	var code string
	switch ce.Code() {
	case connect.CodeInvalidArgument:
		code = protocol.ErrInvalidArgs
	case connect.CodeNotFound:
		code = protocol.ErrNotFound
	case connect.CodePermissionDenied:
		code = protocol.ErrPermissionDenied
	case connect.CodeFailedPrecondition:
		code = protocol.ErrFailedPrecondition
	case connect.CodeResourceExhausted:
		// A refusal the caller can act on (the destination is full, or the
		// caller's own max_bytes cap was hit) — keep its message.
		code = protocol.ErrInvalidArgs
	default:
		return &connectapi.ControllerError{Code: protocol.ErrInternal, Message: side + ": executor request failed"}
	}
	return &connectapi.ControllerError{Code: code, Message: side + ": " + ce.Message()}
}
