// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// A bodiless 404 from net/http becomes CodeUnimplemented in Connect. That is
// what an old rafikid produces, and the diagnostic must say so rather than
// leaving the user with "unimplemented: 404 Not Found".
func TestDiagnoseUnimplementedNamesAnOldDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	err := diagnoseConnectError(
		connect.NewError(connect.CodeUnimplemented, errors.New("404 Not Found")),
		"/run/rafiki/controller.sock",
	)
	msg := err.Error()
	c.False(!strings.Contains(msg, "predates") && !strings.Contains(msg, "older"), "want a message about an out-of-date daemon, got: %s", msg)
	c.StrContains(msg, "rafikid", "want the message to name rafikid, got")
}

func TestDiagnoseUnavailableNamesTheSocket(t *testing.T) {
	err := diagnoseConnectError(
		connect.NewError(connect.CodeUnavailable, errors.New("dial unix: connect: no such file or directory")),
		"/run/rafiki/controller.sock",
	)
	assert.NewAborting(t).StrContains(err.Error(), "/run/rafiki/controller.sock", "want the socket path in the message, got: %s", err)
}

// A real Connect error carries a body and a real code. It must pass through
// unchanged so the user sees the daemon's own answer.
func TestDiagnoseNotFoundPassesThrough(t *testing.T) {
	orig := connect.NewError(connect.CodeNotFound, errors.New("no such child"))
	got := diagnoseConnectError(orig, "/run/rafiki/controller.sock")
	assert.NewAborting(t).StrContains(got.Error(), "no such child", "want the original message preserved, got: %s", got)
}

func TestConnectHTTPClientIsNotNil(t *testing.T) {
	assert.NewAborting(t).NotNil(connectHTTPClient("/tmp/nope.sock"), "connectHTTPClient returned nil")
}

// An Unauthenticated carries the recovery path for the one case it now names:
// a token the daemon refuses may be stale — or the machine may never have
// logged in — and `rafiki login` mints a new one. (The verb predates this
// diagnostic; the hint rides the shared error path so every verb advertises it.)
func TestDiagnoseUnauthenticatedSuggestsLogin(t *testing.T) {
	err := diagnoseConnectError(
		connect.NewError(connect.CodeUnauthenticated, errors.New("invalid auth token")),
		"https://rafiki.example.net",
	)
	msg := err.Error()
	c := assert.NewAborting(t)
	c.StrContains(msg, "rafiki login", "want the login hint in the message, got: %s", msg)
	c.StrContains(msg, "https://rafiki.example.net", "want the endpoint named, got: %s", msg)
	c.StrContains(msg, "invalid auth token", "want the daemon's reason preserved, got: %s", msg)
}

// connectEndpointFor must wrap errRemoteNoToken so a caller (the web proxy) can
// tell "no credential — sign in" apart from any other resolve failure.
func TestConnectEndpointForRemoteNoTokenWrapsSentinel(t *testing.T) {
	c := assert.NewAborting(t)
	_, err := connectEndpointFor(profile.Resolved{
		Profile: profile.Profile{Name: "remote", URL: "https://rafiki.example.net"},
	})
	c.Require().Error(err, "a tokenless remote profile must not resolve")
	c.True(errors.Is(err, errRemoteNoToken), "error must wrap errRemoteNoToken, got %v", err)
	c.StrContains(err.Error(),
		`profile "remote" names a remote daemon but has no token`,
		"the original message prefix must still be present")
}

// TestDiagnoseUnavailableSurfacesTheDaemonsMessage is the ruling that a
// daemon-authored Unavailable keeps its own words: an unwired backend, or a
// spawn refused for a missing RAFIKI_SECRETS_KEY, is the daemon TELLING the
// operator what is wrong. Answering "is rafikid running?" would send them to
// check a process that is plainly alive.
func TestDiagnoseUnavailableSurfacesTheDaemonsMessage(t *testing.T) {
	c := assert.NewAborting(t)
	orig := connect.NewWireError(connect.CodeUnavailable,
		errors.New("spawn refused: RAFIKI_SECRETS_KEY is not set"))
	err := diagnoseConnectError(orig, "/run/rafiki/controller.sock")
	c.Eq("the rafiki daemon at /run/rafiki/controller.sock reports: spawn refused: RAFIKI_SECRETS_KEY is not set",
		err.Error(), "a daemon-authored Unavailable")
	c.NotStrContains(err.Error(), "is rafikid running", "the reachability guess must not replace the daemon's reason")
}

// TestDiagnoseUnavailableKeepsTheHintForATransportFailure is the other branch:
// a code the CLIENT synthesized (a refused dial, an unrouted proxy) never
// reached a daemon, so the hint is the whole diagnosis and the cause stays
// wrapped for errors.Is.
func TestDiagnoseUnavailableKeepsTheHintForATransportFailure(t *testing.T) {
	c := assert.NewAborting(t)
	orig := connect.NewError(connect.CodeUnavailable, errors.New("dial unix: connect: connection refused"))
	err := diagnoseConnectError(orig, "/run/rafiki/controller.sock")
	c.StrContains(err.Error(), "cannot reach the rafiki daemon at /run/rafiki/controller.sock",
		"a transport failure keeps the reachability advice, got: %s", err)
	c.StrContains(err.Error(), "is rafikid running", "error text")
	c.StrContains(err.Error(), "connection refused", "the transport cause must survive")
	c.ErrorIs(err, orig, "the original error is no longer wrapped")
}

// TestDiagnoseUnavailableFallsBackWhenTheDaemonSaysNothing pins the boundary of
// the branch above: a wire error with an empty message has nothing to surface,
// so the reachability text (which always says something) wins.
func TestDiagnoseUnavailableFallsBackWhenTheDaemonSaysNothing(t *testing.T) {
	c := assert.NewAborting(t)
	err := diagnoseConnectError(connect.NewWireError(connect.CodeUnavailable, errors.New("  ")), "sock")
	c.StrContains(err.Error(), "is rafikid running", "an empty daemon message must not swallow the hint")
}
