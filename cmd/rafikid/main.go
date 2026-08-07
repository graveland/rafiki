// Package main is the rafiki daemon entry point (a pi-controller successor,
// speaking the same protocol). It sets up
// directories, loads persisted state, starts the UDS server, and blocks
// until a signal triggers graceful shutdown.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/version"
)

// modelCatalogTTL matches llm.NewClient's own default (the catalog a
// ClientOption-less client would build for itself) — sharing one instance
// between the proxy face and the Controller (ctrl_get/list's ContextWindow)
// only makes sense if both see the same refresh cadence.
const modelCatalogTTL = time.Hour

func main() {
	// Load the environment file before anything reads configuration — that
	// includes the XDG lookups below, since the file may legitimately set them.
	//
	// This runs ahead of cobra dispatch deliberately, so a hand-run
	// `rafikid fundi` or `rafikid agent` gets the same environment the daemon
	// would have given it. That is safe because the real environment always
	// wins: values the daemon sets explicitly when it spawns a child are
	// already present and the file only fills gaps.
	loadServiceEnv()

	root := newRootCmd()

	// Resolve the config paths once at startup for the help text.
	root.Long = rootLong()

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "rafikid:", err)
		os.Exit(1)
	}
}

// rootLong returns the full daemon usage string with resolved config paths.
func rootLong() string {
	var b strings.Builder
	fmt.Fprintln(&b, `rafikid — coding-agent controller daemon and native agent runtime.

Subcommands:
  fundi     Run one agent child on stdio (pi rpc protocol).
            Spawned by the daemon; see 'rafikid fundi -h'.
  agent     DSN-backed insights CLI: stats|search|export|
            analyze|findings. See 'rafikid agent' with no verb.
  migrate   Apply the conversations schema migration chain.

The command-line client is a separate binary, "rafiki".

The daemon listens on a unix socket and stores its state under the XDG base
directories (override with the standard XDG_* variables). These are this
process's own resolved paths — a client (rafiki, or a launchd/systemd unit
with a different environment) may resolve differently if its HOME or XDG_*
variables disagree with the daemon's:`)
	fmt.Fprintf(&b, "\n  %-12s %s\n", "socket", paths.SocketPath())
	fmt.Fprintf(&b, "  %-12s %s\n", "records", paths.RecordsDir())
	fmt.Fprintf(&b, "  %-12s %s\n", "logs", paths.LogsDir())
	fmt.Fprintf(&b, "  %-12s %s\n", "instructions", paths.InstructionsFile())
	for i, d := range paths.SkillsDirs() {
		label := ""
		if i == 0 {
			label = "skills"
		}
		fmt.Fprintf(&b, "  %-12s %s\n", label, d)
	}
	fmt.Fprintf(&b, "  %-12s %s\n", "presets", paths.PresetsFile())
	fmt.Fprintf(&b, "  %-12s %s\n", "mcp", paths.GlobalMCPConfig())
	fmt.Fprint(&b, `
$RAFIKI_SOCKET overrides the socket path for both the daemon's clients
and any child it spawns. $RAFIKI_INSTRUCTIONS, $RAFIKI_SKILLS_DIRS, and
$RAFIKI_MCP_CONFIG override the instructions/skills/mcp paths above.
`)
	return b.String()
}

func newRootCmd() *cobra.Command {
	var configPath string
	var listenAddr string
	var dbDSN string
	var dev bool

	root := &cobra.Command{
		Use:           "rafikid",
		Short:         "Coding-agent controller daemon and native agent runtime",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return err
			}
			if dev {
				if cfg.Tokens == nil {
					cfg.Tokens = map[string]string{}
				}
				cfg.Tokens["dev"] = "dev"
			}
			return runDaemon(runDaemonOpts{
				Config: cfg,
				Listen: listenAddr,
				DB:     dbDSN,
				Dev:    dev,
			})
		},
	}

	root.Flags().StringVar(&configPath, "config", "", "config file (named client tokens, openai routes, default model)")
	root.Flags().StringVar(&listenAddr, "listen", "", "proxy face listen address (overrides RAFIKI_PROXY_LISTEN)")
	root.Flags().StringVar(&dbDSN, "db", "", "postgres DSN (overrides RAFIKI_DB)")
	root.Flags().BoolVar(&dev, "dev", false, "dev mode: auto-migrate the schema, accept the token \"dev\"")

	root.AddCommand(newFundiCmd())
	root.AddCommand(newAgentCmd())
	root.AddCommand(newMigrateCmd())

	return root
}

func newFundiCmd() *cobra.Command {
	return &cobra.Command{
		Use:                protocol.KindFundi,
		Short:              "Run one agent child on stdio (pi rpc protocol)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			code := runAgent(args)
			if code != 0 {
				// runAgent already printed errors via slog; just exit with the
				// right code. Don't let cobra print anything.
				os.Exit(code)
			}
			return nil
		},
	}
}

func newAgentCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "agent",
		Short:              "DSN-backed insights CLI",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return agentCmd(args)
		},
	}
}

func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "migrate",
		Short:              "Apply the conversations schema migration chain",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return migrateCmd(args)
		},
	}
}

// runDaemonOpts is the cobra-parsed daemon configuration.
type runDaemonOpts struct {
	Config Config
	Listen string
	DB     string
	Dev    bool
}

