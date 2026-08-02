package main

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// The face must expose /metrics and /healthz WITHOUT a token: scrapers and
// probes do not carry client credentials. Only the LLM faces require auth.
func TestProxyFace_MetricsAndHealthzAreUnauthenticated(t *testing.T) {
	t.Setenv("FUNDI_PROXY_LISTEN", "127.0.0.1:0")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := prometheus.NewRegistry()
	face, err := startProxyFace(ctx, faceOptions{
		Logger:   slog.New(slog.DiscardHandler),
		Registry: reg,
	})
	if err != nil {
		t.Fatalf("startProxyFace: %v", err)
	}
	defer face.Close(ctx)

	for _, path := range []string{"/metrics", "/healthz"} {
		resp, err := http.Get(face.URL + path) //nolint:noctx // short-lived test request
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s (no token) = %d, want 200", path, resp.StatusCode)
		}
	}
}

// An LLM face still requires a token — /metrics being open must not have
// opened everything else.
func TestProxyFace_MessagesStillRequiresToken(t *testing.T) {
	t.Setenv("FUNDI_PROXY_LISTEN", "127.0.0.1:0")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	face, err := startProxyFace(ctx, faceOptions{
		Logger:   slog.New(slog.DiscardHandler),
		Registry: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("startProxyFace: %v", err)
	}
	defer face.Close(ctx)

	resp, err := http.Post(face.URL+"/v1/messages", "application/json", http.NoBody) //nolint:noctx // short-lived test request
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /v1/messages (no token) = %d, want 401", resp.StatusCode)
	}
}
