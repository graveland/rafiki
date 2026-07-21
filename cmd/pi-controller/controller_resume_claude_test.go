package main

import (
	"strings"
	"testing"

	"git.graveland.dev/brent/fundi/internal/child"
	"git.graveland.dev/brent/fundi/internal/store"
	"git.graveland.dev/brent/fundi/protocol"
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

func TestResumeRequestFromSnapshot_Claude(t *testing.T) {
	snap := store.Snapshot{
		Cwd:       "/tmp",
		Kind:      "claude",
		ConfigDir: "/home/u/.claude-personal",
		Model:     "claude-opus-4-8",
		SessionID: "sess-xyz",
	}
	req := resumeRequestFromSnapshot(snap, "")
	if req.Kind != "claude" || req.ConfigDir != "/home/u/.claude-personal" {
		t.Fatalf("kind/configdir not carried: %+v", req)
	}
	if req.ResumeSession != "sess-xyz" {
		t.Fatalf("claude must resume by session id, got ResumeSession=%q", req.ResumeSession)
	}
}

func TestResumeRequestFromSnapshot_Pi(t *testing.T) {
	snap := store.Snapshot{
		Cwd:         "/tmp",
		Kind:        "", // pi
		SessionFile: "/tmp/sessions/s.jsonl",
		SessionID:   "ignored-for-pi",
	}
	req := resumeRequestFromSnapshot(snap, "")
	if req.ResumeSession != "/tmp/sessions/s.jsonl" {
		t.Fatalf("pi must resume by session file path, got %q", req.ResumeSession)
	}
}
