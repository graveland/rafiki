// Package main is the rafiki daemon entry point. It sets up directories, loads
// persisted state, starts the UDS server, and blocks until a signal triggers
// graceful shutdown. (rafiki began as a fork of pi-controller, and the control
// plane still speaks the same newline-delimited JSON frame protocol.)
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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
	"go.graveland.dev/rafiki/pkg/darajapool"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/executorsdb"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
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

	// Normalize single-dash long flags (-model → --model) for pflag compatibility.
	// stdlib flag accepted both forms; pflag rejects the single-dash form as an
	// unknown shorthand. ctrl_spawn's caller-supplied ExtraArgs reach rafikid
	// fundi through buildAgentArgv, so this is a wire-visible contract.
	normalizeArgsForPflag()

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
			return runDaemon(runDaemonOpts{
				Config: cfg,
				Listen: listenAddr,
				DB:     dbDSN,
				Dev:    dev,
			})
		},
	}

	root.Flags().StringVar(&configPath, "config", "", "config file (openai routes, default model)")
	root.Flags().StringVar(&listenAddr, "listen", "", "proxy face listen address (overrides RAFIKI_PROXY_LISTEN)")
	root.Flags().StringVar(&dbDSN, "db", "", "postgres DSN (overrides RAFIKI_DB)")
	root.Flags().BoolVar(&dev, "dev", false, "dev mode: auto-migrate the schema")

	root.AddCommand(newFundiCmd())
	root.AddCommand(newAgentCmd())
	root.AddCommand(newMigrateCmd())

	return root
}

