// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/version"
)

// TestVersionShowsClientAndServer pins the whole point of the verb: the
// client's own build, then the daemon's fetched over Status. The stub's
// version is deliberately unlike anything the client could report, so a
// "server:" line that merely echoes the client's version cannot pass.
func TestVersionShowsClientAndServer(t *testing.T) {
	stub := &stubControl{statusVersion: "daemon-1a2b3c4"}
	serveStubControl(t, stub)

	out, _, err := runCmd(t, newRootCmd(), "version")
	if err != nil {
		t.Fatalf("version failed: %v\noutput: %s", err, out)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("version printed %d lines, want client+server:\n%s", len(lines), out)
	}
	if want := "client: " + version.String(); lines[0] != want {
		t.Errorf("first line = %q, want %q", lines[0], want)
	}
	if lines[1] != "server: daemon-1a2b3c4" {
		t.Errorf("second line = %q, want the daemon's own version", lines[1])
	}
	if stub.statusCalls != 1 {
		t.Errorf("Status called %d times, want 1", stub.statusCalls)
	}
}

// TestVersionEmptyServerVersionReadsUnknown: a daemon that reports no version
// must not render a bare "server:" line.
func TestVersionEmptyServerVersionReadsUnknown(t *testing.T) {
	stub := &stubControl{statusVersion: ""}
	serveStubControl(t, stub)

	out, _, err := runCmd(t, newRootCmd(), "version")
	if err != nil {
		t.Fatalf("version failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "server: unknown\n") {
		t.Errorf("output missing \"server: unknown\":\n%s", out)
	}
}

// TestVersionDeadDaemonKeepsClientLine: the client line is written before the
// dial, so an unreachable daemon still reports the client's version on stdout
// and fails the command with the diagnosed connect error (main renders that
// to stderr, exit 1). The profile resolves to a socket nothing listens on.
func TestVersionDeadDaemonKeepsClientLine(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	sock := filepath.Join(t.TempDir(), "nothing.sock")
	saveProfile(t, profile.Profile{Name: "scratch", Socket: sock})

	out, _, err := runCmd(t, newRootCmd(), "version")
	if err == nil {
		t.Fatalf("version against a dead daemon must fail, output:\n%s", out)
	}
	if !strings.Contains(out, "client: "+version.String()) {
		t.Errorf("stdout missing the client line despite the failed dial:\n%s", out)
	}
	if !strings.Contains(err.Error(), "cannot reach the rafiki daemon") {
		t.Errorf("error is not the diagnosed connect failure: %v", err)
	}
}

// TestVersionStatusErrorSurfaces: a daemon-side Status failure (here
// Unauthenticated, the shape a stale token produces) is not swallowed into a
// blank server line.
func TestVersionStatusErrorSurfaces(t *testing.T) {
	stub := &stubControl{statusErr: connect.NewError(connect.CodeUnauthenticated, errors.New("invalid auth token"))}
	serveStubControl(t, stub)

	out, _, err := runCmd(t, newRootCmd(), "version")
	if err == nil {
		t.Fatalf("version must fail when Status fails, output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "rejected the profile's credential") {
		t.Errorf("error is not the diagnosed Unauthenticated failure: %v", err)
	}
	if !strings.Contains(out, "client: "+version.String()) {
		t.Errorf("stdout missing the client line despite the failed Status:\n%s", out)
	}
}

// TestVersionNoArgs pins cobra.NoArgs: a stray argument is a typo, not data,
// and must fail before RunE dials anything.
func TestVersionNoArgs(t *testing.T) {
	_, _, err := runCmd(t, newRootCmd(), "version", "extra")
	if err == nil {
		t.Fatal("version accepted a stray argument")
	}
}

// TestVersionRejectsJAndJ pins that the verb validates the output shorthands
// like every other verb, even though it never renders JSON itself.
func TestVersionRejectsJAndJ(t *testing.T) {
	stub := &stubControl{statusVersion: "daemon-1a2b3c4"}
	serveStubControl(t, stub)

	_, _, err := runCmd(t, newRootCmd(), "version", "-j", "-J")
	if err == nil || !strings.Contains(err.Error(), "cannot combine -j and -J") {
		t.Fatalf("version -j -J = %v, want the combine error", err)
	}
	if stub.statusCalls != 0 {
		t.Errorf("Status called %d times on a user-input error, want 0", stub.statusCalls)
	}
}
