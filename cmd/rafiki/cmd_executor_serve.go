package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

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
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	return mux
}

// ─── serve ─────────────────────────────────────────────────────────────────────

func newExecutorServeCmd() *cobra.Command {
	var (
		socketPath        string
		connectAddr       string
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
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run an executor: serve filesystem and shell tools to a daemon",
		Long: `Run an executor.

Two transports, exactly one of which must be given:

  --socket   listen on a local unix socket; the daemon connects to it.
  --connect  reverse-dial the daemon and serve HTTP/2 on the dialled
             connection. Required when the daemon cannot reach this host,
             which is the usual case for a laptop behind NAT.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			if socketPath != "" && connectAddr != "" {
				return fmt.Errorf("--socket and --connect are mutually exclusive")
			}
			if socketPath == "" && connectAddr == "" {
				return fmt.Errorf("one of --socket or --connect is required")
			}

			wd, err := resolveRoot(root)
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
			})
			defer func() { _ = srv.Close() }()
			handler := executorHandler(srv)

			if connectAddr != "" {
				return serveReverseDial(connectAddr, pinnedFingerprint, serverName,
					enrollToken, credential, credentialFile, handler)
			}
			return serveUnixSocket(socketPath, wd, handler)
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "path to the unix socket to listen on")
	cmd.Flags().StringVar(&connectAddr, "connect", "", "daemon executor endpoint to dial (host:port)")
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

func serveReverseDial(addr, pinCert, serverName, enrollToken, credential, credentialFile string, handler http.Handler) error {
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

func serveUnixSocket(socketPath, wd string, handler http.Handler) error {
	// Refuse rather than clobber: a live executor already on this path means
	// two processes would serve one socket, and the second bind silently wins.
	if conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("%s is already served by a live executor", socketPath)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot remove stale socket %s: %w", socketPath, err)
	}

	// Umask before bind, not chmod after: chmod leaves a window in which another
	// local user can connect.
	oldMask := unix.Umask(0o177)
	ln, err := net.Listen("unix", socketPath)
	unix.Umask(oldMask)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}

	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: handler, Protocols: protos}
	slog.Info("executor listening", "socket", socketPath, "root", wd, "version", version.String())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		slog.Info("executor shutting down")
		if err := httpSrv.Shutdown(context.Background()); err != nil {
			slog.Warn("executor shutdown", "error", err)
		}
	}()

	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
