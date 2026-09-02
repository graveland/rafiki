// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
)

// With no RAFIKI_URL the cockpit uses the local socket, exactly as before.
func TestEndpointDefaultsToTheLocalSocket(t *testing.T) {
	t.Setenv(paths.URL, "")
	ep, err := newConnectEndpoint(nil)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.baseURL != connectUDSBaseURL {
		t.Fatalf("baseURL = %q, want the UDS sentinel", ep.baseURL)
	}
	if ep.describe != paths.ConnectSocketPath() {
		t.Fatalf("describe = %q, want the socket path", ep.describe)
	}
}

// The bug this change closes: `rafiki list` honoured RAFIKI_URL and the
// cockpit dialled a socket on the operator's laptop, so attach failed with
// "no such file or directory" while every other verb worked.
func TestRemoteURLIsHonoured(t *testing.T) {
	t.Setenv(paths.URL, "https://rafiki.example.dev")
	t.Setenv(paths.Token, "s3cret")
	ep, err := newConnectEndpoint(nil)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.baseURL != "https://rafiki.example.dev" {
		t.Fatalf("baseURL = %q, want the remote URL", ep.baseURL)
	}
	if strings.Contains(ep.describe, ".sock") {
		t.Fatalf("describe = %q, want the URL rather than a socket path", ep.describe)
	}
}

// An http:// RAFIKI_URL is the loopback PROXY face, which has no control
// plane. Treating it as remote would aim the cockpit at the wrong surface —
// the same gate mustDial applies, shared rather than duplicated.
func TestPlaintextURLStaysLocal(t *testing.T) {
	t.Setenv(paths.URL, "http://localhost:8035")
	ep, err := newConnectEndpoint(nil)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.baseURL != connectUDSBaseURL {
		t.Fatalf("baseURL = %q, want the local socket for an http:// URL", ep.baseURL)
	}
}

// There is no bootstrap mode on this plane, so an absent token can only ever
// produce a 401. Saying so before the round trip beats saying so after.
func TestRemoteWithoutATokenFailsWithAnActionableMessage(t *testing.T) {
	t.Setenv(paths.URL, "https://rafiki.example.dev")
	t.Setenv(paths.Token, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no token FILE either
	_, err := newConnectEndpoint(nil)
	if err == nil {
		t.Fatal("want an error when a remote endpoint has no credential")
	}
	if !strings.Contains(err.Error(), paths.Token) {
		t.Fatalf("want the message to name %s, got: %v", paths.Token, err)
	}
}

// The credential rides the TRANSPORT, not the call site. The cockpit's client
// is handed to pkg/tui and never seen again, so a per-call header would
// authenticate the pre-flight and leave the event stream unauthenticated —
// failing only once the alt screen is already up.
func TestTheCredentialRidesEveryRequestIncludingTheTUIs(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv(paths.URL, "https://rafiki.example.dev")
	t.Setenv(paths.Token, "s3cret")
	ep, err := newConnectEndpoint(nil)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}

	// ep.httpClient is the exact value handed to tui.Options.HTTPClient.
	resp, err := ep.httpClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if got != "Bearer s3cret" {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}
}

// A RoundTripper must not mutate the caller's request.
func TestBearerTransportDoesNotMutateTheCallersRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := &bearerTransport{base: http.DefaultTransport, token: "s3cret"}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	if h := req.Header.Get("Authorization"); h != "" {
		t.Fatalf("the caller's request was mutated: Authorization = %q", h)
	}
}

// A --socket override must move the Connect socket with it. connect.sock sits
// BESIDE the framed control socket by construction (paths pins them as
// siblings in RuntimeDir), so a command pointed at a scratch daemon's
// controller.sock must query THAT daemon's Connect plane — not the default
// runtime path. newConnectEndpoint used to discard its cmd entirely, so
// `rafiki --socket /scratch/controller.sock models` and every migrated
// completion silently queried the default daemon.
func TestEndpointHonorsTheSocketOverride(t *testing.T) {
	t.Setenv(paths.URL, "")
	t.Setenv(paths.Socket, "")

	cmd := &cobra.Command{}
	cmd.Flags().String("socket", "", "")
	if err := cmd.Flags().Set("socket", "/tmp/scratch-1/rafiki/controller.sock"); err != nil {
		t.Fatal(err)
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	want := "/tmp/scratch-1/rafiki/connect.sock"
	if ep.describe != want {
		t.Errorf("describe = %q, want the sibling connect.sock %q", ep.describe, want)
	}
	if ep.identity != "unix:"+want {
		t.Errorf("identity = %q, want %q", ep.identity, "unix:"+want)
	}
}

// The env override moves the Connect socket the same way: RAFIKI_SOCKET wins
// over the XDG default for framed dials (client.DefaultSocketPath), so the
// Connect plane is its sibling.
func TestEndpointHonorsTheSocketEnvOverride(t *testing.T) {
	t.Setenv(paths.URL, "")
	t.Setenv(paths.Socket, "/tmp/env-scratch/rafiki/controller.sock")

	ep, err := newConnectEndpoint(nil)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.describe != "/tmp/env-scratch/rafiki/connect.sock" {
		t.Errorf("describe = %q, want the sibling of the env-override socket", ep.describe)
	}
}

// The default case is unchanged: no RAFIKI_URL, no override — the canonical
// ConnectSocketPath, and the identity stays the "unix:"-prefixed key the
// completion cache has always written.
func TestEndpointIdentityDefaultsToTheCanonicalConnectSocket(t *testing.T) {
	t.Setenv(paths.URL, "")
	t.Setenv(paths.Socket, "")
	ep, err := newConnectEndpoint(nil)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.describe != paths.ConnectSocketPath() {
		t.Fatalf("describe = %q, want %q", ep.describe, paths.ConnectSocketPath())
	}
	if ep.identity != "unix:"+paths.ConnectSocketPath() {
		t.Fatalf("identity = %q, want %q", ep.identity, "unix:"+paths.ConnectSocketPath())
	}
}
