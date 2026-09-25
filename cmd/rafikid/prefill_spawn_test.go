package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
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
			err := validatePrefill(tc.req)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validatePrefill: want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validatePrefill: want %q, got nil", tc.want)
			}
			var ce *control.ControllerError
			if !errors.As(err, &ce) {
				t.Fatalf("want *control.ControllerError, got %T: %v", err, err)
			}
			if ce.Code != protocol.ErrInvalidArgs {
				t.Errorf("Code = %v, want ErrInvalidArgs", ce.Code)
			}
			if ce.Message != tc.want {
				t.Errorf("Message = %q, want %q", ce.Message, tc.want)
			}
		})
	}
}

// TestPrefillSpawnCallsValidate is the test that dies if the validatePrefill
// call site in Controller.Spawn is deleted: a claude-kind spawn with a pre-fill
// must be refused through the real Spawn entry point with ErrInvalidArgs.
func TestPrefillSpawnCallsValidate(t *testing.T) {
	t.Parallel()
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
	if err == nil {
		t.Fatal("Spawn with a pre-fill on a claude child must be refused")
	}
	var ce *control.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("want *control.ControllerError, got %T: %v", err, err)
	}
	if ce.Code != protocol.ErrInvalidArgs {
		t.Errorf("Code = %v, want ErrInvalidArgs", ce.Code)
	}
	if ce.Message != "prefill: only kind fundi supports a pre-fill" {
		t.Errorf("Message = %q", ce.Message)
	}
}

// TestPrefillArgvRoundTrip follows TestArgvRoundTripsIntoRuntimeOptions: a
// pre-fill must survive buildAgentArgv → parseAgentFlags → toRuntimeOptions
// unchanged, or the in-process child silently runs without it.
func TestPrefillArgvRoundTrip(t *testing.T) {
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
	if err != nil {
		t.Fatalf("parseAgentFlags(%q): %v", argv[1:], err)
	}
	got, err := f.toRuntimeOptions(req.Cwd, nil, false, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if !reflect.DeepEqual(got.Prefill, want) {
		t.Errorf("Prefill = %+v, want %+v", got.Prefill, want)
	}

	// The no-prefill default: no --prefill flag, no entries.
	argv = buildAgentArgv(protocol.SpawnRequest{Kind: protocol.KindFundi, Cwd: req.Cwd, Model: req.Model}, "c_noprefill", t.TempDir())
	f, err = parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	got, err = f.toRuntimeOptions(req.Cwd, nil, false, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Prefill != nil {
		t.Errorf("Prefill = %+v, want nil without --prefill", got.Prefill)
	}
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
	if !reflect.DeepEqual(req.Prefill, want) {
		t.Errorf("resume request Prefill = %+v, want %+v", req.Prefill, want)
	}
}
