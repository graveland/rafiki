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
//
// `serve` and `serve-stdio` are separate subcommands rather than one command
// with a mode flag: the mutual exclusion is then structural, cobra enforces it,
// and `serve-stdio` does not advertise a dozen flags that mean nothing inside a
// container.

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
		pinnedFingerprint string
		isolation         string
		workspaceMode     string
		image             string
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
			if isolation == "container" && image == "" {
				return fmt.Errorf("--isolation container requires --image")
			}
			if isolation == "none" && workspaceMode == "ephemeral" {
				return fmt.Errorf("--isolation none does not support --workspace-mode ephemeral")
			}

			wd, err := resolveRoot(root)
			if err != nil {
				return err
			}

			srv := executor.NewServer(executor.Options{
				Root:          wd,
				Concurrency:   concurrency,
				Version:       version.String(),
				Isolation:     isolation,
				WorkspaceMode: workspaceMode,
				Image:         image,
			})
			handler := executorHandler(srv)

			if connectAddr != "" {
				return serveReverseDial(connectAddr, pinnedFingerprint, enrollToken, credentialFile, wd, handler)
			}
			return serveUnixSocket(socketPath, wd, handler)
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "path to the unix socket to listen on")
	cmd.Flags().StringVar(&connectAddr, "connect", "", "daemon executor endpoint to dial (host:port)")
	cmd.Flags().StringVar(&root, "root", "", "working directory root (defaults to the current directory)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 6, "maximum concurrent tool calls")
	cmd.Flags().StringVar(&enrollToken, "enroll-token", os.Getenv("RAFIKI_ENROLL_TOKEN"),
		"one-time enrollment token, required on first --connect")
	cmd.Flags().StringVar(&credentialFile, "credential-file", "",
		"where the durable credential is stored after enrollment")
	cmd.Flags().StringVar(&pinnedFingerprint, "pin-cert", "",
		"SHA-256 fingerprint of the daemon's leaf certificate")
	cmd.Flags().StringVar(&isolation, "isolation", "none", "isolation this executor provides: none|container")
	cmd.Flags().StringVar(&workspaceMode, "workspace-mode", "pinned",
		"pinned (expose --root) or ephemeral (construct a workspace per child)")
	cmd.Flags().StringVar(&image, "image", "", "container image for --isolation container")

	return cmd
}

func serveReverseDial(addr, pinCert, enrollToken, credentialFile, wd string, handler http.Handler) error {
	credFile := credentialFile
	if credFile == "" {
		credFile = filepath.Join(wd, ".rafiki-executor-credential")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := execpool.Connect(ctx, execpool.ConnectOptions{
		Addr:           addr,
		PinCert:        pinCert,
		EnrollToken:    enrollToken,
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

// ─── serve-stdio ───────────────────────────────────────────────────────────────

func newExecutorServeStdioCmd() *cobra.Command {
	var (
		root        string
		concurrency int
	)

	cmd := &cobra.Command{
		Use:   "serve-stdio",
		Short: "Serve tools on stdin/stdout (used inside a container workspace)",
		Long: `Serve the executor's tool surface over stdin and stdout.

This is how a container workspace runs tools. The container has no network at
all — the daemon derives the grant with Network: "none" — so there is no socket
to listen on; the outer executor reaches this process through the stdio of a
` + "`docker exec -i`" + ` and speaks the same HTTP/2 it would over TLS.

stdout is the wire. Every diagnostic goes to stderr.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			wd, err := resolveRoot(root)
			if err != nil {
				return err
			}

			// Isolation is fixed at "none" and the workspace mode at "pinned",
			// not taken from a flag: this process is already inside the box, and an
			// inner server that provisioned containers of its own would be both
			// wrong and, with no docker socket in the workspace, a confusing
			// failure at the first Provision rather than here.
			srv := executor.NewServer(executor.Options{
				Root:          wd,
				Concurrency:   concurrency,
				Version:       version.String(),
				Isolation:     "none",
				WorkspaceMode: "pinned",
			})

			// No signal handling. Teardown is the outer executor killing the
			// `docker exec` process, which closes our stdin and makes ServeConn
			// return on EOF; a SIGTERM handler would race that for no gain.
			slog.Info("executor serving on stdio", "root", wd, "version", version.String())
			if err := execpool.ServeInverted(
				execpool.NewStdioConn(os.Stdin, os.Stdout), executorHandler(srv)); err != nil {
				return fmt.Errorf("serve-stdio: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&root, "root", "", "working directory root (defaults to the current directory)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 6, "maximum concurrent tool calls")

	return cmd
}
