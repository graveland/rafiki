// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	adminpb "go.graveland.dev/rafiki/pkg/adminpb"
	darajapb "go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/darajapool"
	execpoolv1 "go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ─── DarajaDependencies & wiring ──────────────────────────────────────────────

// DarajaDependencies carries the pool-level dependencies needed by the three
// Daraja RPC handlers. Attached once at daemon startup after the controller is
// built — the same post-construction pattern used for events, lineage, etc.
type DarajaDependencies struct {
	ExecPool   *execpoolv1.Pool
	DarajaReg  *darajapool.Registry
	DarajaPool *darajapool.Pool
	DialAddr   string // Unix socket path or TCP host:port where daraja dials back
}

// SetDaraja attaches the dependencies needed by the three Daraja RPCs.
func (s *Server) SetDaraja(deps DarajaDependencies) {
	var reg *darajapool.Registry
	if deps.DarajaReg != nil {
		reg = deps.DarajaReg
	}
	h := &darajaHandlers{
		execPool:   deps.ExecPool,
		darajaReg:  reg,
		darajaPool: deps.DarajaPool,
		dialAddr:   deps.DialAddr,
	}
	s.daraja.Store(&h)
}

// darajaHandlers holds the runtime dependencies for DarajaLaunch/Send/Watch.
type darajaHandlers struct {
	execPool   *execpoolv1.Pool
	darajaReg  *darajapool.Registry
	darajaPool *darajapool.Pool
	dialAddr   string
}

// darajaLoad returns the configured handlers, or nil when no pool is wired.
func (s *Server) darajaHandlers() *darajaHandlers {
	if p := s.daraja.Load(); p != nil {
		return *p
	}
	return nil
}

// describeHasLaunchKind reports whether desc declares kind among its LaunchKinds.
func describeHasLaunchKind(desc *executorpb.DescribeResponse, kind string) bool {
	if desc == nil {
		return false
	}
	return slices.Contains(desc.LaunchKinds, kind)
}

// explainNoMatch builds a refusal diagnostic naming the excluding predicate
// per candidate — the same shape cmd/rafikid/executor_select.go uses for spawn.
func explainNoMatch(labelSelector string, liveExecs []execpoolv1.LiveExecutor,
	candidates []execpoolv1.LiveExecutor) error {
	sel, _ := executors.ParseSelector(labelSelector)
	var b strings.Builder
	fmt.Fprintf(&b, "launch refused: no executor satisfies %q.\n", labelSelector)
	fmt.Fprintf(&b, "  %d live executor(s), %d with launch kind:\n",
		len(liveExecs), len(candidates))
	for _, le := range candidates {
		e := le.Executor
		reason := ""
		if !e.Enabled {
			reason = "disabled"
		} else if _, err := executors.ParseSelector(e.Admits); err != nil {
			reason = fmt.Sprintf("unparseable admission selector %q", e.Admits)
		}
		// If we got here, admission passed — proceed to launch kind check.
		if reason == "" && !describeHasLaunchKind(le.Describe, "claude") {
			reason = "does not support launching claude children"
		}
		if reason == "" {
			explain := sel.Explain(e.Labels)
			if explain != "" {
				reason = fmt.Sprintf("excluded by your selector:   %s", explain)
			} else {
				reason = "excluded by your selector"
			}
		}
		id := e.ID
		if len(id) > 12 {
			id = id[:12]
		}
		fmt.Fprintf(&b, "    %-12s  %s\n", id, reason)
	}
	return errors.New(b.String())
}

// ─── DarajaLaunch handler ─────────────────────────────────────────────────────

