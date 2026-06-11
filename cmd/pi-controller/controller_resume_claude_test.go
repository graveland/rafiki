package main

import (
	"strings"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/child"
	"git.graveland.dev/brent/pi-controller/protocol"
)

func TestResolveSpawnPlan_Claude(t *testing.T) {
	_, argv, prov, err := resolveSpawnPlan(protocol.SpawnRequest{
		Kind:     "claude",
		PiBinary: "/custom/claude",
		Model:    "claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := prov.(child.ClaudeProvider); !ok {
		t.Fatalf("provider = %T, want child.ClaudeProvider", prov)
	}
	if !strings.Contains(strings.Join(argv, " "), "--input-format stream-json") {
		t.Fatalf("argv missing stream-json: %v", argv)
	}
}

func TestResolveSpawnPlan_PiDefault(t *testing.T) {
	_, _, prov, err := resolveSpawnPlan(protocol.SpawnRequest{Kind: "", PiBinary: "/x/pi"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := prov.(child.PiProvider); !ok {
		t.Fatalf("empty kind should default to PiProvider, got %T", prov)
	}
}

func TestResolveSpawnPlan_UnknownKind(t *testing.T) {
	if _, _, _, err := resolveSpawnPlan(protocol.SpawnRequest{Kind: "bogus"}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}
