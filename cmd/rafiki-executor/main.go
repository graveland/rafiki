// rafiki-executor serves the filesystem and shell tools over a Connect RPC
// surface on a local unix socket. The daemon dispatches tool calls to it; the
// executor has no direct database or credential access.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
)

var version = "dev"

func main() {
	socketPath := flag.String("socket", "", "path to the unix socket (required)")
	root := flag.String("root", "", "working directory root (defaults to current directory)")
	concurrency := flag.Int("concurrency", 6, "maximum concurrent tool calls")
	flag.Parse()

	if *socketPath == "" {
		fmt.Fprintln(os.Stderr, "error: --socket is required")
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

	// Remove a stale socket file from a previous run.
	os.Remove(*socketPath)

	ln, err := net.Listen("unix", *socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: listen on %s: %v\n", *socketPath, err)
		os.Exit(1)
	}
	if err := os.Chmod(*socketPath, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "error: chmod %s: %v\n", *socketPath, err)
		os.Exit(1)
	}

	srv := executor.NewServer(executor.Options{
		Root:        wd,
		Concurrency: *concurrency,
		Version:     version,
	})

	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	// Protocols instead of h2c.NewHandler: h2c is deprecated, and an
	// http.Server that advertises UnencryptedHTTP2 serves prior-knowledge
	// HTTP/2 directly. Connect needs HTTP/2 for its streaming RPCs.
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{
		Handler:   mux,
		Protocols: protos,
	}

	slog.Info("executor listening", "socket", *socketPath, "root", wd, "version", version)

	// Graceful shutdown on SIGINT/SIGTERM.
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
		fmt.Fprintf(os.Stderr, "error: serve: %v\n", err)
		os.Exit(1)
	}
}
