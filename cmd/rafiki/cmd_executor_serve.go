package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/version"
)

// This file is the executor. It used to be its own binary, cmd/rafiki-executor,
// and folding it into `rafiki` leaves one client and one server rather than
// three commands to build, ship and version.
//
// Two things made the fold possible and are worth not undoing. First,
// pkg/executor became pgx-free, so `rafiki` linking it does not trip
// TestClientDoesNotLinkPostgres — see pkg/executor/no_postgres_test.go for what
// that took. Second, the operator verbs alongside this (`enroll`, `list`,
// `label`, `disable`, `enable`) only work against the daemon's control socket,
// which an executor host does not have, so shipping them to one is inert rather
// than a widening of authority.

// resolveRoot turns a possibly-empty, possibly-relative --root into an absolute
// path, defaulting to the working directory.
func resolveRoot(root string) (string, error) {
	wd := root
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot get working directory: %w", err)
		}
	}
	if !filepath.IsAbs(wd) {
		abs, err := filepath.Abs(wd)
		if err != nil {
			return "", fmt.Errorf("cannot resolve root: %w", err)
		}
		wd = abs
	}
	return wd, nil
}

func executorHandler(srv *executor.Server) http.Handler {
	mux := http.NewServeMux()
	interceptor := executor.NewRPCInterceptor()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv, connect.WithInterceptors(interceptor)))
	return mux
}

// loadExecutorEnv applies the executor's environment files before anything in
// RunE resolves a default from the environment. This is what gives a launchd-
// or systemd-supervised executor the operator's ordinary working environment:
// captureExecutorEnv froze it into the 0600 file at install time, and this is
// where serve picks it up.
//
// Two files, two precedences, applied in order:
//
//   - executor.env (paths.ExecutorEnvFile) fills gaps. A variable already
//     present in the process environment — everything the unit bakes in, HOME
//     and PATH above all — wins over the file.
//   - executor-overrides.env (paths.ExecutorOverridesFile) WINS: every
//     variable it names is set unconditionally, beating the unit and the
//     fill-gaps file alike. It exists for the variables launchd/systemd seed
//     themselves and get wrong — SSH_AUTH_SOCK is the canonical case: launchd
//     injects its own per-session agent socket into every LaunchAgent, so a
//     captured value in executor.env is inert under fill-gaps precedence, and
//     only a file that overrides can point the executor at the agent that
//     actually holds the keys. Hand-maintained; install never writes it.
//
// Scoped to `executor serve` ONLY, and deliberately not loaded at binary
// startup the way the daemon's file is: `rafiki` is also the client, and
// applying executor.env to every client invocation would leak captured
// machine state — a RAFIKI_URL or credential captured on an executor-only box
// — into unrelated commands run against some other instance.
func loadExecutorEnv() {
	path := paths.ExecutorEnvFile()
	applied, warnings, err := paths.LoadEnvFile(path)
	if err != nil {
		slog.Error("could not read the executor environment file; continuing without it",
			"path", path, "error", err)
	}
	for _, w := range warnings {
		slog.Warn("executor environment file", "detail", w)
	}
	if len(applied) > 0 {
		slog.Info("loaded executor environment file", "path", path, "vars", applied)
	}

	// Resolved AFTER the fill-gaps load, so executor.env itself can name the
	// overrides file (a RAFIKI_EXECUTOR_OVERRIDES_FILE entry there is applied
	// by the load above and picked up here); the default is otherwise
	// <config dir>/executor-overrides.env.
	overrides := paths.ExecutorOverridesFile()
	if overrides == path {
		slog.Warn("executor environment overrides file is the environment file itself; ignoring",
			"path", path)
		return
	}
	oapplied, owarnings, oerr := paths.LoadEnvFileOverrides(overrides)
	if oerr != nil {
		slog.Error("could not read the executor environment overrides file; continuing without it",
			"path", overrides, "error", oerr)
	}
	for _, w := range owarnings {
		slog.Warn("executor environment overrides file", "detail", w)
	}
	if len(oapplied) > 0 {
		slog.Info("loaded executor environment overrides file", "path", overrides, "vars", oapplied)
	}
}

// ─── serve ─────────────────────────────────────────────────────────────────────

