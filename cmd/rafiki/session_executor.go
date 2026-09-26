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

	"go.graveland.dev/rafiki/pkg/client"
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

// sessionConnectTarget decides where this client's executor dials.
//
// Derived from the resolved PROFILE rather than configured, so it always
// reaches the same daemon the control connection did. An executor pointed at a
// different daemon than the spawn would serve a filesystem nobody asked for.
func sessionConnectTarget(p profile.Resolved) (addr, socket string, err error) {
	if p.URL != "" {
		a, err := client.DialAddr(p.URL)
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
func sessionConnectEndpoint(p profile.Resolved) connectEndpoint {
	if p.URL == "" {
		sock := connectSocketFor(p)
		httpClient := connectHTTPClient(sock)
		if p.Token != "" {
			httpClient = &http.Client{Transport: &bearerTransport{base: httpClient.Transport, token: p.Token}}
		}
		return connectEndpoint{
			httpClient: httpClient,
			baseURL:    connectUDSBaseURL,
			describe:   sock,
			identity:   "unix:" + sock,
		}
	}
	return connectEndpoint{
		httpClient: &http.Client{Transport: &bearerTransport{base: http.DefaultTransport, token: p.Token}},
		baseURL:    p.URL,
		describe:   p.URL,
		identity:   p.URL,
	}
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
	if client.IsRemoteURL(u) {
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
			addr, err := client.DialAddr(u)
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
func waitExecutorLive(ctx context.Context, cc rafikiv1connect.ControlClient, selector string) error {
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
			return ctx.Err()
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

	ep := sessionConnectEndpoint(p)
	cc := ep.control()

	sessionCtx, cancel := context.WithCancel(ctx)

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
		if sessionCtx.Err() == nil {
			// The stream ended without this session asking it to: the
			// daemon evicted this executor. Surface it exactly the way a
			// dropped connection was surfaced before, and tear the local
			// executor down — the ticket that authenticated it belonged to
			// this stream and is now spent.
			if err := stream.Err(); err != nil {
				slog.Warn("this machine's executor stopped", "error", err)
			} else {
				slog.Warn("this machine's executor stopped: the daemon ended the session")
			}
			stop()
		}
	}()

	if err := waitExecutorLive(ctx, cc, ready.GetSelector()); err != nil {
		stop()
		wg.Wait()
		return "", noop, err
	}
	return ready.GetSelector(), func() {
		stop()
		wg.Wait()
	}, nil
}
