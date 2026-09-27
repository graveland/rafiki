package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/version"
)

// sessionReadyTimeout bounds the wait for the executor's row to report
// connected.
//
// A spawn issued before then fails: chooseExecutor matches only LIVE executors.
// Generous rather than tight — a first connection to a remote daemon includes a
// TLS handshake and an enrollment round trip, and the cost of waiting too long
// is a slow start where the cost of waiting too little is a confusing refusal.
const sessionReadyTimeout = 20 * time.Second

// sessionCleanupJoinCap bounds startSessionExecutor's wait for its two
// goroutines (the execpool connection and the stream watch) when the session
// ends. W4a review finding (MAJOR): execpool.Connect's remote dial is not
// context-aware — tls.DialWithDialer performs its own handshake with no
// deadline — so on a partitioned or black-holed remote daemon that goroutine
// can stay stuck for the OS TCP connect timeout (~75s) or, if the peer accepts
// TCP but never speaks TLS, indefinitely. Without this cap, the operator
// quitting the cockpit or hitting Ctrl-C (whose deferred cleanup runs here)
// blocks on the join for that long. Past the cap the join is abandoned, not
// completed: the goroutines die with the process. The WaitGroup itself stays
// so the join — and anything observing it under -race — remains meaningful.
const sessionCleanupJoinCap = 3 * time.Second

// boundedJoin waits for wg, but never longer than cap. Past the cap it
// returns with the join still outstanding; the goroutines it was waiting on
// are left to die with the process.
func boundedJoin(wg *sync.WaitGroup, cap time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cap):
	}
}

// sessionConnectTarget decides where this client's executor dials.
//
// Derived from the resolved PROFILE rather than configured, so it always
// reaches the same daemon the control connection did. An executor pointed at a
// different daemon than the spawn would serve a filesystem nobody asked for.
func sessionConnectTarget(p profile.Resolved) (addr, socket string, err error) {
	if p.URL != "" {
		a, err := dialAddr(p.URL)
		if err != nil {
			return "", "", err
		}
		return a, "", nil
	}
	return "", paths.ExecutorSocketPath(), nil
}

// sessionConnectEndpoint resolves the Connect control-plane endpoint for an
// already-resolved profile, mirroring newConnectEndpoint's own local/remote
// branches (connectclient.go). startSessionExecutor runs after profile
// resolution has already happened, with no *cobra.Command to hand
// newConnectEndpoint — the CLI's usual single place that resolves an
// endpoint — so this is the one other place that does it, from a
// profile.Resolved instead of a command.
//
// The remote branch mirrors newConnectEndpoint's no-token guard too: this
// plane has no bootstrap mode, so an absent credential can only ever produce
// a bare Unauthenticated after a round trip, and naming the missing token
// file here beats that (W4a minor). (Factoring one shared
// connectEndpointFromProfile is parked in todo.md.)
func sessionConnectEndpoint(p profile.Resolved) (connectEndpoint, error) {
	if p.URL == "" {
		sock := p.Socket
		httpClient := connectHTTPClient(sock)
		// Optional, unlike the remote branch below: the socket itself is
		// the trust boundary, and a local profile with no token must keep
		// working for every verb that never looks at identity.
		if p.Token != "" {
			httpClient = &http.Client{Transport: &bearerTransport{base: httpClient.Transport, token: p.Token}}
		}
		return connectEndpoint{
			httpClient: httpClient,
			baseURL:    connectUDSBaseURL,
			describe:   sock,
			identity:   "unix:" + sock,
		}, nil
	}
	if p.Token == "" {
		return connectEndpoint{}, fmt.Errorf(
			"profile %q names a remote daemon but has no token: write one to %s "+
				"(or recreate it with `rafiki profile add %s --url %s --token …`)",
			p.Name, profile.TokenFile(p.Name), p.Name, p.URL)
	}
	return connectEndpoint{
		httpClient: &http.Client{Transport: &bearerTransport{base: http.DefaultTransport, token: p.Token}},
		baseURL:    p.URL,
		describe:   p.URL,
		identity:   p.URL,
	}, nil
}

// executorEnvURL is remoteDialURL's surviving half, kept local to the executor
// path on purpose.
//
// `rafiki executor serve` and `executor service install` run on headless boxes
// configured by executor.env, where there may be no interactive profile at all
// — so they are exempt from profile resolution and from
// profile.CheckRetiredEnv, and they still derive --connect from RAFIKI_URL.
// Do NOT "unify" this with the profile: bootstrapping a default profile
// pointing at a local socket no daemon is listening on would be worse than
// useless there.
func executorEnvURL() string {
	u := paths.Get(paths.URL)
	if isRemoteURL(u) {
		return u
	}
	return ""
}