func newExecutorServeCmd() *cobra.Command {
	var (
		connectAddr       string
		connectSocket     string
		root              string
		concurrency       int
		enrollToken       string
		credentialFile    string
		credential        string
		pinnedFingerprint string
		serverName        string
		rtkMode           string
		spillDir          string
		jobBudgetMB       int64
		lspConfig         string
		noLSP             bool
		proxyArgs         []string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run an executor: serve filesystem and shell tools to a daemon",
		Long: `Run an executor.

Two transports, exactly one of which is used:

  --connect         reverse-dial the daemon and serve HTTP/2 on the dialled
                    connection. Required when the daemon cannot reach this host,
                    which is the usual case for a laptop behind NAT. Defaults to
                    the host:port derived from $RAFIKI_URL when neither flag is
                    given.
  --connect-socket  reverse-dial a rafikid on this machine over its executor
                    unix socket, enrolling as a fully rowed pool member.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			loadExecutorEnv()

			resolvedConnect, resolvedSocket, err := resolveExecutorConnectFlags(connectAddr, connectSocket)
			if err != nil {
				return err
			}

			wd, err := resolveRoot(root)
			if err != nil {
				return err
			}

			proxies, err := executor.ParseProxyFlags(proxyArgs)
			if err != nil {
				return err
			}

			srv := executor.NewServer(executor.Options{
				Root:            wd,
				Concurrency:     concurrency,
				Version:         version.String(),
				RTK:             tools.ParseRTKMode(rtkMode),
				SpillDir:        spillDir,
				JobOutputBudget: jobBudgetMB << 20,
				LSPConfig:       lspConfig,
				NoLSP:           noLSP,
				Proxies:         proxies,
			})
			defer func() { _ = srv.Close() }()
			handler := executorHandler(srv)

			// resolveExecutorConnectFlags already guarantees exactly one of
			// these is set, or returned an error above.
			return serveReverseDial(resolvedConnect, resolvedSocket, pinnedFingerprint, serverName,
				enrollToken, credential, credentialFile, handler)
		},
	}

	cmd.Flags().StringVar(&connectAddr, "connect", "", "daemon executor endpoint to dial (host:port; default: derived from $RAFIKI_URL)")
	cmd.Flags().StringVar(&connectSocket, "connect-socket", "",
		"unix socket of a rafikid on this machine to reverse-dial (mutually exclusive with --connect)")
	cmd.Flags().StringVar(&root, "root", "",
		"working directory for this executor's tools (defaults to the current directory). "+
			"NOT a sandbox: an absolute path reaches outside it. What this executor may "+
			"actually reach is its process's filesystem view — the container's mounts, or "+
			"the host user's permissions — and the authoritative description of that lives "+
			"on its database row")
	cmd.Flags().IntVar(&concurrency, "concurrency", 6, "maximum concurrent tool calls")
	cmd.Flags().StringVar(&rtkMode, "rtk", "auto",
		"rewrite known commands through rtk: auto|on|off. The executor operator's choice, "+
			"not the child's — a child cannot see what is installed on this machine")
	cmd.Flags().StringVar(&spillDir, "spill-dir", "",
		"where oversized tool results and background job output are written (defaults to the system temp dir)")
	cmd.Flags().Int64Var(&jobBudgetMB, "job-output-budget-mb", 256,
		"megabytes of background-job output retained per workspace, oldest finished job dropped "+
			"first. Output is kept until the workspace is released; there is no time limit")
	cmd.Flags().StringVar(&lspConfig, "lsp-config", "",
		"path to an lsp.json describing language servers this executor may start (default: auto-detect what is on PATH)")
	cmd.Flags().BoolVar(&noLSP, "no-lsp", false, "disable language servers on this executor entirely")
	cmd.Flags().StringArrayVar(&proxyArgs, "proxy", nil, "LLM endpoint this executor will forward to, name=base_url (repeatable)")
	cmd.Flags().StringVar(&enrollToken, "enroll-token", os.Getenv("RAFIKI_ENROLL_TOKEN"),
		"one-time enrollment token, required on first --connect")
	cmd.Flags().StringVar(&credentialFile, "credential-file", "",
		"where the durable credential is stored after enrollment (default: <data dir>/executor.cred)")
	cmd.Flags().StringVar(&credential, "credential", os.Getenv("RAFIKI_EXECUTOR_CREDENTIAL"),
		"use this credential directly and write nothing to disk. For stateless deployments that inject it from a secret store; skips enrollment entirely")
	cmd.Flags().StringVar(&pinnedFingerprint, "pin-cert", "",
		"SHA-256 fingerprint of the daemon's leaf certificate. Pins the leaf instead of verifying against system roots — use for a self-signed or internal-CA daemon")
	cmd.Flags().StringVar(&serverName, "server-name", "",
		"TLS server name (SNI) to present, when it differs from the host in --connect. Needed when dialling an IP or a node port while the certificate names a hostname")

	return cmd
}

func serveReverseDial(addr, socketPath, pinCert, serverName, enrollToken, credential, credentialFile string, handler http.Handler) error {
	// The default deliberately does NOT sit under --root. It used to
	// (`<root>/.rafiki-executor-credential`), which put the executor's own
	// credential inside the very directory tree its file tools operate on — and
	// native executors have no path scoping, by design. An agent running on the
	// executor could read it and reconnect as that machine, including after an
	// operator disabled it. paths.DataDir is for state that must survive a
	// reboot, which a credential must: losing it means re-enrolling by hand.
	credFile := credentialFile
	if credFile == "" && credential == "" {
		credFile = filepath.Join(paths.DataDir(), "executor.cred")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := execpool.Connect(ctx, execpool.ConnectOptions{
		Addr:           addr,
		SocketPath:     socketPath,
		PinCert:        pinCert,
		ServerName:     serverName,
		EnrollToken:    enrollToken,
		Credential:     credential,
		CredentialFile: credFile,
		SelfReported: map[string]string{
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"version": version.String(),
		},
		Handler: handler,
	}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}
