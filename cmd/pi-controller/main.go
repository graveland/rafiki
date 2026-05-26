// Package main is the pi-controller daemon entry point. It sets up
// directories, loads persisted state, starts the UDS server, and blocks
// until a signal triggers graceful shutdown.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"graveland.dev/pi-controller/internal/persist"
	"graveland.dev/pi-controller/internal/server"
	"graveland.dev/pi-controller/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("user home dir", "error", err)
		os.Exit(1)
	}

	runDir := filepath.Join(home, ".pi", "run")
	stateDir := filepath.Join(runDir, "state")
	logsDir := filepath.Join(runDir, "logs")
	socketPath := filepath.Join(runDir, "controller.sock")

	for _, dir := range []string{stateDir, logsDir} {
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

	st := store.New()
	dumper := persist.NewLogDumper(logsDir, persist.ModeOnExit)
	ctrl := NewController(st, stateDir, logsDir, socketPath, dumper)
	ctrl.loadOrphans(records)
	ctrl.startSweeper(ctx)

	slog.Info("loaded orphans", "count", len(records))

	handler := server.NewDispatch(ctrl)
	srv, err := server.Listen(socketPath, handler)
	if err != nil {
		slog.Error("listen", "socket", socketPath, "error", err)
		os.Exit(1)
	}
	slog.Info("pi-controller listening", "socket", socketPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sig := <-sigCh

	slog.Info("shutting down", "signal", sig)
	cancel() // stop the background sweeper
	if err := srv.Close(); err != nil {
		slog.Warn("server close", "error", err)
	}
	slog.Info("done")
}
