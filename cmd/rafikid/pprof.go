package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"go.graveland.dev/rafiki/pkg/paths"
)

const defaultPprofListen = "127.0.0.1:6060"

// startPprof serves net/http/pprof on loopback, always, because a profile is
// most useful when nobody expected to need one. RAFIKI_PPROF_LISTEN overrides
// the address ("off" disables it).
//
// The handlers are mounted on a private mux rather than http.DefaultServeMux so
// importing net/http/pprof cannot leak profiles onto any other listener, and
// the bind address must be a loopback IP: profiles expose stacks, symbols and
// the command line, and this listener carries no authentication.
//
// A bind failure on the default address only logs: several daemons on one host
// (or a developer daemon beside a test daemon) cannot all hold 6060, and a
// diagnostic aid must not stop the daemon. An address the operator named
// explicitly is an error.
func startPprof(ctx context.Context, logger *slog.Logger) error {
	addr := paths.Get(paths.PprofListen)
	explicit := addr != ""
	switch addr {
	case "off":
		return nil
	case "":
		addr = defaultPprofListen
	}
	if err := requireLoopback(addr); err != nil {
		return fmt.Errorf("%s: %w", paths.PprofListen, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if !explicit {
			logger.Warn("pprof disabled: default address unavailable; set "+paths.PprofListen+" to another loopback address",
				"addr", addr, "error", err)
			return nil
		}
		return fmt.Errorf("pprof listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// No WriteTimeout: /debug/pprof/profile and /trace stream for as long as
	// the caller's ?seconds= asks.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof listener stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Info("pprof listening", "addr", ln.Addr().String())
	return nil
}

// requireLoopback refuses any bind address that is not a literal loopback IP.
// A hostname is refused too: it can resolve to a routable address.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q is not a loopback IP address (e.g. 127.0.0.1:6060)", addr)
	}
	return nil
}
