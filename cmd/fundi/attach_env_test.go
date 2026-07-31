package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// TestAttachEnv_SocketReachesChild spawns a real subprocess and reads the value
// back, rather than asserting on the slice attachEnv returns.
//
// The point is the dedup guarantee: the socket path is appended to an
// environment that may already carry the variable, and the child must see the
// appended value. os/exec documents that the last entry for a repeated key
// wins, but getenv(3) itself returns the *first* match — so if that dedup ever
// stopped happening, the child would silently read the stale inherited path and
// dial the wrong daemon. Only an actual exec proves which value lands.
func TestAttachEnv_SocketReachesChild(t *testing.T) {
	t.Setenv(paths.Socket, "/inherited/stale.sock")

	const want = "/tmp/resolved-by-parent.sock"
	cmd := exec.Command("sh", "-c", "printenv "+paths.Socket)
	cmd.Env = attachEnv(want)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run child: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("child saw %s=%q, want %q (the appended value must win over the inherited one)", paths.Socket, got, want)
	}
}

// TestResolvedSocket_NeverEmpty guards the reason resolvedSocket exists: a path
// handed to another process cannot be "", because that other process would
// resolve its own default, which is exactly the bug this replaced.
func TestResolvedSocket_NeverEmpty(t *testing.T) {
	root := newRootCmd()
	root.SetOut(io.Discard) // --help below would otherwise dump usage into the test log
	root.SetErr(io.Discard)

	if got := resolvedSocket(root); got == "" {
		t.Fatal("resolvedSocket returned empty with no --socket set")
	}

	root.SetArgs([]string{"--socket", "/tmp/explicit.sock", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := resolvedSocket(root); got != "/tmp/explicit.sock" {
		t.Errorf("resolvedSocket = %q, want the --socket value", got)
	}
}
