// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

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
