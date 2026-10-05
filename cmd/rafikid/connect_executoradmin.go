// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
)

// connectExecutorAdmin adapts *Controller to connectapi.ExecutorAdmin — the
// operator-side executor enroll/create/label/disable/enable/delete RPCs. A
// distinct type for the same reason connectModels is one: the Controller
// methods this delegates to already exist with these signatures, and renaming
// them would touch every existing caller for no gain.
//
// Errors pass through UNMAPPED. The connectapi handler owns error mapping —
// executorAdminErr logs an uncoded cause (ConnectErr redacts it) and hands a
// ControllerError to ConnectErr with its authored message intact. Mapping here
// instead would hand the handler an already-mapped *connect.Error, which its
// own ConnectErr pass would re-classify as Internal and lose the code.
type connectExecutorAdmin struct{ c *Controller }

// executorAuthority resolves what the caller's connection identity may see
// and touch on this face. A nil identity (the unix socket's local trust) is
// the operator at the daemon host and sees everything; so is an admin user
// credential; any other user credential is scoped to rows whose OwnerUserID
// equals its own. A child credential is refused by the route's policy
// interceptor before the handler runs (every verb here is userOnly); it is
// still handled fail-closed (nothing, no owner) so a future policy
// regression cannot widen this face by accident.
func executorAuthority(ctx context.Context) (all bool, userID string) {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		return true, ""
	}
	if !id.IsUserCredential() {
		return false, ""
	}
	if id.IsAdmin {
		return true, ""
	}
	return false, id.UserID
}

// refuseForeignExecutor is the shared guard behind Label, Disable, Enable and
// Delete: the executor is resolved FIRST (durable store row, or — for a
// transient session executor, which has no row at all — the live pool's
// in-memory row, whose OwnerUserID the session ticket stamped), and a scoped
// caller who does not own it is refused before the Controller method runs, so
// a foreign row is never read-modified even in memory. An executor that
// resolves to nothing falls through: the downstream Controller call reports
// its own not-found rather than this guard inventing a second one.
func (a connectExecutorAdmin) refuseForeignExecutor(ctx context.Context, executorID string) error {
	all, userID := executorAuthority(ctx)
	if all {
		return nil
	}
	e, ok := a.c.executorForOwnerCheck(ctx, executorID)
	if !ok || e.OwnerUserID == userID {
		return nil
	}
	return &connectapi.ControllerError{
		Code:    protocol.ErrPermissionDenied,
		Message: fmt.Sprintf("executor %s belongs to another user", shortID(e.ID)),
	}
}

// executorListDefaultLimit and executorListMaxLimit are ListExecutors' limit
// contract: the proto comment says `0 = default`, and this face serves the
// management listing, so `Limit <= 0 → 50` and the cap live here.
const (
	executorListDefaultLimit = 50
	executorListMaxLimit     = 500
)

// Enroll mints a one-time enrollment token. The owner rides the CONTEXT, never
// the request: spawnOwner maps the proxy face's authenticated identity — nil
// (UDS local trust) becomes users.Identity{}, whose owner then resolves to the
// daemon's own OS user — and the request has no owner field and must never
// grow one. The
// same rule guards the labels: executorTrustLabels refuses a request carrying
// `owner` (or `machine`, which --name owns).
func (a connectExecutorAdmin) Enroll(
	ctx context.Context,
	req *rafikiv1.EnrollExecutorRequest,
) (*rafikiv1.EnrollExecutorResponse, error) {
	resp, err := a.c.ExecutorEnroll(spawnOwner(ctx), protocol.ExecutorEnrollRequest{
		Name:          req.GetName(),
		Labels:        req.GetLabels(),
		Roots:         req.GetRoots(),
		Isolation:     req.GetIsolation(),
		WorkspaceMode: req.GetWorkspaceMode(),
		Admits:        req.GetAdmits(),
		TTL:           req.GetTtl().AsDuration(),
	})
	if err != nil {
		return nil, err
	}
	return &rafikiv1.EnrollExecutorResponse{Token: resp.Token}, nil
}

// Create mints an executor row and its durable credential in one step. Same
// owner rule as Enroll.
func (a connectExecutorAdmin) Create(
	ctx context.Context,
	req *rafikiv1.CreateExecutorRequest,
) (*rafikiv1.CreateExecutorResponse, error) {
	resp, err := a.c.ExecutorCreate(spawnOwner(ctx), protocol.ExecutorCreateRequest{
		Name:          req.GetName(),
		Labels:        req.GetLabels(),
		Roots:         req.GetRoots(),
		Isolation:     req.GetIsolation(),
		WorkspaceMode: req.GetWorkspaceMode(),
		Admits:        req.GetAdmits(),
	})
	if err != nil {
		return nil, err
	}
	return &rafikiv1.CreateExecutorResponse{ExecutorId: resp.ExecutorID, Credential: resp.Credential}, nil
}

