package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"

	"github.com/multigres/testkit/assert"
)

func TestResolveSpawnPlan_Claude(t *testing.T) {
	c := assert.NewAborting(t)
	_, argv, prov, err := resolveSpawnPlan(protocol.SpawnRequest{
		Kind:     "claude",
		PiBinary: "/custom/claude",
		Model:    "claude-opus-4-8",
	}, "", "", proxyenv.Values{})
	c.NoError(err, "err")
	_, ok := prov.(child.ClaudeProvider)
	c.True(ok, "provider = %T, want child.ClaudeProvider", prov)
	c.StrContains(strings.Join(argv, " "), "--input-format stream-json", "argv missing stream-json: %v", argv)
}

func TestResolveSpawnPlan_DefaultKind(t *testing.T) {
	c := assert.NewAborting(t)
	_, _, prov, err := resolveSpawnPlan(protocol.SpawnRequest{Kind: "", Model: "anthropic/test"}, "", "st", proxyenv.Values{})
	c.NoError(err, "err")
	_, ok := prov.(child.IdentityProvider)
	c.True(ok, "empty kind should default to IdentityProvider (fundi), got %T", prov)
}

func TestResolveSpawnPlan_UnknownKind(t *testing.T) {
	_, _, _, err := resolveSpawnPlan(protocol.SpawnRequest{Kind: "bogus"}, "", "", proxyenv.Values{})
	assert.NewAborting(t).Error(err, "expected error for unknown kind")
}

func TestResumeRequestFromSnapshot_Claude(t *testing.T) {
	c := assert.NewAborting(t)
	snap := childstore.Snapshot{
		Cwd:       "/tmp",
		Kind:      "claude",
		ConfigDir: "/home/u/.claude-personal",
		Model:     "claude-opus-4-8",
		SessionID: "sess-xyz",
	}
	req := resumeRequestFromSnapshot(snap, "")
	c.False(req.Kind != "claude" || req.ConfigDir != "/home/u/.claude-personal", "kind/configdir not carried: %+v", req)
	c.Eq("sess-xyz", req.ResumeSession, "claude must resume by session id, got ResumeSession=")
}

func TestResumeRequestFromSnapshot_Pi(t *testing.T) {
	snap := childstore.Snapshot{
		Cwd:         "/tmp",
		Kind:        "", // pi
		SessionFile: "/tmp/sessions/s.jsonl",
		SessionID:   "ignored-for-pi",
	}
	req := resumeRequestFromSnapshot(snap, "")
	assert.NewAborting(t).Eq("/tmp/sessions/s.jsonl", req.ResumeSession, "pi must resume by session file path, got")
}

// TestResumeRequestFromSnapshot_CarriesRecordRequests guards against the bug
// where a child spawned with --record-requests silently stopped capturing on
// its very first resume: resumeRequestFromSnapshot rebuilt ~30 spawn fields
// but dropped RecordRequests, and agentRuntimeOptions nils out ro.RawTrace
// whenever req.RecordRequests is false (see agent_runtime.go). Covers both
// values so a future change that hardcodes true cannot pass silently.
func TestResumeRequestFromSnapshot_CarriesRecordRequests(t *testing.T) {
	for _, want := range []bool{true, false} {
		snap := childstore.Snapshot{
			Cwd:            "/tmp",
			Kind:           "", // pi
			SessionFile:    "/tmp/sessions/s.jsonl",
			RecordRequests: want,
		}
		req := resumeRequestFromSnapshot(snap, "")
		assert.NewAborting(t).Eq(want, req.RecordRequests, "RecordRequests")
	}
}
