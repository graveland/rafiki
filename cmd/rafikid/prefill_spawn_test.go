package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// TestPrefillValidateRefusals covers validatePrefill's table: the refusals
// that remain (kind, entry shape) are verbatim, and the accepting cases —
// now including tool-less children — return nil.
func TestPrefillValidateRefusals(t *testing.T) {
	badRange := []protocol.PrefillRead{{Path: "a.md", Start: 9, End: 3}}
	globEntry := []protocol.PrefillRead{{Path: "notes/*.md"}}
	plain := []protocol.PrefillRead{{Path: "a.md"}}

	cases := []struct {
		name string
		req  protocol.SpawnRequest
		want string // empty means nil error expected
	}{
		{"empty prefill with kind claude", protocol.SpawnRequest{Kind: protocol.KindClaude}, ""},
		{"no tools restrict nothing", protocol.SpawnRequest{Prefill: plain}, ""},
		{"tools include read and glob with a glob entry", protocol.SpawnRequest{Prefill: globEntry, Tools: "read,glob"}, ""},
		// Tool-less children carry pre-fills: the reads run through the
		// engine's internal reader, so neither NoBuiltinTools nor an allowlist
		// without read/glob is a refusal any more.
		{"no builtin tools accepted", protocol.SpawnRequest{Prefill: plain, NoBuiltinTools: true}, ""},
		{"no builtin tools with a glob entry accepted", protocol.SpawnRequest{Prefill: globEntry, NoBuiltinTools: true}, ""},
		{"tools omit read accepted", protocol.SpawnRequest{Prefill: plain, Tools: "bash,edit"}, ""},
		{"glob entry, tools omit glob accepted", protocol.SpawnRequest{Prefill: globEntry, Tools: "read"}, ""},
		{"claude kind", protocol.SpawnRequest{Kind: protocol.KindClaude, Prefill: plain}, "prefill: only kind fundi supports a pre-fill"},
		{"bad range", protocol.SpawnRequest{Prefill: badRange, Tools: "read"}, "prefill: entry 1: start 9 is after end 3"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := validatePrefill(tc.req)
			if tc.want == "" {
				c.Require().NoError(err, "validatePrefill: want nil, got")
				return
			}
			c.Require().Error(err, "validatePrefill: want %q, got nil", tc.want)
			var ce *connectapi.ControllerError
			c.Require().True(errors.As(err, &ce), "want *connectapi.ControllerError, got %T: %v", err, err)
			c.Eq(protocol.ErrInvalidArgs, ce.Code, "Code")
			c.Eq(tc.want, ce.Message, "Message")
		})
	}
}

// TestPrefillSpawnCallsValidate is the test that dies if the validatePrefill
// call site in Controller.Spawn is deleted: a claude-kind spawn with a pre-fill
// must be refused through the real Spawn entry point with ErrInvalidArgs.
func TestPrefillSpawnCallsValidate(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	ctrl := newTestController(t)

	req := protocol.SpawnRequest{
		Kind:     protocol.KindClaude,
		Cwd:      t.TempDir(),
		PiBinary: fakePiBin(t),
		Prefill:  []protocol.PrefillRead{{Path: "a.md"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := ctrl.Spawn(ctx, req, users.Identity{})
	c.Require().Error(err, "Spawn with a pre-fill on a claude child must be refused")
	var ce *connectapi.ControllerError
	c.Require().True(errors.As(err, &ce), "want *connectapi.ControllerError, got %T: %v", err, err)
	c.Eq(protocol.ErrInvalidArgs, ce.Code, "Code")
	c.Eq("prefill: only kind fundi supports a pre-fill", ce.Message, "Message =")
}

// TestPrefillArgvRoundTrip follows TestArgvRoundTripsIntoRuntimeOptions: a
// pre-fill must survive buildAgentArgv → parseAgentFlags → toRuntimeOptions
// unchanged, or the in-process child silently runs without it.
func TestPrefillArgvRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	want := []protocol.PrefillRead{
		{Path: "docs/plan.md", Start: 10, End: 40},
		{Path: "notes/*"},
	}
	req := protocol.SpawnRequest{
		Kind:    protocol.KindFundi,
		Cwd:     t.TempDir(),
		Model:   "anthropic/claude-sonnet-4-5",
		Tools:   "read,glob",
		Prefill: want,
	}

	argv := buildAgentArgv(req, "c_prefill", t.TempDir())
	f, err := parseAgentFlags(argv[1:])
	c.Require().NoError(err, "parseAgentFlags(%q)", argv[1:])
	got, err := f.toRuntimeOptions(req.Cwd, nil, false, nil)
	c.Require().NoError(err, "toRuntimeOptions")
	c.EqDiff(want, got.Prefill, "Prefill")

	// The no-prefill default: no --prefill flag, no entries.
	argv = buildAgentArgv(protocol.SpawnRequest{Kind: protocol.KindFundi, Cwd: req.Cwd, Model: req.Model}, "c_noprefill", t.TempDir())
	f, err = parseAgentFlags(argv[1:])
	c.Require().NoError(err, "parseAgentFlags")
	got, err = f.toRuntimeOptions(req.Cwd, nil, false, nil)
	c.Require().NoError(err, "toRuntimeOptions")
	c.Nil(got.Prefill, "Prefill")
}

// TestPrefillSurvivesResumeRebuild pins the resume half of the plumbing: the
// SpawnRequest the resume path rebuilds must carry the snapshot's pre-fill.
// The resume sites build the SpawnRequest via the resumeRequestFromSnapshot
// helper, so testing that helper covers every one of them.
func TestPrefillSurvivesResumeRebuild(t *testing.T) {
	want := []protocol.PrefillRead{
		{Path: "docs/plan.md", Start: 10, End: 40},
		{Path: "notes/*"},
	}
	req := resumeRequestFromSnapshot(childstore.Snapshot{
		Kind:    protocol.KindFundi,
		Prefill: want,
	}, "")
	assert.NewCollecting(t).EqDiff(want, req.Prefill, "resume request Prefill")
}