func (a connectExecutorAdmin) Label(
	ctx context.Context,
	req *rafikiv1.LabelExecutorRequest,
) (connectapi.ExecutorRow, error) {
	if err := a.refuseForeignExecutor(ctx, req.GetExecutorId()); err != nil {
		return connectapi.ExecutorRow{}, err
	}
	e, err := a.c.ExecutorLabel(protocol.ExecutorLabelRequest{
		ExecutorID: req.GetExecutorId(),
		Set:        req.GetSet(),
		Remove:     req.GetRemove(),
	})
	if err != nil {
		return connectapi.ExecutorRow{}, err
	}
	return executorRowFrom(e), nil
}

func (a connectExecutorAdmin) Disable(ctx context.Context, executorID string) error {
	if err := a.refuseForeignExecutor(ctx, executorID); err != nil {
		return err
	}
	return a.c.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: executorID})
}

func (a connectExecutorAdmin) Enable(ctx context.Context, executorID string) error {
	if err := a.refuseForeignExecutor(ctx, executorID); err != nil {
		return err
	}
	return a.c.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: executorID})
}

func (a connectExecutorAdmin) Delete(ctx context.Context, executorID string) error {
	if err := a.refuseForeignExecutor(ctx, executorID); err != nil {
		return err
	}
	return a.c.ExecutorDelete(protocol.ExecutorDeleteRequest{ExecutorID: executorID})
}

// List is the plain listing behind ListExecutors with an empty kind. The limit
// is normalized to the framed contract BEFORE the Controller sees it, because
// Controller.ExecutorList itself treats 0 as "no cap" — the framed default of
// 50 and the 500 clamp live in its dispatcher, and this face must not turn a
// client that omitted the field into an unbounded listing.
//
// The result is scoped to the caller: executorAuthority decides whether the
// caller sees everything (operator socket, admin) or only rows it owns, and
// the filter runs over the merged row set — durable rows and the live pool's
// transient session executors alike — so a user's listing is exactly their
// fleet. The limit is applied by Controller.ExecutorList to the UNFILTERED
// set, so a scoped caller may see fewer rows than the limit asked for; a
// filter may only shrink what a caller sees, never the frame's ceiling.
func (a connectExecutorAdmin) List(ctx context.Context, selector string, limit int32) ([]connectapi.ExecutorRow, error) {
	switch {
	case limit <= 0:
		limit = executorListDefaultLimit
	case limit > executorListMaxLimit:
		limit = executorListMaxLimit
	}
	execs, err := a.c.ExecutorList(protocol.ExecutorListRequest{Selector: selector, Limit: int(limit)})
	if err != nil {
		return nil, err
	}
	all, userID := executorAuthority(ctx)
	out := make([]connectapi.ExecutorRow, 0, len(execs))
	for _, e := range execs {
		if !all && e.OwnerUserID != userID {
			continue
		}
		out = append(out, executorRowFrom(e))
	}
	return out, nil
}

// executorRowFrom maps an executor store row onto the connectapi face's row.
// The field mapping duplicates Controller.ListExecutorRows' inline conversion
// rather than calling it: that one is fused to the live pool's Describe (for
// launch kinds) and to an eligibility evaluation, which a plain listing must
// not perform. What a plain listing leaves ZERO is deliberate, and pinned by
// TestExecutorAdminListLeavesEligibilityUnevaluated: LaunchKinds is a
// live-pool observation the store row does not carry, and Eligible/Reason are
// unset on this path by ListExecutorsRequest's contract. The timestamps ride
// the row's own fields: ConnectedAt from the live pool's join time (the zero
// time when the executor has no current connection) and LastSeen from the
// store's last_seen_at (the zero time when the row has never been seen).
func executorRowFrom(e executors.Executor) connectapi.ExecutorRow {
	return connectapi.ExecutorRow{
		ID:            e.ID,
		Machine:       e.Labels["machine"],
		Labels:        e.Labels,
		Isolation:     e.Isolation,
		WorkspaceMode: e.WorkspaceMode,
		Roots:         e.Roots,
		Admits:        e.Admits,
		Enabled:       e.Enabled,
		Connected:     e.Connected,
		ConnectedAt:   timeOrZero(e.ConnectedAt),
		LastSeen:      e.LastSeenAt,
	}
}

// timeOrZero returns the pointed-to time, or the zero time when the pointer is
// nil. The zero time is the wire's "absent" here: the store leaves LastSeenAt at
// its zero value when last_seen_at is NULL (an executor never seen), and
// Controller.ExecutorList leaves ConnectedAt nil for an executor with no live
// connection.
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
