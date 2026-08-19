package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

// serveExecutorUDS accepts executor connections from this machine over a unix
// socket, using the same upgrade handler the TLS listener mounts.
//
// The same handler, deliberately: this repo has twice shipped a correct guard
// that a second code path routed around, and two accept paths for one
// privileged surface is exactly that shape. Enrollment, credential checking,
// labels and admission are all downstream of the handler and therefore
// identical on both transports — the socket changes who can reach the daemon,
// never what the daemon will accept.
//
// Note this does NOT use execpool.Pool.Serve: that starts its own park sweeper,
// and the daemon runs exactly one for the pool regardless of how many listeners
// feed it.
//
// p may be nil in tests that only exercise the socket's lifecycle; the route is
// then simply not mounted.
func serveExecutorUDS(ctx context.Context, p *execpool.Pool, path string) (net.Listener, error) {
	// Refuse rather than clobber. Two daemons serving one path means the second
	// bind silently wins and the first's executors connect into a void.
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s is already served by a live daemon", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot remove stale socket %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	// Umask before bind, not chmod after: chmod leaves a window in which
	// another local user can connect, and anyone who can connect can attempt
	// enrollment.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}

	mux := http.NewServeMux()
	if p != nil {
		mux.Handle(upgradeconn.PathFor(upgradeconn.Executor), p.UpgradeHandler())
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
			slog.Error("executor unix listener stopped", "path", path, "error", err)
		}
	}()
	return ln, nil
}
