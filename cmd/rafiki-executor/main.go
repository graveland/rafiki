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
	enrollToken := flag.String("enroll-token", os.Getenv("RAFIKI_ENROLL_TOKEN"), "one-time enrollment token, required on first connect")
	credentialFile := flag.String("credential-file", "", "path the durable credential is stored at after enrollment")
	pinnedFingerprint := flag.String("pin-cert", "", "SHA-256 fingerprint of rafikid's leaf certificate")
	flag.Parse()

	if *socketPath != "" && *connectAddr != "" {
		fmt.Fprintln(os.Stderr, "error: --socket and --connect are mutually exclusive")
		os.Exit(2)
	}
	if *socketPath == "" && *connectAddr == "" {
		fmt.Fprintln(os.Stderr, "error: one of --socket or --connect is required")
		flag.Usage()
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
		Root:        wd,
		Concurrency: *concurrency,
		Version:     version,
	})

	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := mux

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
