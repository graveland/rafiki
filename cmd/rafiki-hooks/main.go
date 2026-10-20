// SPDX-License-Identifier: Apache-2.0

// Command rafiki-hooks is the webhook gateway: an internet-facing listener that
// authenticates a sender's webhook, decides whether it is worth waking an agent
// for, and reports it to rafikid over the Control plane's Notify RPC.
//
// It is a separate binary from both rafiki and rafikid on purpose. It is the
// one piece of rafiki meant to be reachable from outside, so it holds no
// database DSN, no executor, and no daemon code — only a narrowed token, one
// RPC, and the routes an operator configured.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/version"
)

func main() {
	configPath := flag.String("config", "", "path to the gateway config file (TOML); required")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "rafiki-hooks: -config is required")
		flag.Usage()
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(*configPath); err != nil {
		slog.Error("rafiki-hooks: exiting", "error", err)
		os.Exit(1)
	}
}

// run is main's body, for the error path that must be logged rather than
// panicked.
func run(configPath string) error {
	cfg, err := Load(configPath)
	if err != nil {
		return err
	}

	httpClient, err := daemonClient(cfg)
	if err != nil {
		return err
	}
	client := rafikiv1connect.NewControlClient(httpClient, cfg.Daemon.URL)

	mux := http.NewServeMux()
	// Liveness only: it touches no route, no secret and no daemon, so a
	// gateway whose daemon is down is still up.
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeText(w, http.StatusOK, "ok")
	})
	mux.Handle("/", newGateway(cfg, controlNotifier{client: client}))

	srv := &http.Server{
		Addr:              cfg.Daemon.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeoutSeconds * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("rafiki-hooks: listening",
			"addr", cfg.Daemon.Listen, "daemon", cfg.Daemon.URL,
			"routes", len(cfg.Routes), "tls", cfg.Daemon.TLSCert != "", "version", version.Version)
		var err error
		if cfg.Daemon.TLSCert != "" {
			err = srv.ListenAndServeTLS(cfg.Daemon.TLSCert, cfg.Daemon.TLSKey)
		} else {
			err = srv.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		slog.Info("rafiki-hooks: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-serveErr
	}
}

// daemonClient is the HTTP client the control plane is reached with: every
// request carries the gateway's token and the protocol epoch.
//
// Both ride the transport rather than a per-call header, for the reason
// cmd/rafiki's bearerTransport does: the *http.Client is handed to the
// generated client and never seen again, so a header set at one call site
// would miss the next.
func daemonClient(cfg *Config) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}
	transport := base.Clone()
	if cfg.Daemon.CAFile != "" {
		pem, err := os.ReadFile(cfg.Daemon.CAFile)
		if err != nil {
			return nil, fmt.Errorf("daemon.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("daemon.ca_file %s: no PEM certificate found", cfg.Daemon.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: &daemonTransport{base: transport, token: cfg.token, epoch: strconv.Itoa(protocol.Epoch)}}, nil
}

// daemonTransport attaches the gateway's credential and the protocol epoch to
// every request.
type daemonTransport struct {
	base  http.RoundTripper
	token string
	epoch string
}

func (t *daemonTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not modify the caller's request.
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	r.Header.Set(protocol.EpochHeader, t.epoch)
	return t.base.RoundTrip(r)
}
