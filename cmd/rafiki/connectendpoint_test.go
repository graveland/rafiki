// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// newTestRoot builds a root command carrying the same persistent flags the
// real one does, so tests exercise flag plumbing rather than reimplementing it.
func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	return root
}

func TestConnectEndpointFollowsTheProfileToARemote(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}), "Save")
	c.NoError(profile.WriteToken("personal", "sk-personal"), "WriteToken")
	c.NoError(profile.SavePointer("personal"), "SavePointer")

	ep, err := newConnectEndpoint(newTestRoot())
	c.NoError(err, "newConnectEndpoint")
	c.Eq("https://rafiki.example.net", ep.baseURL, "baseURL =")
	c.Eq("https://rafiki.example.net", ep.identity, "identity")
}

func TestConnectEndpointFollowsTheProfileToASocket(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: "/tmp/work/controller.sock"},
	}}), "Save")
	c.NoError(profile.SavePointer("work"), "SavePointer")

	ep, err := newConnectEndpoint(newTestRoot())
	c.NoError(err, "newConnectEndpoint")
	c.Eq(connectUDSBaseURL, ep.baseURL, "baseURL")
	c.True(strings.HasSuffix(ep.describe, "/tmp/work/controller.sock"), "describe = %q, want the profile's own socket", ep.describe)
	c.Eq("unix:/tmp/work/controller.sock", ep.identity, "identity =")
}

func TestConnectEndpointRefusesARemoteWithNoToken(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}), "Save")
	c.NoError(profile.SavePointer("personal"), "SavePointer")

	_, err := newConnectEndpoint(newTestRoot())
	c.Error(err, "newConnectEndpoint with a tokenless remote = nil error")
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
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.dev"},
	}}), "Save")
	c.NoError(profile.WriteToken("personal", "s3cret"), "WriteToken")
	c.NoError(profile.SavePointer("personal"), "SavePointer")

	ep, err := newConnectEndpoint(newTestRoot())
	c.NoError(err, "newConnectEndpoint")

	// ep.httpClient is the exact value handed to tui.Options.HTTPClient.
	resp, err := ep.httpClient.Get(srv.URL)
	c.NoError(err, "get")
	defer resp.Body.Close()
	c.Eq("Bearer s3cret", got, "Authorization")
}

// A RoundTripper must not mutate the caller's request. Untouched by the
// profile migration — bearerTransport is constructed directly here, with no
// env vars or profile setup at all.
func TestBearerTransportDoesNotMutateTheCallersRequest(t *testing.T) {
	c := assert.NewAborting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(protocol.EpochHeader, strconv.Itoa(protocol.Epoch))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	c.NoError(err)
	rt := &bearerTransport{base: http.DefaultTransport, token: "s3cret"}
	resp, err := rt.RoundTrip(req)
	c.NoError(err, "roundtrip")
	defer resp.Body.Close()
	c.Eq("", req.Header.Get("Authorization"), "the caller's request was mutated: Authorization =")
	c.Eq("", req.Header.Get(protocol.EpochHeader), "the caller's request was mutated: Rafiki-Protocol =")
}

func TestSocketFlagIsGone(t *testing.T) {
	root := newRootCmd()
	assert.NewAborting(t).Nil(root.PersistentFlags().Lookup("socket"), "--socket is still registered; everything it expressed is a profile field now")
}