func newFundiCmd() *cobra.Command {
	var f agentFlags

	cmd := &cobra.Command{
		Use:   protocol.KindFundi,
		Short: "Run one agent child on stdio (pi rpc protocol)",
		Long: `Runs a single agent child speaking pi's rpc protocol on stdio, in place of
Claude Code. Normally spawned by the rafiki daemon rather than invoked directly.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// pflag: detect whether --tools-web appeared in argv.
			if fl := cmd.Flags().Lookup("tools-web"); fl != nil {
				f.toolsWebSet = fl.Changed
			}

			if f.model == "" || !strings.Contains(f.model, "/") {
				return fmt.Errorf(`--model is required and must be provider-qualified, e.g. "anthropic/sonnet-latest" or "deepseek/deepseek-chat"`)
			}

			code := runAgentWithFlags(f)
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&f.model, "model", "", "provider-qualified model id, e.g. \"anthropic/sonnet-latest\" or \"deepseek/deepseek-chat\" (required)")
	cmd.Flags().StringVar(&f.thinking, "thinking", "off", "extended-thinking level: off|low|medium|high|xhigh")
	cmd.Flags().StringVar(&f.systemPrompt, "system-prompt", "", "override the base system prompt")
	cmd.Flags().StringVar(&f.appendSystemPrompt, "append-system-prompt", "", "append to the system prompt")
	cmd.Flags().BoolVar(&f.noContextFiles, "no-context-files", false, "skip loading CLAUDE.md/AGENTS.md context files")
	cmd.Flags().StringArrayVar(&f.skillsDir, "skills-dir", nil, "additional skills directory (repeatable)")
	cmd.Flags().StringVar(&f.skills, "skills", "", "comma-separated list restricting discovered skills to these names")
	cmd.Flags().BoolVar(&f.noSkills, "no-skills", false, "disable skill discovery and the skill tool entirely")
	cmd.Flags().StringVar(&f.mcpConfig, "mcp-config", "", "path to .mcp.json (default: <cwd>/.mcp.json if present, else $RAFIKI_MCP_CONFIG or <ConfigDir>/mcp.json)")
	cmd.Flags().StringVar(&f.lspConfig, "lsp-config", "", "path to lsp.json (default: <cwd>/.lsp.json if present, else $RAFIKI_LSP_CONFIG or <ConfigDir>/lsp.json)")
	cmd.Flags().StringVar(&f.ref, "ref", paths.Get(paths.ChildID), "external ref correlating the conversation across restarts")
	cmd.Flags().StringVar(&f.db, "db", paths.Get(paths.DB), "postgres url for conversation persistence (empty: in-memory)")
	cmd.Flags().StringVar(&f.spillDir, "spill-dir", "", "directory for clipped tool output (default: <XDG_CACHE_HOME>/rafiki/spill/<ref>)")
	cmd.Flags().StringVar(&f.name, "name", "", "session name reported through get_state")
	cmd.Flags().IntVar(&f.maxOutputTokens, "max-output-tokens", 0, "per-turn output token cap sent to upstream (0 = default 16384)")
	cmd.Flags().BoolVar(&f.recordRequests, "record-requests", false, "Record raw LLM API requests and responses for debugging")
	cmd.Flags().StringVar(&f.bashRTK, "bash-rtk", "", "route bash commands through rtk for output compression: auto, on, or off (overrides $RAFIKI_BASH_RTK)")
	cmd.Flags().BoolVar(&f.toolsWeb, "tools-web", false, "enable the webfetch/websearch tools (overrides $RAFIKI_TOOLS_WEB; default off; disable with --tools-web=false)")
	cmd.Flags().StringVar(&f.fakeTurns, "fake-turns", "", "replay a recorded turn file instead of calling upstream (testing)")

	return cmd
}

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "DSN-backed insights CLI: stats, search, export, analyze, findings",
		Long: `DSN-backed CLI over the same conversation store the proxy captures into.
Subcommands: stats (aggregates), search (full-text + filters),
export (single transcript), analyze (LLM-driven skill-gap detector),
findings (triage analysis output).`,
		SilenceUsage: true,
	}

	// Persistent flags shared by every agent subcommand.
	def := os.Getenv("RAFIKI_DB")
	if def == "" {
		def = os.Getenv("RAFIKI_TEST_DSN")
	}
	cmd.PersistentFlags().String("db", def, "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")
	cmd.PersistentFlags().BoolP("json", "j", false, "JSON output, indented")
	cmd.PersistentFlags().BoolP("json-compact", "J", false, "JSON output, compact")

	cmd.AddCommand(newAgentStatsCmd())
	cmd.AddCommand(newAgentSearchCmd())
	cmd.AddCommand(newAgentExportCmd())
	cmd.AddCommand(newAgentAnalyzeCmd())
	cmd.AddCommand(newAgentFindingsCmd())

	return cmd
}

func newMigrateCmd() *cobra.Command {
	var dbDSN string

	cmd := &cobra.Command{
		Use:           "migrate",
		Short:         "Apply the conversations schema migration chain",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbDSN == "" {
				return errors.New("--db (or RAFIKI_DB) is required")
			}
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, dbDSN)
			if err != nil {
				return err
			}
			defer pool.Close()
			return store.Migrate(ctx, pool)
		},
	}

	def := os.Getenv("RAFIKI_DB")
	if def == "" {
		def = os.Getenv("RAFIKI_TEST_DSN")
	}
	cmd.Flags().StringVar(&dbDSN, "db", def, "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")

	return cmd
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

	// XDG locations, NOT ~/.pi — that is pi's own directory, and a daemon must
	// not write its runtime state into another program's config directory.
	// (Historically this also prevented a socket collision with pi-controller.)
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
	// or close one themselves.
	//
	// The database is REQUIRED as of 2026-08-25 (Phase C design 2.1). This
	// used to be a warning and a nil pool; the comment here used to say "'The
	// database is required' is a phase 2 decision that has not been made".
	// It has been made.
	dsn, err := requireDSN(opts.DB)
	if err != nil {
		slog.Error("cannot start", "error", err)
		os.Exit(1)
	}
	pool, err := pgxpool.New(baseCtx, dsn)
	if err != nil {
		slog.Error("database DSN is invalid", "env", paths.DB, "error", err)
		os.Exit(1)
	}
	// pgxpool.New only PARSES the DSN. Ping is what proves the database is
	// reachable, and an unreachable database is a startup failure now rather
	// than a daemon that runs with every persistent feature silently dead.
	pingCtx, pingCancel := context.WithTimeout(baseCtx, 10*time.Second)
	err = pool.Ping(pingCtx)
	pingCancel()
	if err != nil {
		slog.Error("database is not reachable", "env", paths.DB, "error", err)
		os.Exit(1)
	}
	slog.Info("agent database pool opened")
	if opts.Dev {
		if err := store.Migrate(baseCtx, pool); err != nil {
			slog.Error("dev mode: migrate failed", "error", err)
			os.Exit(1)
		}
		slog.Info("dev mode: store migrated")
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

	// Raw request/response trace store. Created whenever the daemon has a
	// database pool so per-session opt-in via --record-requests always works.
	// RAFIKI_RECORD_REQUESTS=1 lifts the per-session gate: every session (and
	// every proxied request, regardless of header) is recorded unconditionally.
	var rawTrace *rawtrace.RawTraceStore
	rawTraceAll := false
	if pool != nil {
		rawTrace = rawtrace.NewRawTraceStore(pool)
		if paths.Get(paths.RecordRequests) == "1" {
			rawTraceAll = true
			slog.Info("raw request capture enabled (RAFIKI_RECORD_REQUESTS=1, record-all)")
		}
	}

	// Identity store. Nil when RAFIKI_DB is unset — every user verb then
	// returns ErrNoAgentDB rather than pretending an empty user table.
	var userStore users.Store
	if pool != nil {
		userStore = usersdb.NewPostgresStore(pool)
	}

	// Load the provider registry once at startup. A missing file is not an
	// error — it falls back to the shipped default (anthropic + openrouter).
	prov, err := providers.Load(paths.ProvidersFile())
	if err != nil {
		slog.Error("could not load provider registry; continuing with defaults", "error", err)
		prov = providers.Default()
	} else {
		slog.Info("provider registry loaded", "providers", prov.Names())
	}

	face, err := startProxyFace(baseCtx, faceOptions{
		Pool:        pool,
		Logger:      slog.Default(),
		Tracer:      tp,
		Registry:    reg,
		Config:      opts.Config,
		Listen:      opts.Listen,
		Catalog:     catalog,
		RawTrace:    rawTrace,
		RawTraceAll: rawTraceAll,
		Users:       userStore,
		Providers:   prov,
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

	// The executor store backs both the control-plane verbs (enroll/list/
	// label/disable) and the reverse-dial listener's authentication. Build it
	// once, up front, when the listener is configured; both consumers share it.
	//
	// controlAddr is resolved here (rather than at its original call site
	// further down) so executorsEnabled's default can depend on it; the TCP
	// listener setup below reuses this same value instead of re-parsing it.
	controlAddr := parseControlListenAddr()
	var execStore executors.Store
	if executorsEnabled(controlAddr, pool != nil) {
		execStore = executorsdb.NewPostgresStore(pool)
	}

	ctrl := NewController(st, stateDir, logsDir, socketPath, dumper, pool, rawTrace, baseCtx, execStore, userStore, prov)
	ctrl.wireEventBuffer()
	ctrl.SetCatalog(catalog)
	if face != nil {
		ctrl.SetProxy(face.URL, face.Token)
		if face.Control != nil {
			face.Control.SetChildResolver(ctrl)
			face.Control.SetEventSource(ctrl.nativeEventSource())
			face.Control.SetLineage(ctrl)
			face.Control.SetEventLog(ctrl.evlog)
			face.Control.SetInbox(ctrl.connectInbox())
			face.Control.SetChildLister(ctrl)
			face.Control.SetTaskLister(ctrl)
			face.Control.SetChildLifecycle(connectLifecycle{c: ctrl})
			face.Control.SetModelLister(connectModels{c: ctrl})
			if face.QuotaStore != nil {
				face.Control.SetQuotaReader(connectQuota{store: face.QuotaStore})
			}
		}
	}
	// The executor pool no longer owns a listener. It is reached at a PATH on
	// the shared TLS listener below, upgraded out of HTTP/1.1, so the control
	// plane and the executor link share one port and one certificate — and the
	// executor link inherits GetCertificate, which its own listener never had
	// (a cert-manager rotation used to need a pod restart to reach it).
	var execPool *execpool.Pool
	if execStore != nil {
		execPool = execpool.New(execStore)
		execPool.SetOnLost(ctrl.HandleExecutorLost)
		ctrl.execPool = execPool
		ctrl.execPoolConn = execPool
		// One sweeper for the pool, regardless of how many listeners feed it.
		// It used to start inside the TLS branch, which was correct only while
		// that was the sole way in; the unix listener below is the second.
		execPool.StartSweeper(ctx)
	}

	// Daraja pool: accepts per-child reverse-dialled connections.
	var darajaPool *darajapool.Pool
	if execStore != nil {
		darajaPool = darajapool.New(darajapool.NewRegistry())
	}

	// Wire the controller's reach-through callbacks on connect/disconnect.
	if darajaPool != nil {
		ctrl.WireDaraja(darajaPool, darajaPool.Reg())
	}

	if execPool != nil {
		execSock := paths.ExecutorSocketPath()
		if ln, err := serveExecutorUDS(ctx, execPool, darajaPool, execSock); err != nil {
			// Not fatal. A daemon whose TLS listener serves a whole fleet must
			// not fail to start because a stale local socket is held by
			// something else; local enrollment is a convenience and the remote
			// path is unaffected.
			slog.Warn("executor unix listener unavailable; local enrollment disabled",
				"path", execSock, "error", err)
		} else {
			defer ln.Close()
			slog.Info("rafiki daemon accepting executors (unix)", "path", execSock)
		}
	}

	if face != nil && face.Control != nil {
		connectSock := paths.ConnectSocketPath()
		if ln, err := serveConnectUDS(ctx, face.Control, face.TokenAuth, connectSock); err != nil {
			// Fatal. This socket is how `rafiki attach` reaches the daemon; a
			// daemon serving no local control plane looks alive and answers
			// nothing the TUI asks for.
			slog.Error("cannot serve the local Connect control plane",
				"path", connectSock, "error", err)
			os.Exit(1)
		} else {
			defer ln.Close()
			slog.Info("rafiki daemon serving Connect (unix)", "path", connectSock)
		}
	}

	ctrl.loadChildren(baseCtx)
	ctrl.startSweeper(ctx)
	ctrl.startLeaseRenewal(ctx)

	if pool != nil {
		go syncPricingLoop(baseCtx, pool, catalog)
	}

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		slog.Error("listen", "socket", socketPath, "error", err)
		os.Exit(1)
	}
	slog.Info("rafiki daemon listening", "socket", socketPath)

	// TCP control listener (optional — for remote attach, k8s deployment).
	var tcpSrv *control.Server
	if addr := controlAddr; addr != "" {
		// Remote serving requires a database: user auth is row-backed and
		// there is nothing to degrade to. Without this check the daemon
		// comes up serving a TLS listener stuck in permanent bootstrap
		// mode — unauthenticated ctrl_user_create accepted from anywhere,
		// then failing on the insert.
		if userStore == nil {
			slog.Error("RAFIKI_CONTROL_LISTEN requires RAFIKI_DB: user identity is database-backed")
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
		// http/1.1 only. net/http can hijack an HTTP/1.1 connection and not an
		// HTTP/2 one, and every protocol here is reached by upgrading out of a
		// plain request.
		tlsConfig.NextProtos = execpool.ALPNProtocols

		ln, err := tls.Listen("tcp", addr, tlsConfig)
		if err != nil {
			slog.Error("TCP control listener failed", "addr", addr, "error", err)
			os.Exit(1)
		}

		attached := control.NewAttached(handler)
		tcpSrv = attached

		mux := http.NewServeMux()
		mux.Handle(upgradeconn.PathFor(upgradeconn.Control),
			upgradeconn.Handler(upgradeconn.Control, func(c *upgradeconn.Conn) {
				attached.ServeUpgraded(c, userStore)
			}))
		if execPool != nil {
			mux.Handle(upgradeconn.PathFor(upgradeconn.Executor), execPool.UpgradeHandler())
		}
		if darajaPool != nil {
			mux.Handle(upgradeconn.PathFor(upgradeconn.Daraja), darajaPool.UpgradeHandler())
		}

		// Everything else on this listener is the proxy face: /v1/messages,
		// /v1/chat/completions, /healthz, /metrics and its unrouted-request
		// logger. ServeMux prefers the longest matching pattern, so the two
		// exact paths above win and the rest falls through here.
		//
		// This is what makes ONE hostname on ONE port serve all three surfaces.
		// The face keeps its loopback listener too — children talk to their own
		// daemon over 127.0.0.1 and should not need a certificate to do it.
		if face != nil && face.Handler != nil {
			mux.Handle("/", face.Handler)
		}

		muxSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := muxSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("control/executor listener stopped", "error", err)
			}
		}()
		// While no user exists, anyone who can reach this listener can claim
		// the daemon with the first ctrl_user_create. That window is
		// deliberate, but it should never be quiet.
		go warnWhileUnclaimed(ctx, userStore, addr, unclaimedWarnInterval)

		slog.Info("rafiki daemon listening (TCP/TLS)", "addr", addr,
			"control", upgradeconn.PathFor(upgradeconn.Control),
			"executor", upgradeconn.PathFor(upgradeconn.Executor),
			"daraja", upgradeconn.PathFor(upgradeconn.Daraja),
			"executorEnabled", execPool != nil)
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
	ctrl.ReleaseAllLeases()
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

// unclaimedWarnInterval is how often the daemon repeats the bootstrap
// warning while no user exists.
const unclaimedWarnInterval = time.Minute

// warnWhileUnclaimed logs a warning every unclaimedWarnInterval for as long as
// the TLS listener is in bootstrap mode. It checks first and waits second, so
// the warning lands when the listener comes up — the moment an operator is
// actually reading the log — rather than a minute later.
//
// It stops only when a user exists. A store error keeps it ticking: "I could
// not check" is not evidence the window is closed, and returning on the first
// blip would silence the warning for the daemon's whole lifetime.
func warnWhileUnclaimed(ctx context.Context, userStore users.Store, addr string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := userStore.CountActive(ctx)
		if err == nil {
			if n > 0 {
				return
			}
			slog.Warn("no users exist: this listener accepts an unauthenticated "+
				"ctrl_user_create from anyone who can reach it. Run `rafiki user create <name>` now.",
				"addr", addr)
		}
		// A store error falls through to the next tick: it is not evidence
		// the window is closed. It is not logged here either — the outage
		// logs itself, on every request that touches the database.
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// executorsEnabled decides whether to build the executor store, per the
// RAFIKI_EXECUTORS_ENABLED doc comment in pkg/paths/envvar.go: explicit "0"/
// "false" always refuses, any other explicit value always enables (subject to
// dbConfigured, since there is nothing to back the store with otherwise), and
// an unset value defaults on when there is no TCP control listener (the only
// executor path is then the local UDS socket, no new exposure to gate) and
// off once one is configured (that path would otherwise be reachable
// remotely the moment RAFIKI_CONTROL_LISTEN is turned on).
func executorsEnabled(controlAddr string, dbConfigured bool) bool {
	if !dbConfigured {
		return false
	}
	switch paths.Get(paths.ExecutorsEnabled) {
	case "0", "false":
		return false
	case "":
		return controlAddr == ""
	default:
		return true
	}
}

// parseControlListenAddr returns the TCP address string from
// RAFIKI_CONTROL_LISTEN, stripping an optional "tcp:" prefix. Returns ""
// when the env var is unset or not a valid host:port.
//
// A bare port after stripping the prefix (the documented "tcp:8036" form,
// used verbatim in .env.example, README.md, and
// docs/reference/control-protocol.md) has no colon at all, so
// net.SplitHostPort would reject it with "missing port in address" — that
// silently disabled the TCP control listener for anyone following the docs.
// Such a value is promoted to ":8036" (all-interfaces, that port) before
// validation. Already-valid forms ("host:port", ":port") pass through
// unchanged.
func parseControlListenAddr() string {
	v := paths.Get(paths.ControlListen)
	if v == "" {
		return ""
	}
	addr := strings.TrimPrefix(v, "tcp:")
	if addr != "" && !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
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

// normalizeArgsForPflag converts single-dash long flags (-model) to double-dash
// form (--model) in os.Args before cobra parses them. stdlib flag accepts both
// -db and --db; pflag rejects -db as "unknown shorthand flag: 'd'".
// ctrl_spawn's caller-supplied ExtraArgs reach rafikid fundi through
// buildAgentArgv, so single-dash forms are a wire-visible contract.
//
// Single-character args (-j) are left alone as shorthands.
func normalizeArgsForPflag() {
	normalized := make([]string, len(os.Args))
	for i, a := range os.Args {
		normalized[i] = normalizeArg(a)
	}
	os.Args = normalized
}

// syncPricingLoop runs SyncModelPricing once at startup and then on the given
// interval, keeping the model_pricing table populated so conversation search and
// v_turn views can compute costs in SQL.
func syncPricingLoop(ctx context.Context, pool *pgxpool.Pool, src store.PriceSource) {
	interval := 6 * time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	syncOnce := func() {
		n, err := store.SyncModelPricing(ctx, pool, src)
		if err != nil {
			slog.Warn("sync model pricing failed", "error", err)
			return
		}
		slog.Info("model pricing synced", "rows", n)
	}

	syncOnce()
	for {
		select {
		case <-ticker.C:
			syncOnce()
		case <-ctx.Done():
			return
		}
	}
}

// form. Single-char (-j), double-dash (--x), and non-flag args pass through.
func normalizeArg(a string) string {
	if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
		return a
	}
	// -x=value or -x value forms; strip the leading dash.
	rest := a[1:]
	if eq := strings.IndexByte(rest, '='); eq >= 0 {
		name := rest[:eq]
		if len(name) > 1 {
			return "--" + rest
		}
		return a // single-char shorthand with =value (-j=...)
	}
	// No = sign.
	if len(rest) > 1 {
		return "--" + rest
	}
	return a // single-char shorthand (-j)
}
