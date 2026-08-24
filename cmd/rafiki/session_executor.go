package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
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
// Derived from RAFIKI_URL rather than configured, so it always reaches the same
// daemon the control connection did. An executor pointed at a different daemon
// than the spawn would serve a filesystem nobody asked for.
func sessionConnectTarget() (addr, socket string, err error) {
	if u := remoteDialURL(); u != "" {
		a, err := client.DialAddr(u)
		if err != nil {
			return "", "", err
		}
		return a, "", nil
	}
	return "", paths.ExecutorSocketPath(), nil
}

// resolveExecutorConnectFlags applies the same "derive from RAFIKI_URL"
// default sessionConnectTarget already gives the automatic in-process
// executor to a durable executor's explicit --connect/--connect-socket
// flags (`rafiki executor serve`, `rafiki executor service install`) — an
// operator who already has RAFIKI_URL set everywhere else shouldn't have to
// separately derive and pass host:port by hand, and getting that derivation
// wrong by hand is exactly how a durable executor ends up pointed at the
// wrong port.
//
// The default only applies when NEITHER flag was given: an explicit
// --connect-socket already names the local-socket transport deliberately,
// and must not be overridden by a RAFIKI_URL-derived --connect layered on
// top of it. An explicit --connect likewise always wins verbatim.
func resolveExecutorConnectFlags(connect, connectSocket string) (string, string, error) {
	if connect == "" && connectSocket == "" {
		if u := remoteDialURL(); u != "" {
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

// requestSessionExecutor asks the daemon how this client should reach an
// executor that shares its filesystem.
func requestSessionExecutor(ctx context.Context, c *client.Client, root string) (protocol.ExecutorSessionResponseData, error) {
	name, _, err := paths.MachineName()
	if err != nil {
		return protocol.ExecutorSessionResponseData{}, err
	}
	req := protocol.ExecutorSessionRequest{
		Type:  protocol.TypeCtrlExecutorSession,
		Name:  name,
		Roots: []string{root},
	}
	resp, err := c.Request(ctx, req)
	if err != nil {
		return protocol.ExecutorSessionResponseData{}, err
	}
	if !resp.Success {
		return protocol.ExecutorSessionResponseData{}, fmt.Errorf("ctrl_executor_session: %s", client.FormatError(resp))
	}
	var data protocol.ExecutorSessionResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return protocol.ExecutorSessionResponseData{}, fmt.Errorf("malformed response: %w", err)
	}
	return data, nil
}

// waitExecutorLive polls the executor list until an enabled, connected executor
// matches selector, or sessionReadyTimeout elapses. chooseExecutor matches only
// LIVE executors, so a spawn sent before this returns fails.
func waitExecutorLive(ctx context.Context, c *client.Client, selector string) error {
	deadline := time.Now().Add(sessionReadyTimeout)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := executorLive(ctx, c, selector)
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

// executorLive reports whether an enabled, connected executor matching selector
// is in the daemon's list. The list reads the store and marks Connected from
// the live pool, so both flags together mean the executor is up and usable.
func executorLive(ctx context.Context, c *client.Client, selector string) (bool, error) {
	req := protocol.ExecutorListRequest{
		Type:     protocol.TypeCtrlExecutorList,
		Selector: selector,
	}
	resp, err := c.Request(ctx, req)
	if err != nil {
		return false, err
	}
	if !resp.Success {
		return false, fmt.Errorf("ctrl_executor_list: %s", client.FormatError(resp))
	}
	var wrapper struct {
		Executors []executors.Executor `json:"executors"`
	}
	if err := json.Unmarshal(resp.Data, &wrapper); err != nil {
		return false, fmt.Errorf("malformed response: %w", err)
	}
	for _, e := range wrapper.Executors {
		if e.Enabled && e.Connected {
			return true, nil
		}
	}
	return false, nil
}

// childCwd returns the named child's working directory as the daemon reports
// it, or "" when it cannot be resolved. Attach uses it as the workspace root so
// the machine offers the directory the child is actually working in.
func childCwd(ctx context.Context, c *client.Client, childID string) string {
	resp, err := c.Request(ctx, protocol.GetRequest{Type: protocol.TypeCtrlGet, ChildID: childID})
	if err != nil || !resp.Success {
		return ""
	}
	var child protocol.ChildSummary
	if err := json.Unmarshal(resp.Data, &child); err != nil {
		return ""
	}
	return child.Cwd
}

// startSessionExecutor makes this machine available as a workspace.
//
// It returns the selector to put on spawns. That selector is returned even when
// no executor was started: the daemon answers with an empty ticket when a
// durable executor already covers this machine and owner, and using it is the
// point — that executor outlives this process, so an agent keeps working after
// the operator closes the terminal.
func startSessionExecutor(ctx context.Context, c *client.Client, root string) (string, func(), error) {
	noop := func() {}

	resp, err := requestSessionExecutor(ctx, c, root)
	if err != nil {
		return "", noop, err
	}
	if !resp.RunLocal {
		// A durable executor already covers this machine. Use it: it outlives
		// this process, so an agent keeps working after the operator detaches.
		return resp.Selector, noop, nil
	}

	addr, socket, err := sessionConnectTarget()
	if err != nil {
		return "", noop, err
	}

	srv := executor.NewServer(executor.Options{
		Root:    root,
		Version: version.String(),
		RTK:     tools.RTKAuto,
	})
	runCtx, cancel := context.WithCancel(ctx)
	stop := func() {
		cancel()
		_ = srv.Close()
	}

	go func() {
		err := execpool.Connect(runCtx, execpool.ConnectOptions{
			Addr:       addr,
			SocketPath: socket,
			Ticket:     resp.Ticket,
			SelfReported: map[string]string{
				"version": version.String(),
			},
			Handler: executorHandler(srv),
		})
		switch {
		case err == nil, errors.Is(err, context.Canceled):
		case errors.Is(err, execpool.ErrEnrollmentRejected):
			// A ticket is one-shot and tied to this control connection.
			// Rejection means the connection is gone or the ticket was spent,
			// and neither is recoverable by retrying.
			slog.Warn("this session's executor ticket was refused; the machine " +
				"is no longer offered as a workspace for this session")
		default:
			slog.Warn("this machine's executor stopped", "error", err)
		}
	}()

	if err := waitExecutorLive(ctx, c, resp.Selector); err != nil {
		stop()
		return "", noop, err
	}
	return resp.Selector, stop, nil
}