// resolveExecutorConnectFlags applies the same "derive from RAFIKI_URL"
// default sessionConnectTarget used to give the automatic in-process executor
// to a durable executor's explicit --connect/--connect-socket flags
// (`rafiki executor serve`, `rafiki executor service install`) — an operator
// who already has RAFIKI_URL set everywhere else shouldn't have to separately
// derive and pass host:port by hand, and getting that derivation wrong by hand
// is exactly how a durable executor ends up pointed at the wrong port.
//
// This is the ONE place left that still reads RAFIKI_URL directly
// (executorEnvURL): these two headless commands may run on a machine with no
// profile configured at all, so they stay exempt from profile resolution.
//
// The default only applies when NEITHER flag was given: an explicit
// --connect-socket already names the local-socket transport deliberately,
// and must not be overridden by a RAFIKI_URL-derived --connect layered on
// top of it. An explicit --connect likewise always wins verbatim.
func resolveExecutorConnectFlags(connect, connectSocket string) (string, string, error) {
	if connect == "" && connectSocket == "" {
		if u := executorEnvURL(); u != "" {
			addr, err := dialAddr(u)
			if err != nil {
				return "", "", fmt.Errorf("derive --connect from RAFIKI_URL: %w", err)
			}
			connect = addr
		}
	}

	modes := 0
	for _, set := range []bool{connect != "", connectSocket != ""} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return "", "", fmt.Errorf("--connect and --connect-socket are mutually exclusive")
	}
	if modes == 0 {
		return "", "", fmt.Errorf("one of --connect or --connect-socket is required (or set RAFIKI_URL)")
	}
	return connect, connectSocket, nil
}

// waitExecutorLive polls ListExecutors until an enabled, connected executor
// matches selector, or sessionReadyTimeout elapses. chooseExecutor matches only
// LIVE executors, so a spawn sent before this returns fails.
//
// It must be called with the SESSION context, not the caller's: when the
// daemon ends the ExecutorSession stream before the executor reports live, the
// watch goroutine cancels that context, and this then surfaces the stream's
// end (streamEnded) instead of a misleading "timed out waiting for executor
// to connect" (W4a minor). streamEnded carries the watch goroutine's
// classification of how the stream ended; when nothing was recorded, the
// context's own error is returned.
func waitExecutorLive(ctx context.Context, cc rafikiv1connect.ControlClient, selector string, streamEnded <-chan error) error {
	deadline := time.Now().Add(sessionReadyTimeout)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := executorLive(ctx, cc, selector)
		if err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			select {
			case why := <-streamEnded:
				return fmt.Errorf("executor session for %q ended before the executor connected: %w", selector, why)
			default:
				return ctx.Err()
			}
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for executor %q to connect; a spawn issued now would fail to find it",
				sessionReadyTimeout, selector)
		}
	}
}

// executorLive reports whether an enabled, connected executor matching
// selector is in the daemon's list. ListExecutors reads the store and marks
// Connected from the live pool, so both flags together mean the executor is
// up and usable.
func executorLive(ctx context.Context, cc rafikiv1connect.ControlClient, selector string) (bool, error) {
	resp, err := cc.ListExecutors(ctx, connect.NewRequest(&rafikiv1.ListExecutorsRequest{
		Selector: selector,
	}))
	if err != nil {
		return false, err
	}
	for _, e := range resp.Msg.GetRows() {
		if e.GetEnabled() && e.GetConnected() {
			return true, nil
		}
	}
	return false, nil
}

// childCwd returns the named child's working directory as the daemon reports
// it, or "" when it cannot be resolved. Attach uses it as the workspace root so
// the machine offers the directory the child is actually working in.
//
//nolint:unused
func childCwd(ctx context.Context, cc rafikiv1connect.ControlClient, childID string) string {
	resp, err := cc.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		return ""
	}
	return resp.Msg.GetChild().GetCwd()
}

