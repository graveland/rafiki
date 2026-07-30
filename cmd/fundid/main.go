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

	"git.graveland.dev/brent/fundi/internal/envvar"
	"git.graveland.dev/brent/fundi/internal/paths"
	"git.graveland.dev/brent/fundi/internal/persist"
	"git.graveland.dev/brent/fundi/internal/server"
	"git.graveland.dev/brent/fundi/internal/store"
	"git.graveland.dev/brent/fundi/protocol"
)

func main() {
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
	if dsn := envvar.Get(envvar.AgentDB); dsn != "" {
		pool, err = pgxpool.New(baseCtx, dsn)
		if err != nil {
			slog.Error("open agent database", "error", err)
			os.Exit(1)
		}
		slog.Info("agent database pool opened")
	} else {
		slog.Warn("no agent database configured; agent conversations are in-memory and no cost data will be recorded",
			"env", envvar.AgentDB)
	}

	st := store.New()
	dumper := persist.NewLogDumper(logsDir, persist.ModeOnExit)
	ctrl := NewController(st, stateDir, logsDir, socketPath, dumper, pool, baseCtx)
	ctrl.loadOrphans(records)
	ctrl.startSweeper(ctx)

	slog.Info("loaded orphans", "count", len(records))

	handler := server.NewDispatch(ctrl)
	srv, err := server.Listen(socketPath, handler)
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

	ctrl.Stop() // wait for sweeper goroutine to exit
	if err := srv.Close(); err != nil {
		slog.Warn("server close", "error", err)
	}

	// Close the pool only after ShutdownAllChildren has returned: every
	// in-process agent's own shutdown (e.g. flushing conversation state) runs
	// before this point, so closing earlier could pull the pool out from
	// under a child still finishing its own graceful stop.
	if pool != nil {
		pool.Close()
		slog.Info("agent database pool closed")
	}

	slog.Info("done")
}
