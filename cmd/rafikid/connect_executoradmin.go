// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// connectExecutorAdmin adapts *Controller to connectapi.ExecutorAdmin — the
// operator-side executor verbs the framed ctrl_executor_enroll/create/label/
// disable/enable/delete faces served. A distinct type for the same reason
// connectModels is one: the Controller methods this delegates to already exist
// with the framed planes' signatures, and renaming them would touch every
// existing caller for no gain.
//
// Errors pass through UNMAPPED. The connectapi handler owns error mapping —
// executorAdminErr logs an uncoded cause (ConnectErr redacts it) and hands a
// ControllerError to ConnectErr with its authored message intact. Mapping here
// instead would hand the handler an already-mapped *connect.Error, which its
// own ConnectErr pass would re-classify as Internal and lose the code.
type connectExecutorAdmin struct{ c *Controller }

// executorListDefaultLimit and executorListMaxLimit are the framed
// ctrl_executor_list dispatcher's limit contract (pkg/control/dispatch.go:
// maxExecutorListLimit, and the `Limit <= 0 → 50` default). ListExecutors'
// proto comment says `0 = default`, and this face serves the same listing, so
// it carries the same contract. The numbers are duplicated here because the
// dispatcher's const is unexported; if the framed default ever moves, move it
// here with it.
const (
	executorListDefaultLimit = 50
	executorListMaxLimit     = 500
)

// Enroll mints a one-time enrollment token. The owner rides the CONTEXT, never
// the request: spawnOwner maps the proxy face's authenticated identity — nil
// (UDS local trust) becomes users.Identity{}, the same value connIdentity
// hands the framed dispatcher, whose owner then resolves to the daemon's own
// OS user — and the request has no owner field and must never grow one. The
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
		TTLSeconds:    req.GetTtlSeconds(),
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
	return a.c.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: executorID})
}

func (a connectExecutorAdmin) Enable(ctx context.Context, executorID string) error {
	return a.c.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: executorID})
}

func (a connectExecutorAdmin) Delete(ctx context.Context, executorID string) error {
	return a.c.ExecutorDelete(protocol.ExecutorDeleteRequest{ExecutorID: executorID})
}

// List is the plain listing behind ListExecutors with an empty kind. The limit
// is normalized to the framed contract BEFORE the Controller sees it, because
// Controller.ExecutorList itself treats 0 as "no cap" — the framed default of
// 50 and the 500 clamp live in its dispatcher, and this face must not turn a
// client that omitted the field into an unbounded listing.
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
	out := make([]connectapi.ExecutorRow, 0, len(execs))
	for _, e := range execs {
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
// the row's own fields: ConnectedAtMs from the live pool's join time (0 when
// the executor has no current connection) and LastSeenMs from the store's
// last_seen_at (0 when the row has never been seen).
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
		ConnectedAtMs: unixMs(e.ConnectedAt),
		LastSeenMs:    unixMs(&e.LastSeenAt),
	}
}

// unixMs renders an observation time as the wire's unix-ms encoding: a nil
// pointer or the zero time.Time is 0, the wire's "absent". The zero time is a
// meaningful state here, not a bug: the store leaves LastSeenAt at its zero
// value when last_seen_at is NULL (an executor never seen), and Controller.
// ExecutorList leaves ConnectedAt nil for an executor with no live
// connection. UnixMilli of the zero time would otherwise surface a year-1
// count no client can render.
func unixMs(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