// startSessionExecutor makes this machine available as a workspace.
//
// It returns the selector to put on spawns. That selector is returned even when
// no executor was started: the daemon answers with an empty ticket when a
// durable executor already covers this machine and owner, and using it is the
// point — that executor outlives this process, so an agent keeps working after
// the operator closes the terminal.
//
// The session's lifetime rides the ExecutorSession Connect stream, opened
// with a context that lives exactly as long as the session: the returned
// cleanup cancels it, which is the daemon's eviction trigger. The endpoint
// resolves from the same profile the caller's spawn uses (sessionConnectEndpoint),
// so the function needs no command and no connection of its own.
func startSessionExecutor(ctx context.Context, root string, p profile.Resolved) (string, func(), error) {
	noop := func() {}

	name, _, err := paths.MachineName()
	if err != nil {
		return "", noop, err
	}

	ep, err := sessionConnectEndpoint(p)
	if err != nil {
		return "", noop, err
	}
	cc := ep.control()

	sessionCtx, cancel := context.WithCancel(ctx)
	// streamEnded records how the ExecutorSession stream ended, so the
	// liveness wait can surface an eviction instead of a timeout (see
	// waitExecutorLive). Buffered, and written before the write also tears
	// the session down, so a reader woken by that teardown always sees the
	// value.
	streamEnded := make(chan error, 1)

	stream, err := cc.ExecutorSession(sessionCtx, connect.NewRequest(&rafikiv1.ExecutorSessionRequest{
		Name:  name,
		Roots: []string{root},
	}))
	if err != nil {
		cancel()
		return "", noop, diagnoseConnectError(err, ep.describe)
	}
	if !stream.Receive() {
		cancel()
		if err := stream.Err(); err != nil {
			return "", noop, diagnoseConnectError(err, ep.describe)
		}
		return "", noop, fmt.Errorf("executor session: stream at %s ended before sending a ready event", ep.describe)
	}
	ready := stream.Msg().GetReady()
	if ready == nil {
		cancel()
		return "", noop, errors.New("executor session: unexpected event before ready")
	}

	if !ready.GetRunLocal() {
		// A durable executor already covers this machine. Use it: it
		// outlives this process, so an agent keeps working after the
		// operator detaches. Nothing was started, so there is nothing this
		// session needs to keep the stream open for.
		cancel()
		return ready.GetSelector(), noop, nil
	}

	addr, socket, err := sessionConnectTarget(p)
	if err != nil {
		cancel()
		return "", noop, err
	}

	srv := executor.NewServer(executor.Options{
		Root:    root,
		Version: version.String(),
		RTK:     tools.RTKAuto,
	})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			_ = srv.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := execpool.Connect(sessionCtx, execpool.ConnectOptions{
			Addr:       addr,
			SocketPath: socket,
			Ticket:     ready.GetTicket(),
			SelfReported: map[string]string{
				"version": version.String(),
			},
			Handler: executorHandler(srv, nil),
		})
		switch {
		case err == nil, errors.Is(err, context.Canceled):
		case errors.Is(err, execpool.ErrEnrollmentRejected):
			// A ticket is one-shot and tied to this session's stream.
			// Rejection means the stream is gone or the ticket was spent,
			// and neither is recoverable by retrying.
			slog.Warn("this session's executor ticket was refused; the machine " +
				"is no longer offered as a workspace for this session")
		default:
			slog.Warn("this machine's executor stopped", "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		// ready is the only event shape this stream ever sends (see proto
		// ExecutorSessionEvent); this loop just watches for the stream to
		// end, which is the daemon's eviction signal.
		for stream.Receive() {
		}
		if sessionCtx.Err() != nil {
			// This session ended the stream itself (cleanup, or the parent
			// context going away): nothing to propagate, and warning would
			// misdescribe a self-inflicted end as an eviction.
			return
		}
		// The stream ended without this session asking it to: the daemon
		// evicted this executor. Classify the end once — a real stream
		// error, or a clean daemon-side end, which for this session is
		// still an eviction — share it with the liveness wait, and tear
		// the local executor down: the ticket that authenticated it
		// belonged to this stream and is now spent.
		var why error
		if err := stream.Err(); err != nil {
			why = err
		} else {
			why = errors.New("the daemon ended the session")
		}
		select {
		case streamEnded <- why:
		default:
		}
		slog.Warn("this machine's executor stopped", "error", why)
		stop()
	}()

	if err := waitExecutorLive(sessionCtx, cc, ready.GetSelector(), streamEnded); err != nil {
		stop()
		boundedJoin(&wg, sessionCleanupJoinCap)
		return "", noop, err
	}
	return ready.GetSelector(), func() {
		stop()
		// Bounded, not bare: the join can otherwise hang past the process's
		// lifetime on execpool.Connect's context-blind remote dial (see
		// sessionCleanupJoinCap, W4a major).
		boundedJoin(&wg, sessionCleanupJoinCap)
	}, nil
}
