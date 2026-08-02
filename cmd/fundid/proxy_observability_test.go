package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

// A token named in the config file is accepted by the face, alongside the
// per-boot child secret.
//
// This does NOT assert on status code alone: a request with a token our own
// middleware accepts still gets forwarded upstream (ANTHROPIC_API_KEY is a
// fake "test-key" here), and a live network path to api.anthropic.com — which
// this environment has — legitimately returns its own 401 for the bad
// upstream key. That is indistinguishable from OUR middleware's 401 by status
// code alone. So the assertion instead checks the response body does not
// carry StaticTokenAuth's own rejection text (see pkg/server/statictoken.go),
// which is the one thing that can only come from OUR middleware refusing the
// token, never from an upstream response.
func TestProxyFace_ConfigTokenIsAccepted(t *testing.T) {
	t.Setenv("FUNDI_PROXY_LISTEN", "127.0.0.1:0")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	face, err := startProxyFace(ctx, faceOptions{
		Logger:   slog.New(slog.DiscardHandler),
		Registry: prometheus.NewRegistry(),
		Config:   Config{Tokens: map[string]string{"sentinel": "tok-sentinel"}},
	})
	if err != nil {
		t.Fatalf("startProxyFace: %v", err)
	}
	defer face.Close(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, face.URL+"/v1/messages", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-sentinel")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && strings.Contains(string(body), "unknown token") {
		t.Errorf("a token named in the config file was rejected as unauthorized: %s", body)
	}
}

// A config file naming a client "fundi-child" must not be allowed to silently
// overwrite the per-boot child secret: every real spawned child would then
// present the true per-boot token and be rejected as "unknown token". This
// must fail at startup, loudly, naming the offending key — not resolve the
// collision quietly in either direction.
func TestStartProxyFace_ReservedChildTokenNameIsRejected(t *testing.T) {
	t.Setenv("FUNDI_PROXY_LISTEN", "127.0.0.1:0")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	face, err := startProxyFace(ctx, faceOptions{
		Logger:   slog.New(slog.DiscardHandler),
		Registry: prometheus.NewRegistry(),
		Config:   Config{Tokens: map[string]string{"fundi-child": "whatever"}},
	})
	if face != nil {
		defer face.Close(ctx)
	}
	if err == nil {
		t.Fatal("startProxyFace with a config token named \"fundi-child\" should fail, got nil error")
	}
	if !strings.Contains(err.Error(), "fundi-child") {
		t.Errorf("error %q should name the offending key %q", err.Error(), "fundi-child")
	}
}
