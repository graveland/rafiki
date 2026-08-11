package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestNoExecutorSocketMeansEveryToolStaysLocal(t *testing.T) {
	c := &Controller{}
	got, err := c.resolveExecutor(protocol.SpawnRequest{})
	if err != nil {
		t.Fatalf("an unconfigured executor is the default, not an error: %v", err)
	}
	if got != nil {
		t.Fatal("no socket must mean a nil client, preserving today's in-process behaviour exactly")
	}
}

// A socket that is not there must fail the SPAWN, loudly. Falling back to
// in-process would silently run the child's bash and edits on the daemon's own
// machine — the exact confinement the caller asked for, quietly discarded.
func TestUnreachableExecutorSocketFailsTheSpawn(t *testing.T) {
	c := &Controller{}
	_, err := c.resolveExecutor(protocol.SpawnRequest{ExecutorSocket: "/definitely/not/a/socket"})
	if err == nil {
		t.Fatal("an unreachable executor must refuse the spawn, never fall back to local")
	}
	if !strings.Contains(err.Error(), "/definitely/not/a/socket") {
		t.Errorf("the error must name the socket: %v", err)
	}
}