// runDaemon is the daemon's main loop, extracted from the old main() body.
// It owns everything from flag parsing onward and never returns on success
// (it blocks until signalled).
func runDaemon(opts runDaemonOpts) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// XDG locations, NOT ~/.pi — that is pi's own directory, and sharing its
	// run dir meant rafiki and pi-controller claimed the same controller socket
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
	// in-process agent child (Controller.baseCtx -> fundi.RuntimeOptions) so a
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
	// daemon (fundi.RuntimeOptions.Pool); BuildEngine/BuildRuntime never open
	// or close one themselves. A nil pool (RAFIKI_DB unset) means every
	// agent conversation is in-memory, matching `rafikid agent --db` unset.
	var pool *pgxpool.Pool
	dsn := opts.DB
	if dsn == "" {
		dsn = paths.Get(paths.DB)
	}
	if dsn != "" {
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
				"env", paths.DB, "error", err)
			pool = nil
		} else {
			slog.Info("agent database pool opened")
			if opts.Dev {
				if err := store.Migrate(baseCtx, pool); err != nil {
					slog.Error("dev mode: migrate failed", "error", err)
					os.Exit(1)
				}
				slog.Info("dev mode: store migrated")
			}
		}
	} else {
		slog.Warn("no agent database configured; agent conversations are in-memory and no cost data will be recorded",
			"env", paths.DB)
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

	// One catalog shared between the proxy face's llm.Client and the
	// Controller (ctrl_get/list's ContextWindow field) — built here,
	// independent of whether the proxy face itself manages to start, so a
	// failed face doesn't also cost ctrl_get its context-window data.
	catalog := routing.NewModelCatalog(http.DefaultClient, modelCatalogTTL, slog.Default())

	face, err := startProxyFace(baseCtx, faceOptions{
		Pool:     pool,
		Logger:   slog.Default(),
		Tracer:   tp,
		Registry: reg,
		Config:   opts.Config,
		Listen:   opts.Listen,
		Catalog:  catalog,
	})
	if err != nil {
		// Not fatal: agent children reach the library in-process and are
		// unaffected, and killing the daemon would take them down for a face
		// they never use.
		slog.Error("could not start the proxy face; pi and claude children will not be routed through it",
			"error", err)
	}

	// OpenRouter OTLP broadcast receiver (opt-in, separate port).
	broadcastSrv, err := startBroadcastListener(baseCtx, pool, slog.Default())
	if err != nil {
		slog.Error("failed to start broadcast listener", "error", err)
		os.Exit(1)
	}

	st := childstore.New()
	dumper := persist.NewLogDumper(logsDir, persist.ModeOnExit)
	ctrl := NewController(st, stateDir, logsDir, socketPath, dumper, pool, baseCtx)
	ctrl.SetCatalog(catalog)
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
	slog.Info("rafiki daemon listening", "socket", socketPath)

	// TCP control listener (optional — for remote attach, k8s deployment).
	var tcpSrv *control.Server
	if addr := parseControlListenAddr(); addr != "" {
		controlToken := paths.Get(paths.ControlToken)
		if controlToken == "" {
			slog.Error("RAFIKI_CONTROL_TOKEN must be set when RAFIKI_CONTROL_LISTEN is set")
			os.Exit(1)
		}
		certFile := paths.Get(paths.ControlTLSCert)
		keyFile := paths.Get(paths.ControlTLSKey)
		if certFile == "" || keyFile == "" {
			slog.Error("RAFIKI_CONTROL_TLS_CERT and RAFIKI_CONTROL_TLS_KEY must be set when RAFIKI_CONTROL_LISTEN is set")
			os.Exit(1)
		}
		initialCert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			slog.Error("failed to load TLS cert/key for control plane", "cert", certFile, "key", keyFile, "error", err)
			os.Exit(1)
		}
		tlsConfig := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{initialCert},
			GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(certFile, keyFile)
				if err != nil {
					slog.Error("control: TLS cert reload failed; using last known good cert", "error", err)
					return nil, nil // fall back to Certificates[0]
				}
				return &cert, nil
			},
		}
		tcpSrv, err = control.ListenTCP(addr, controlToken, tlsConfig, handler)
		if err != nil {
			slog.Error("TCP control listener failed", "addr", addr, "error", err)
			os.Exit(1)
		}
		slog.Info("rafiki daemon listening (TCP/TLS)", "addr", addr)
	}

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
	// the platform will SIGKILL us first — they should re-run `rafiki service
	// uninstall && rafiki service install` to pick up the new timeouts.
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

	if broadcastSrv != nil {
		bcCtx, bcCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bcCancel()
		if err := broadcastSrv.Shutdown(bcCtx); err != nil {
			slog.Warn("broadcast listener shutdown error", "error", err)
		}
	}

	ctrl.Stop() // wait for sweeper goroutine to exit
	if err := srv.Close(); err != nil {
		slog.Warn("server close", "error", err)
	}
	if tcpSrv != nil {
		if err := tcpSrv.Close(); err != nil {
			slog.Warn("TCP server close", "error", err)
		}
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
	return nil
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

// parseControlListenAddr returns the TCP address string from
// RAFIKI_CONTROL_LISTEN, stripping an optional "tcp:" prefix. Returns ""
// when the env var is unset or not a valid host:port.
func parseControlListenAddr() string {
	v := paths.Get(paths.ControlListen)
	if v == "" {
		return ""
	}
	addr := strings.TrimPrefix(v, "tcp:")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		slog.Warn("RAFIKI_CONTROL_LISTEN is not a valid address; ignoring", "value", v, "error", err)
		return ""
	}
	return addr
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
