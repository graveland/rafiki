// Package main is the fundi daemon entry point (a pi-controller successor,
// speaking the same protocol). It sets up
// directories, loads persisted state, starts the UDS server, and blocks
// until a signal triggers graceful shutdown.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func main() {
	// Load the environment file before anything reads configuration — that
	// includes the XDG lookups below, since the file may legitimately set them.
	//
	// This runs ahead of the `agent` dispatch deliberately, so a hand-run
	// `fundid agent` gets the same environment the daemon would have given it.
	// That is safe because the real environment always wins: values the daemon
	// sets explicitly when it spawns a child are already present and the file
	// only fills gaps.
	loadServiceEnv()

	// Dispatch `fundid agent ...` before any daemon setup below - it is a
	// separate process mode (a single agent child speaking pi's rpc
	// protocol on stdio) and must not fall through into the daemon's own
	// flag-less startup. Every other invocation (including no args) runs the
	// daemon unchanged.
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		os.Exit(runAgent(os.Args[2:]))
	}

	// The daemon takes no flags, so without this `fundid -h` fell through into
	// startup and failed on the controller socket instead of printing anything.
	// Help goes to stdout and exits 0 — it was asked for, it isn't an error.
	if len(os.Args) > 1 && isHelpArg(os.Args[1]) {
		printRootUsage(os.Stdout)
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// XDG locations, NOT ~/.pi — that is pi's own directory, and sharing its
	// run dir meant fundi and pi-controller claimed the same controller socket
	// ("socket in use by a live process" whenever both were up).
	stateDir := paths.RecordsDir()
	logsDir := paths.LogsDir()
	socketPath := paths.SocketPath()

	// The socket's directory is created separately: it comes from
	// XDG_RUNTIME_DIR when that is set, which is a different tree from the data
	// and state dirs, so creating those would not create it.
	for _, dir := range []string{stateDir, logsDir, filepath.Dir(socketPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Error("mkdir", "dir", dir, "error", err)
			os.Exit(1)
		}
	}

	records, err := persist.ScanRecords(stateDir)
	if err != nil {
		slog.Error("scan state records", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// baseCtx is the daemon's own lifetime context, threaded into every
	// in-process agent child (Controller.baseCtx -> agent.RuntimeOptions) so a
	// hard daemon exit cancels them too. It is deliberately a SEPARATE context
	// from ctx/cancel above: ctx is cancelled early (below, "stop the
	// background sweeper") well before ShutdownAllChildren runs, and reusing
	// it here would abort every in-flight in-process turn before
	// ShutdownAllChildren's graceful stdin-close ladder (mirrored by
	// inproc.Runner.Terminate/Kill for in-process children) gets a chance to
	// run. baseCancel only fires when main() itself returns — strictly after
	// ShutdownAllChildren has already had its full timeout window.
	baseCtx, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()

	// The agent kind's in-process children share one pool across the whole
	// daemon (agent.RuntimeOptions.Pool); BuildEngine/BuildRuntime never open
	// or close one themselves. A nil pool (FUNDI_AGENT_DB unset) means every
	// agent conversation is in-memory, matching `fundid agent --db` unset.
	var pool *pgxpool.Pool
	if dsn := paths.Get(paths.AgentDB); dsn != "" {
		pool, err = pgxpool.New(baseCtx, dsn)
		if err != nil {
			// Deliberately NOT fatal. pgxpool.New only PARSES the DSN, it does
			// not connect, so the only way to reach this branch is a malformed
			// DSN — and killing the daemon over one takes pi and claude
			// children down with it, for an operator who may never spawn an
			// agent at all. "The database is required" is a phase 2 decision
			// that has not been made; exiting here would make it silently, in
			// a failure path, ahead of the design.
			slog.Error("agent database DSN is invalid; starting without a pool. "+
				"Agent conversations will be in-memory and no cost data will be recorded",
				"env", paths.AgentDB, "error", err)
			pool = nil
		} else {
			slog.Info("agent database pool opened")
		}
	} else {
		slog.Warn("no agent database configured; agent conversations are in-memory and no cost data will be recorded",
			"env", paths.AgentDB)
	}

	// The daemon's own proxy face. pi and claude children are separate
	// processes and can only speak HTTP, so they need an address — and making
	// that a second daemon someone must remember to start is how a child ends
	// up talking to a provider directly. Serving it here means it cannot be
	// down while the daemon is up.
	tp, shutdownTracing, err := setupTracing(baseCtx, slog.Default())
	if err != nil {
		slog.Error("otlp tracing setup failed; continuing without tracing", "error", err)
		tp = nil
	} else {
		defer shutdownTracing()
	}

	reg := prometheus.NewRegistry()

	face, err := startProxyFace(baseCtx, faceOptions{
		Pool:     pool,
		Logger:   slog.Default(),
		Tracer:   tp,
		Registry: reg,
	})
	if err != nil {
		// Not fatal: agent children reach the library in-process and are
		// unaffected, and killing the daemon would take them down for a face
		// they never use.
		slog.Error("could not start the proxy face; pi and claude children will not be routed through it",
			"error", err)
	}

	st := childstore.New()
	dumper := persist.NewLogDumper(logsDir, persist.ModeOnExit)
	ctrl := NewController(st, stateDir, logsDir, socketPath, dumper, pool, baseCtx)
	if face != nil {
		ctrl.SetProxy(face.URL, face.Token)
	}
	ctrl.loadOrphans(records)
	ctrl.startSweeper(ctx)

	slog.Info("loaded orphans", "count", len(records))

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		slog.Error("listen", "socket", socketPath, "error", err)
		os.Exit(1)
	}
	slog.Info("fundi daemon listening", "socket", socketPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sig := <-sigCh

	slog.Info("shutting down", "signal", sig)

	// Notify all connected clients so they can exit cleanly before pipes die.
	shutdownEvent := protocol.CtrlDaemonShutdown{
		Type:   protocol.TypeCtrlDaemonShutdown,
		Reason: fmt.Sprintf("signal received: %s", sig),
	}
	if frame, err := json.Marshal(shutdownEvent); err == nil {
		n := srv.Broadcast(frame)
		slog.Info("notified clients of shutdown", "count", n)
		time.Sleep(250 * time.Millisecond) // brief window for frames to land on the wire
	}

	cancel() // stop the background sweeper

	// Shut down all live children gracefully before closing the server. This
	// prevents children from dying via pipe-death (broken stdin/SIGPIPE) when
	// launchd or systemd stops the daemon.
	//
	// pi's own shutdown can involve LLM calls (final compaction, summarisation
	// on session_before_shutdown extension events, etc.), which can take tens
	// of seconds.  The per-child timeouts are picked to give that real work a
	// chance to finish without exceeding the platform-level stop timeouts that
	// the service plist/unit advertises (ExitTimeOut / TimeoutStopSec).
	//
	// The 180s global bound matches the platform stop-timeout we install; if
	// users have an older plist/unit (default 20s on launchd, 90s on systemd),
	// the platform will SIGKILL us first — they should re-run `fundi service
	// uninstall && fundi service install` to pick up the new timeouts.
	const (
		childShutdownTimeout = 120 * time.Second
		childKillTimeout     = 30 * time.Second
		globalTimeout        = 180 * time.Second
	)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), globalTimeout)
	defer shutdownCancel()
	if err := ctrl.ShutdownAllChildren(shutdownCtx, childShutdownTimeout, childKillTimeout); err != nil {
		slog.Warn("child shutdown errors", "error", err)
	}

	// After the children are down: nothing is left to serve, and shutting it
	// earlier would fail their final in-flight turns.
	face.Close(shutdownCtx)

	ctrl.Stop() // wait for sweeper goroutine to exit
	if err := srv.Close(); err != nil {
		slog.Warn("server close", "error", err)
	}

	// Close the pool only after ShutdownAllChildren has returned: every
	// in-process agent's own shutdown (e.g. flushing conversation state) runs
	// before this point, so closing earlier could pull the pool out from
	// under a child still finishing its own graceful stop.
	//
	// baseCancel is called explicitly here, ahead of its own defer, rather
	// than left to fire whenever main() eventually returns: pgxpool.Pool.Close
	// blocks until every acquired connection is released, and
	// ShutdownAllChildren can still return at its 180s global bound with a
	// child's shutdown ladder mid-flight (still holding a connection).
	// Cancelling baseCtx first unblocks that straggler's own ctx-derived work
	// so it releases its connection promptly, bounding pool.Close()'s wait
	// instead of leaving it to block on a child that is never coming back.
	// CancelFunc is idempotent, so the deferred baseCancel() at the top of
	// main is a harmless no-op once this fires.
	baseCancel()
	if pool != nil {
		closePoolBounded(pool, poolCloseTimeout)
	}

	slog.Info("done")
}