func (s *Server) DarajaLaunch(
	ctx context.Context,
	req *connect.Request[rafikiv1.DarajaLaunchRequest],
) (*connect.Response[rafikiv1.DarajaLaunchResponse], error) {
	h := s.darajaHandlers()
	if h == nil || h.execPool == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor pool not configured"))
	}
	if h.darajaReg == nil || h.darajaPool == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("daraja pool not configured"))
	}
	if h.dialAddr == "" {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("dial address not set"))
	}

	spec := req.Msg.GetSpec()
	if spec == nil || spec.Claude == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("spec.claude is required"))
	}

	selStr := req.Msg.GetExecutorSelector()
	sel, err := executors.ParseSelector(selStr)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("invalid executor selector %q: %w", selStr, err))
	}

	allLive := h.execPool.Live()
	var withLaunchKind []execpoolv1.LiveExecutor
	for _, le := range allLive {
		if describeHasLaunchKind(le.Describe, "claude") {
			withLaunchKind = append(withLaunchKind, le)
		}
	}

	candidates := withLaunchKind
	if selStr != "" {
		var narrowed []execpoolv1.LiveExecutor
		for _, le := range candidates {
			if sel.Matches(le.Executor.Labels) {
				narrowed = append(narrowed, le)
			}
		}
		candidates = narrowed
	}

	// Dial transport check. When dialAddr names a Unix socket, daraja must be
	// local — only an executor that connected over UDS can reach the same path.
	// A TCP-enrolled remote executor's launch would fail silently.
	isUDS := !strings.Contains(h.dialAddr, ":") && strings.HasPrefix(h.dialAddr, "/")
	if isUDS {
		var udsCandidates []execpoolv1.LiveExecutor
		for _, le := range candidates {
			if h.execPool.IsEnrolledViaUDS(le.Executor.ID) {
				udsCandidates = append(udsCandidates, le)
			}
		}
		candidates = udsCandidates
		if len(candidates) == 0 && len(withLaunchKind) > 0 {
			// All executors had the launch kind but none enrolled via UDS.
			// explainNoMatch would mislead by listing them; produce a
			// transport-specific diagnostic instead.
			var b strings.Builder
			fmt.Fprintf(&b, "launch refused: no executor satisfies %q.\n", selStr)
			fmt.Fprintf(&b, "  %d live executor(s), %d with launch kind,\n",
				len(allLive), len(withLaunchKind))
			fmt.Fprintf(&b, "  none enrolled over unix domain socket (needed when "+
				"dial address is a local path). Set RAFIKI_CONTROL_LISTEN on this "+
				"daemon to advertise a TCP address instead.\n")
			return nil, connect.NewError(connect.CodeUnavailable, errors.New(b.String()))
		}
	}

	if len(candidates) == 0 {
		return nil, explainNoMatch(selStr, allLive, withLaunchKind)
	}

	le := candidates[0]

	childID := "c_" + fmt.Sprintf("%x", uint64(time.Now().UnixNano()%999999999999999999))

	ticket, err := h.darajaReg.MintTicket(childID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("mint ticket: %w", err))
	}

	adminCLI, err := h.execPool.AdminClientFor(le.Executor.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("no admin client for executor %s: %w", le.Executor.ID, err))
	}

	launchReq := &adminpb.LaunchRequest{
		ChildId:  childID,
		Cwd:      req.Msg.GetCwd(),
		Spec:     spec,
		DialAddr: h.dialAddr,
		Ticket:   ticket,
	}

	launchResp, err := adminCLI.Launch(ctx, connect.NewRequest(launchReq))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("AdminService.Launch on executor %s: %w", le.Executor.ID, err))
	}

	// Wait for the daraja to reverse-dial back into the pool.
	connected := make(chan struct{})
	done := false
	cb := func(cid string) {
		if cid == childID && !done {
			done = true
			close(connected)
		}
	}
	h.darajaPool.OnConnect(cb)

	select {
	case <-connected:
	case <-time.After(30 * time.Second):
		h.darajaPool.Evict(childID)
		return nil, connect.NewError(connect.CodeDeadlineExceeded,
			errors.New("daraja did not connect within 30s"))
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return connect.NewResponse(&rafikiv1.DarajaLaunchResponse{
		ChildId:         childID,
		Pid:             int32(launchResp.Msg.GetPid()),
		Pgid:            int32(launchResp.Msg.GetPgid()),
		ConnectedUnixMs: time.Now().UnixMilli(),
	}), nil
}

// ─── DarajaSend handler ───────────────────────────────────────────────────────

func (s *Server) DarajaSend(
	ctx context.Context,
	req *connect.Request[rafikiv1.DarajaSendRequest],
) (*connect.Response[rafikiv1.DarajaSendResponse], error) {
	h := s.darajaHandlers()
	if h == nil || h.darajaPool == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("daraja pool not configured"))
	}

	childID := req.Msg.GetChildId()
	data := req.Msg.GetData()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}

	// Use the pool's Relay holder — one stream per child, shared between
	// Send and Watch. No drain goroutine: the holder's receive loop already
	// consumes everything from the stream.
	if err := h.darajaPool.Send(childID, data); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("send to daraja %s: %w", childID, err))
	}

	return connect.NewResponse(&rafikiv1.DarajaSendResponse{Acknowledged: true}), nil
}

// ─── DarajaWatch handler ──────────────────────────────────────────────────────

func (s *Server) DarajaWatch(
	ctx context.Context,
	req *connect.Request[rafikiv1.DarajaWatchRequest],
	stream *connect.ServerStream[rafikiv1.DarajaWatchResponse],
) error {
	h := s.darajaHandlers()
	if h == nil || h.darajaPool == nil {
		return connect.NewError(connect.CodeUnavailable,
			errors.New("daraja pool not configured"))
	}

	childID := req.Msg.GetChildId()
	if childID == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}

	subCh, unsub, err := h.darajaPool.Watch(childID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound,
			fmt.Errorf("watch daraja %s: %w", childID, err))
	}
	defer unsub()

	for {
		select {
		case ev, ok := <-subCh:
			if !ok {
				// Holder was torn down (disconnection).
				return connect.NewError(connect.CodeUnavailable,
					errors.New("daraja connection lost"))
			}
			if ev.Err() != nil {
				return connect.NewError(connect.CodeUnavailable,
					fmt.Errorf("relay error: %w", ev.Err()))
			}

			watchResp := &rafikiv1.DarajaWatchResponse{}
			switch respEv := ev.Response().GetEvent().(type) {
			case *darajapb.RelayResponse_Stdout:
				watchResp.Event = &rafikiv1.DarajaWatchResponse_Stdout{Stdout: respEv.Stdout}
			case *darajapb.RelayResponse_Restarted:
				watchResp.Event = &rafikiv1.DarajaWatchResponse_Restarted{
					Restarted: &rafikiv1.DarajaProcessRestarted{Pid: respEv.Restarted.Pid},
				}
			case *darajapb.RelayResponse_Exited:
				watchResp.Event = &rafikiv1.DarajaWatchResponse_Exited{
					Exited: &rafikiv1.DarajaProcessExited{
						ExitCode: respEv.Exited.ExitCode,
						Signal:   respEv.Exited.Signal,
					},
				}
			default:
				continue
			}

			if err := stream.Send(watchResp); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
