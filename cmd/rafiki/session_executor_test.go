package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
)

// The connect target is derived from the resolved PROFILE, not configured: a
// remote profile means the executor dials that daemon's TLS listener, and a
// local (socket) profile means the local daemon's unix socket. Getting this
// backwards points an executor at the wrong machine, which then serves the
// wrong filesystem.
func TestSessionExecutorConnectTarget(t *testing.T) {
	remote := profile.Resolved{Profile: profile.Profile{URL: "https://rafiki.example.dev:8443"}}
	addr, sock, err := sessionConnectTarget(remote)
	if err != nil {
		t.Fatalf("sessionConnectTarget: %v", err)
	}
	if addr != "rafiki.example.dev:8443" {
		t.Errorf("addr = %q, want the remote host:port", addr)
	}
	if sock != "" {
		t.Errorf("socket = %q, want empty for a remote daemon", sock)
	}

	local := profile.Resolved{Profile: profile.Profile{Socket: "/some/path"}}
	addr, sock, err = sessionConnectTarget(local)
	if err != nil {
		t.Fatalf("sessionConnectTarget: %v", err)
	}
	if addr != "" {
		t.Errorf("addr = %q, want empty for a local daemon", addr)
	}
	if sock == "" {
		t.Error("socket is empty; a local daemon is reached over the unix socket")
	}
}

// A durable executor (`rafiki executor serve --connect` / `service install`)
// must default its connect target from RAFIKI_URL the same way the automatic
// in-process executor already does (sessionConnectTarget) — an operator who
// already has RAFIKI_URL set everywhere else shouldn't have to separately
// derive and pass host:port by hand.
func TestResolveExecutorConnectFlags_DerivesFromRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	connect, socket, err := resolveExecutorConnectFlags("", "")
	if err != nil {
		t.Fatalf("resolveExecutorConnectFlags: %v", err)
	}
	if connect != "rafiki.example.dev:8443" {
		t.Errorf("connect = %q, want the RAFIKI_URL-derived host:port", connect)
	}
	if socket != "" {
		t.Errorf("socket = %q, want empty", socket)
	}
}

// An explicit --connect always wins over RAFIKI_URL, even when they disagree —
// the flag is what the operator typed just now.
func TestResolveExecutorConnectFlags_ExplicitConnectWinsOverRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	connect, socket, err := resolveExecutorConnectFlags("other.example.dev:9000", "")
	if err != nil {
		t.Fatalf("resolveExecutorConnectFlags: %v", err)
	}
	if connect != "other.example.dev:9000" {
		t.Errorf("connect = %q, want the explicit flag value untouched", connect)
	}
	if socket != "" {
		t.Errorf("socket = %q, want empty", socket)
	}
}

// An explicit --connect-socket must not be overridden by a RAFIKI_URL-derived
// --connect — naming the local socket transport is itself a deliberate choice.
func TestResolveExecutorConnectFlags_ExplicitSocketWinsOverRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	connect, socket, err := resolveExecutorConnectFlags("", "/tmp/rafiki-executor.sock")
	if err != nil {
		t.Fatalf("resolveExecutorConnectFlags: %v", err)
	}
	if connect != "" {
		t.Errorf("connect = %q, want empty: --connect-socket already chose the transport", connect)
	}
	if socket != "/tmp/rafiki-executor.sock" {
		t.Errorf("socket = %q, want the explicit flag value", socket)
	}
}

// Both flags given is still an error regardless of RAFIKI_URL.
func TestResolveExecutorConnectFlags_BothGivenIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	_, _, err := resolveExecutorConnectFlags("host:1234", "/tmp/x.sock")
	if err == nil {
		t.Fatal("expected an error: --connect and --connect-socket are mutually exclusive")
	}
}

// No flags and no usable RAFIKI_URL (e.g. the local proxy face's http:// URL,
// or unset) is still an error — there is nothing to derive a target from.
func TestResolveExecutorConnectFlags_NeitherGivenNorDerivableIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_URL", "")
	_, _, err := resolveExecutorConnectFlags("", "")
	if err == nil {
		t.Fatal("expected an error: neither flag was given and RAFIKI_URL derives nothing")
	}
}
