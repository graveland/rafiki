// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// A bodiless 404 from net/http becomes CodeUnimplemented in Connect. That is
// what an old rafikid produces, and the diagnostic must say so rather than
// leaving the user with "unimplemented: 404 Not Found".
func TestDiagnoseUnimplementedNamesAnOldDaemon(t *testing.T) {
	err := diagnoseConnectError(
		connect.NewError(connect.CodeUnimplemented, errors.New("404 Not Found")),
		"/run/rafiki/connect.sock",
	)
	msg := err.Error()
	if !strings.Contains(msg, "predates") && !strings.Contains(msg, "older") {
		t.Fatalf("want a message about an out-of-date daemon, got: %s", msg)
	}
	if !strings.Contains(msg, "rafikid") {
		t.Fatalf("want the message to name rafikid, got: %s", msg)
	}
}

func TestDiagnoseUnavailableNamesTheSocket(t *testing.T) {
	err := diagnoseConnectError(
		connect.NewError(connect.CodeUnavailable, errors.New("dial unix: connect: no such file or directory")),
		"/run/rafiki/connect.sock",
	)
	if !strings.Contains(err.Error(), "/run/rafiki/connect.sock") {
		t.Fatalf("want the socket path in the message, got: %s", err)
	}
}

// A real Connect error carries a body and a real code. It must pass through
// unchanged so the user sees the daemon's own answer.
func TestDiagnoseNotFoundPassesThrough(t *testing.T) {
	orig := connect.NewError(connect.CodeNotFound, errors.New("no such child"))
	got := diagnoseConnectError(orig, "/run/rafiki/connect.sock")
	if !strings.Contains(got.Error(), "no such child") {
		t.Fatalf("want the original message preserved, got: %s", got)
	}
}

func TestConnectHTTPClientIsNotNil(t *testing.T) {
	if connectHTTPClient("/tmp/nope.sock") == nil {
		t.Fatal("connectHTTPClient returned nil")
	}
}