// poolCloseTimeout bounds the wait on pgxpool.Pool.Close at daemon exit. Sized
// well under the 180s platform stop timeout the service unit advertises, and
// generous next to the milliseconds a normal close takes once baseCtx is
// cancelled.
const poolCloseTimeout = 15 * time.Second

// closePoolBounded closes pool but refuses to wait on it forever.
//
// pgxpool.Pool.Close blocks until every acquired connection is released.
// Cancelling baseCtx first (see the caller) releases every connection whose
// work is ctx-derived, which is all of them in practice — pgx tears a
// connection down on context cancellation. It is NOT a guarantee: a child that
// Child.Shutdown ABANDONED rather than reaped (see internal/child's
// abandonTimeout) is by definition blocked in something a context cannot
// interrupt, and if that something is holding a pooled connection then
// baseCancel cannot pry it loose. Now that abandonment is a designed outcome
// rather than a theoretical one, the last unbounded wait on the exit path has
// to go: a daemon that has already reported "done" for every child must not
// hang here and get SIGKILLed by the service manager instead of exiting.
//
// Leaking the pool on timeout is safe: the process is about to exit, and the
// kernel closes the sockets.
func closePoolBounded(pool *pgxpool.Pool, timeout time.Duration) {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		pool.Close()
	}()
	select {
	case <-closed:
		slog.Info("agent database pool closed")
	case <-time.After(timeout):
		slog.Error("agent database pool did not close in time; exiting without it. "+
			"A connection is still held by work no context could cancel — most likely an abandoned child",
			"waited", timeout)
	}
}

// loadServiceEnv applies the daemon's environment file (see
// paths.ServiceEnvFile). It is where credentials and any multi-header
// ANTHROPIC_CUSTOM_HEADERS belong: unit files are world-readable, and systemd's
// line-based Environment= cannot represent a value containing the literal
// newline that variable requires as its separator.
//
// Everything here is best-effort but never silent. A missing file is normal and
// says nothing; a load error, a malformed line or loose permissions are all
// reported, because the failure this replaces — configuration that looks
// present and is not — is the expensive one. The applied names are logged (not
// their values) so the log answers "did the daemon actually get the DSN".
func loadServiceEnv() {
	path := paths.ServiceEnvFile()
	applied, warnings, err := paths.LoadEnvFile(path)
	if err != nil {
		slog.Error("could not read the environment file; continuing without it",
			"path", path, "error", err)
	}
	for _, w := range warnings {
		slog.Warn("environment file", "detail", w)
	}
	if len(applied) > 0 {
		slog.Info("loaded environment file", "path", path, "vars", applied)
	}
}
