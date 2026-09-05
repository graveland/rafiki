// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
)

// newTestRoot builds a root command carrying the same persistent flags the
// real one does, so tests exercise flag plumbing rather than reimplementing it.
func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	return root
}

func TestConnectSocketIsASiblingOfTheProfilesControlSocket(t *testing.T) {
	p := profile.Resolved{Profile: profile.Profile{Name: "scratch", Socket: "/tmp/scratch/controller.sock"}}
	if got, want := connectSocketFor(p), filepath.Join("/tmp/scratch", "connect.sock"); got != want {
		t.Fatalf("connectSocketFor = %q, want %q", got, want)
	}
}

func TestConnectEndpointFollowsTheProfileToARemote(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.WriteToken("personal", "sk-personal"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if err := profile.SavePointer("personal"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	ep, err := newConnectEndpoint(newTestRoot())
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.baseURL != "https://rafiki.example.net" {
		t.Fatalf("baseURL = %q", ep.baseURL)
	}
	if ep.identity != "https://rafiki.example.net" {
		t.Fatalf("identity = %q; the completion cache keys on it", ep.identity)
	}
}

func TestConnectEndpointFollowsTheProfileToASocket(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: "/tmp/work/controller.sock"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("work"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	ep, err := newConnectEndpoint(newTestRoot())
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.baseURL != connectUDSBaseURL {
		t.Fatalf("baseURL = %q, want the UDS sentinel", ep.baseURL)
	}
	if !strings.HasSuffix(ep.describe, "/tmp/work/connect.sock") {
		t.Fatalf("describe = %q, want the profile's connect socket", ep.describe)
	}
	if ep.identity != "unix:/tmp/work/connect.sock" {
		t.Fatalf("identity = %q", ep.identity)
	}
}

func TestConnectEndpointRefusesARemoteWithNoToken(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("personal"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	if _, err := newConnectEndpoint(newTestRoot()); err == nil {
		t.Fatal("newConnectEndpoint with a tokenless remote = nil error")
	}
}

// The credential rides the TRANSPORT, not the call site. The cockpit's client
// is handed to pkg/tui and never seen again, so a per-call header would
// authenticate the pre-flight and leave the event stream unauthenticated —
// failing only once the alt screen is already up.
//
// Rewritten for the profile system (this used to seed paths.URL/paths.Token
// directly): the credential now comes from a profile's token file rather than
// an env var, so the setup is isolateProfiles + a seeded remote profile +
// WriteToken, same pattern as TestConnectEndpointFollowsTheProfileToARemote.
// The assertion is unchanged — it's still ep.httpClient, the exact value
// handed to tui.Options.HTTPClient, that must carry the bearer.
func TestTheCredentialRidesEveryRequestIncludingTheTUIs(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.dev"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.WriteToken("personal", "s3cret"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if err := profile.SavePointer("personal"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	ep, err := newConnectEndpoint(newTestRoot())
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

// A RoundTripper must not mutate the caller's request. Untouched by the
// profile migration — bearerTransport is constructed directly here, with no
// env vars or profile setup at all.
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

func TestSocketFlagIsGone(t *testing.T) {
	root := newRootCmd()
	if f := root.PersistentFlags().Lookup("socket"); f != nil {
		t.Fatal("--socket is still registered; everything it expressed is a profile field now")
	}
}
