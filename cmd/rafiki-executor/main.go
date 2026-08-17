// rafiki-executor serves the filesystem and shell tools over a Connect RPC
// surface, either on a local unix socket (--socket) or by reverse-dialing
// rafikid (--connect) and serving on the dialled TLS connection.
package main

import (
	"context"
	"errors"
	"flag"
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

	"golang.org/x/sys/unix"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

var version = "dev"

func main() {
	socketPath := flag.String("socket", "", "path to the unix socket (required)")
	root := flag.String("root", "", "working directory root (defaults to current directory)")
	concurrency := flag.Int("concurrency", 6, "maximum concurrent tool calls")
	connectAddr := flag.String("connect", "", "rafikid executor endpoint to dial (host:port). Mutually exclusive with --socket")
	serveStdio := flag.Bool("serve-stdio", false, "serve on stdin/stdout instead of a socket. Used inside a container workspace, where there is no network")
	enrollToken := flag.String("enroll-token", os.Getenv("RAFIKI_ENROLL_TOKEN"), "one-time enrollment token, required on first connect")
	credentialFile := flag.String("credential-file", "", "path the durable credential is stored at after enrollment")
	pinnedFingerprint := flag.String("pin-cert", "", "SHA-256 fingerprint of rafikid's leaf certificate")
	isolation := flag.String("isolation", "none", "isolation this executor provides: none|container")
	workspaceMode := flag.String("workspace-mode", "pinned", "pinned (expose --root) or ephemeral (construct per child)")
	image := flag.String("image", "", "container image for --isolation container")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	// Before the transport checks: --version is how a container workspace image
	// is probed for a usable executor binary, and it must not require naming a
	// transport it is not going to use.
	if *showVersion {
		fmt.Println(version)
		return
	}

	// Exactly one transport. Counted rather than compared pairwise: three modes
	// need three pairwise checks and a fourth would need six, and the version of
	// this that grows a mode without growing a check is the one that silently
	// ignores a flag.
	modes := 0
	for _, selected := range []bool{*socketPath != "", *connectAddr != "", *serveStdio} {
		if selected {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr, "error: --socket, --connect and --serve-stdio are mutually exclusive")
		os.Exit(2)
	}
	if modes == 0 {
		fmt.Fprintln(os.Stderr, "error: one of --socket, --connect or --serve-stdio is required")
		flag.Usage()
		os.Exit(2)
	}
	if *serveStdio && *isolation == "container" {
		// A stdio server is already INSIDE the container. Honouring this would
		// have it try to start containers of its own, which is both wrong and,
		// with no docker socket in the workspace, a confusing failure at the
		// first Provision rather than here.
		fmt.Fprintln(os.Stderr, "error: --serve-stdio cannot be combined with --isolation container: "+
			"the stdio server runs inside a container workspace and provisions none of its own")
		os.Exit(2)
	}
	if *isolation == "container" && *image == "" {
		fmt.Fprintln(os.Stderr, "error: --isolation container requires --image")
		os.Exit(2)
	}
	if *isolation == "none" && *workspaceMode == "ephemeral" {
		fmt.Fprintln(os.Stderr, "error: --isolation none does not support --workspace-mode ephemeral")
		os.Exit(2)
	}

	wd := *root
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot get working directory: %v\n", err)
			os.Exit(1)
		}
	}
	if !filepath.IsAbs(wd) {
		abs, err := filepath.Abs(wd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot resolve root: %v\n", err)
			os.Exit(1)
		}
		wd = abs
	}

	srv := executor.NewServer(executor.Options{
		Root:          wd,
		Concurrency:   *concurrency,
		Version:       version,
		Isolation:     *isolation,
		WorkspaceMode: *workspaceMode,
		Image:         *image,
	})

	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := mux

	if *serveStdio {
		// stdout is THE WIRE. Anything else written to it corrupts HTTP/2 framing
		// and surfaces on the outer side as an unreadable protocol error rather
		// than as "something printed to stdout", so every diagnostic on this path
		// must go to stderr. slog's default handler already does; the checks
		// above use os.Stderr explicitly for the same reason.
		//
		// Isolation is necessarily "none" and workspace_mode "pinned" here — the
		// validation above refuses container isolation outright and rejects
		// ephemeral on none — which is what we want: this server is already
		// inside the box and provisions no workspaces of its own.
		//
		// No signal handling. Teardown is the outer executor killing the
		// `docker exec` process, which closes our stdin and makes ServeConn
		// return on EOF. A SIGTERM handler here would race that for no gain.
		slog.Info("executor serving on stdio", "root", wd, "version", version)
		if err := execpool.ServeInverted(execpool.NewStdioConn(os.Stdin, os.Stdout), httpSrv); err != nil {
			fmt.Fprintf(os.Stderr, "error: serve-stdio: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *connectAddr != "" {
		// Reverse-dial mode: dial rafikid, serve on the dialled connection.
		credFile := *credentialFile
		if credFile == "" {
			credFile = filepath.Join(wd, ".rafiki-executor-credential")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if err := execpool.Connect(ctx, execpool.ConnectOptions{
			Addr:           *connectAddr,
			PinCert:        *pinnedFingerprint,
			EnrollToken:    *enrollToken,
			CredentialFile: credFile,
			SelfReported: map[string]string{
				"os":      runtime.GOOS,
				"arch":    runtime.GOARCH,
				"version": version,
			},
			Handler: httpSrv,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "error: connect: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Local socket mode.
	if conn, err := net.DialTimeout("unix", *socketPath, 500*time.Millisecond); err == nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "error: %s is already served by a live executor\n", *socketPath)
		os.Exit(1)
	}
	if err := os.Remove(*socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "error: cannot remove stale socket %s: %v\n", *socketPath, err)
		os.Exit(1)
	}

	oldMask := unix.Umask(0o177)
	ln, err := net.Listen("unix", *socketPath)
	unix.Umask(oldMask)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: listen on %s: %v\n", *socketPath, err)
		os.Exit(1)
	}

	h2Server := &http.Server{Handler: httpSrv, Protocols: protos}
	slog.Info("executor listening", "socket", *socketPath, "root", wd, "version", version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		slog.Info("executor shutting down")
		if err := h2Server.Shutdown(context.Background()); err != nil {
			slog.Warn("executor shutdown", "error", err)
		}
	}()

	if err := h2Server.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "error: serve: %v\n", err)
		os.Exit(1)
	}
}
