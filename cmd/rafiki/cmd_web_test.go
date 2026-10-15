// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/webui"

	"github.com/multigres/testkit/assert"
)

// A Go-only build embeds only pkg/webui/dist/.gitkeep, so HasUI is false and
// `rafiki web` must refuse with the make-web hint. Guarded: if `make web` has
// been run in this checkout HasUI is true (index.html is embedded), and the
// no-UI path is then covered by pkg/webui's own HasUI/index.html test rather
// than here.
func TestWebRefusesWithoutUI(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	if webui.HasUI() {
		t.Skip("assets built in this checkout; the true (no-UI) refusal is covered by pkg/webui's HasUI test")
	}

	cmd := newWebCmd()
	cmd.SetOut(&bytes.Buffer{})
	err := cmd.RunE(cmd, nil)
	c.Require().Error(err, "rafiki web with no embedded UI must refuse")
	c.Eq("this rafiki was built without the web UI (make web)", err.Error(), "refusal text")
}

// The proxy's resolver must turn the one non-dialable state — a remote profile
// with no credential — into webui.ErrNoCredential, which the UI renders as
// "sign in". Every other error must pass through untouched.
func TestWebResolverMapsRemoteNoTokenToErrNoCredential(t *testing.T) {
	c := assert.NewAborting(t)

	_, err := webResolver(profile.Resolved{
		Profile: profile.Profile{Name: "remote", URL: "https://example.invalid"},
	})()
	c.Require().Error(err, "a tokenless remote profile must not resolve")
	c.True(errors.Is(err, webui.ErrNoCredential), "error must wrap webui.ErrNoCredential, got %v", err)
}

// After Complete the proxy must forward with the freshly minted token: the
// rebuilt Upstream's client sends `Authorization: Bearer <new token>` to the
// daemon's control plane.
func TestWebLoginCompleteRebuildsUpstreamWithNewToken(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// One server hosts both the Login service and (a stub) Control service,
	// because a profile names ONE endpoint and webLogin derives both from it.
	var mu sync.Mutex
	var auths []string
	mux := http.NewServeMux()
	loginPath, loginHandler := rafikiv1connect.NewLoginHandler(newFakeLogin(
		nil,
		&rafikiv1.CompleteLoginResponse{Token: "sk-web-new", Username: "ada"},
	))
	mux.Handle(loginPath, epochResponseHeader(loginHandler))
	mux.Handle("/rafiki.v1.Control/", epochResponseHeader(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := profile.Resolved{Profile: profile.Profile{Name: "web", URL: srv.URL}}
	up, err := webLogin{p: p}.Complete(context.Background(), "l-web", "code=abc&state=s")
	c.Require().NoError(err, "Complete")
	c.Eq("sk-web-new", profile.ReadToken("web"), "the new token is written to the profile")

	req, err := http.NewRequest(http.MethodPost, up.BaseURL+"/rafiki.v1.Control/GetRateLimitStatus", nil)
	c.Require().NoError(err, "build request")
	resp, err := up.Client.Do(req)
	c.Require().NoError(err, "dial the control server through the rebuilt upstream")
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	c.Require().Len(auths, 1, "the rebuilt upstream must have dialled the control server once")
	c.Eq("Bearer sk-web-new", auths[0], "the upstream client must carry the freshly minted token")
}

// The launch URL is the contract the browser (and the user) depends on: it is
// the first thing on stdout, on 127.0.0.1, carrying a single-use launch code in
// the fragment. The command is driven with a cancellable context so it stops
// the instant the line is read.
func TestWebPrintsURLFirstLine(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	cmd := newWebCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(pw)
	cmd.SetErr(io.Discard)
	c.Require().NoError(cmd.Flags().Set("no-open", "true"), "set --no-open")
	c.Require().NoError(cmd.Flags().Set("dev-assets", "http://127.0.0.1:1"), "set --dev-assets")
	c.Require().NoError(cmd.Flags().Set("port", "0"), "set --port")

	done := make(chan error, 1)
	go func() { done <- cmd.RunE(cmd, nil) }()

	line, err := bufio.NewReader(pr).ReadString('\n')
	c.Require().NoError(err, "read the first stdout line")
	cancel()

	c.True(regexp.MustCompile(`^rafiki web: http://127\.0\.0\.1:\d+/#k=[A-Za-z0-9_-]{43}\n$`).MatchString(line),
		"first stdout line = %q", line)

	select {
	case err := <-done:
		c.Require().NoError(err, "RunE after the context was cancelled")
	case <-time.After(10 * time.Second):
		t.Fatal("rafiki web did not stop after its context was cancelled")
	}
}
