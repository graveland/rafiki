// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	adminpb "go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/adminpb/adminpbconnect"
	"go.graveland.dev/rafiki/pkg/darajapb"
)

// defaultLaunchTimeout bounds how long Launch waits for the daraja's reverse
// dial to land. 30s leaves headroom for a cold `docker run`-backed executor
// without leaving a caller (an RPC handler or a spawn request) hanging
// indefinitely on a machine that never dials back.
const defaultLaunchTimeout = 30 * time.Second

// statusPollInterval is how often the connect wait asks the executor's
// AdminService.Status whether the daraja has already died, alongside the
// connect signal. An exited daraja ends the wait at once instead of burning
// the whole timeout.
const statusPollInterval = time.Second

// statusCallTimeout bounds one Status call inside the wait. It exists so a
// stalled executor (it was alive when Launch was accepted; it may not be a
// second later) cannot wedge the wait's select — an arm that never returns
// would keep the connected arm from firing even though the daraja dialled in.
const statusCallTimeout = 2 * time.Second

// adminLauncher is the slice of *execpool.Pool that Launch needs — an
// interface for the same reason cmd/rafikid/executor_select.go's
// executorPool is one: testable without a listener, a database, or a
// dialling executor. *execpool.Pool satisfies this with no changes.
type adminLauncher interface {
	AdminClientFor(executorID string) (adminpbconnect.AdminServiceClient, error)
}

// LaunchParams is everything Launch needs. The executor is already CHOSEN by
// the caller — see this file's doc comment on why selection stays out of
// Launch itself.
type LaunchParams struct {
	ExecPool   adminLauncher
	Pool       *Pool
	Registry   *Registry
	DialAddr   string
	ExecutorID string
	ChildID    string
	Cwd        string
	Spec       *darajapb.ChildSpec
	// Timeout overrides defaultLaunchTimeout. Zero means the default; tests
	// set this short to avoid a real 30s wait on the failure path.
	Timeout time.Duration
}

type LaunchResult struct {
	Pid  int32
	Pgid int32
}

// Launch mints a one-shot ticket, asks the chosen executor's AdminService to
// start a daraja, and blocks until that daraja's reverse dial lands in Pool.
// Alongside the connect signal the wait polls AdminService.Status every
// second: a daraja that exits before connecting (its reverse dial will never
// come) ends the wait at once with its exit code and stderr tail, while a
// Status RPC that fails for any reason — an old executor without the RPC
// answers Unimplemented — is logged once and the wait continues on the signal
// and the timeout as before.
//
// The executor itself is already chosen by the caller. DarajaLaunch's
// operator-facing selection (bare label match, no admission/lineage
// narrowing) and rafikid's own confinement-aware chooseLaunchExecutor pick
// executors by two genuinely different algorithms; baking either one into
// Launch would force the other caller onto the wrong one. Launch only does
// the mechanical part both callers need identically: ticket,
// AdminService.Launch, wait for the reverse dial.
func Launch(ctx context.Context, p LaunchParams) (LaunchResult, error) {
	if p.ExecPool == nil || p.Pool == nil || p.Registry == nil {
		return LaunchResult{}, errors.New("darajapool: Launch requires ExecPool, Pool and Registry")
	}
	if p.DialAddr == "" {
		return LaunchResult{}, errors.New("darajapool: Launch requires a non-empty DialAddr")
	}
	if p.Spec == nil || (p.Spec.GetClaude() == nil && p.Spec.GetScript() == nil) {
		return LaunchResult{}, errors.New("darajapool: Launch requires spec.claude or spec.script")
	}

	ticket, err := p.Registry.MintTicket(p.ChildID)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("mint ticket: %w", err)
	}

	adminCLI, err := p.ExecPool.AdminClientFor(p.ExecutorID)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("no admin client for executor %s: %w", p.ExecutorID, err)
	}

	// Register BEFORE issuing AdminService.Launch, not after: the reverse
	// dial races the RPC response's own return to this goroutine (the
	// executor starts the daraja process and answers the RPC independently
	// of when daraja actually dials back), so a callback registered after
	// the call returns can lose that race on a fast local loop and wait out
	// the full timeout despite the daraja having connected within
	// milliseconds. See TestLaunchReturnsPidPgidOnceTheReverseDialArrives,
	// which reproduced this the first time it was written with the fake
	// executor firing OnConnect from inside its own Launch stub.
	//
	// Unsubscribed unconditionally on return (defer, below every branch this
	// call can exit through): this registration is one-shot for exactly this
	// ChildID, and every claude spawn creates one, so leaving it registered
	// after Launch returns would grow Pool.onConnect by one per spawn forever
	// and cost every SUBSEQUENT daraja connect one dead call per prior spawn.
	connected := make(chan struct{})
	var fired bool
	unsubscribe := p.Pool.OnConnect(func(cid string) {
		if cid == p.ChildID && !fired {
			fired = true
			close(connected)
		}
	})
	defer unsubscribe()

	launchResp, err := adminCLI.Launch(ctx, connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  p.ChildID,
		Cwd:      p.Cwd,
		Spec:     p.Spec,
		DialAddr: p.DialAddr,
		Ticket:   ticket,
	}))
	if err != nil {
		return LaunchResult{}, fmt.Errorf("AdminService.Launch on executor %s: %w", p.ExecutorID, err)
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultLaunchTimeout
	}

	// The wait arms: the daraja's reverse dial, the overall timeout (one timer
	// created here, so it is measured from the start of the wait and is never
	// reset by the Status polling below), the caller's context, and a 1s poll
	// of the executor's AdminService.Status — a daraja that died before
	// connecting ends the wait at once with the daraja's own stderr instead of
	// burning the full timeout on a machine that will never dial back.
	timeoutC := time.After(timeout)
	ticker := time.NewTicker(statusPollInterval)
	defer ticker.Stop()

	// A Status RPC error is reported once, not once per tick: an executor that
	// predates the RPC (Unimplemented) or one that just died mid-wait must not
	// spray a warning every second for the rest of the wait.
	statusErrLogged := false

	for {
		select {
		case <-connected:
			return LaunchResult{
				Pid:  int32(launchResp.Msg.GetPid()),
				Pgid: int32(launchResp.Msg.GetPgid()),
			}, nil
		case <-timeoutC:
			p.Pool.Evict(p.ChildID)
			return LaunchResult{}, fmt.Errorf("daraja for child %s did not connect within %s", p.ChildID, timeout)
		case <-ctx.Done():
			return LaunchResult{}, ctx.Err()
		case <-ticker.C:
			sctx, cancel := context.WithTimeout(ctx, statusCallTimeout)
			status, err := adminCLI.Status(sctx, connect.NewRequest(&adminpb.StatusRequest{ChildId: p.ChildID}))
			cancel()
			if err != nil {
				// Old executors answer Unimplemented; a just-died executor answers
				// nothing at all. Both mean "keep waiting" — the connect signal and
				// the timeout remain the authorities.
				if !statusErrLogged {
					statusErrLogged = true
					slog.Warn("daraja launch: Status poll failed; waiting on the connect signal",
						"childID", p.ChildID, "executor", p.ExecutorID, "error", err)
				}
				continue
			}
			if st := status.Msg; st.GetKnown() && !st.GetRunning() {
				p.Pool.Evict(p.ChildID)
				return LaunchResult{}, fmt.Errorf("daraja for child %s exited (code %d) before connecting: %s",
					p.ChildID, st.GetExitCode(), st.GetStderrTail())
			}
			// Known and still running: keep waiting for the dial.
		}
	}
}
