// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

type fakeLifecycle struct {
	got        connectapi.SpawnParams
	spawnErr   error
	killOut    connectapi.KillOutcome
	killErr    error
	killedID   string
	gotShutdow int64
	gotKill    int64
}

func (f *fakeLifecycle) Spawn(_ context.Context, p connectapi.SpawnParams) (string, error) {
	f.got = p
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	return "c_new", nil
}

func (f *fakeLifecycle) Kill(_ context.Context, childID string, shutdownMs, killMs int64) (connectapi.KillOutcome, error) {
	f.killedID = childID
	f.gotShutdow = shutdownMs
	f.gotKill = killMs
	if f.killErr != nil {
		return connectapi.KillOutcome{}, f.killErr
	}
	return f.killOut, nil
}

func TestSpawnPassesFieldsThrough(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/work", Name: "scout", Model: "claude-opus-5", Kind: "fundi",
		ParentChildId: "c_0", ExecutorSelector: "kind=native",
		Labels: map[string]string{"team": "a"},
	}))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if resp.Msg.GetChildId() != "c_new" {
		t.Errorf("ChildId = %q, want c_new", resp.Msg.GetChildId())
	}
	if f.got.Cwd != "/work" || f.got.Name != "scout" || f.got.Kind != "fundi" {
		t.Errorf("params wrong: %+v", f.got)
	}
	if f.got.ParentChildID != "c_0" || f.got.ExecutorSelector != "kind=native" {
		t.Errorf("lineage/selector wrong: %+v", f.got)
	}
	if f.got.Labels["team"] != "a" {
		t.Errorf("labels wrong: %+v", f.got.Labels)
	}
}

// TestSpawnUnsetBudgetsStayNil is the important one: unset must NOT become
// zero. An unset MaxCost means unlimited; a zero means "spend nothing", and
// collapsing them makes every unbudgeted agent refuse its first spawn.
func TestSpawnUnsetBudgetsStayNil(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	if _, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"})); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if f.got.MaxDepth != nil {
		t.Errorf("MaxDepth = %v, want nil", *f.got.MaxDepth)
	}
	if f.got.MaxCost != nil {
		t.Errorf("MaxCost = %v, want nil", *f.got.MaxCost)
	}
	if f.got.MaxChildren != nil {
		t.Errorf("MaxChildren = %v, want nil", *f.got.MaxChildren)
	}
}

// TestSpawnExplicitZeroBudgetsSurvive is the mirror image: an explicit zero
// must arrive as a non-nil pointer to zero.
func TestSpawnExplicitZeroBudgetsSurvive(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	zeroI := int32(0)
	zeroF := float64(0)
	if _, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/work", MaxDepth: &zeroI, MaxCost: &zeroF, MaxChildren: &zeroI,
	})); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if f.got.MaxDepth == nil || *f.got.MaxDepth != 0 {
		t.Error("explicit MaxDepth=0 was lost")
	}
	if f.got.MaxCost == nil || *f.got.MaxCost != 0 {
		t.Error("explicit MaxCost=0 was lost")
	}
	if f.got.MaxChildren == nil || *f.got.MaxChildren != 0 {
		t.Error("explicit MaxChildren=0 was lost")
	}
}

func TestSpawnRequiresCwd(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{})
	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestSpawnWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestSpawnErrorBecomesInternal(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{spawnErr: errors.New("budget exceeded")})
	_, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
}

func TestKillPassesTimeouts(t *testing.T) {
	code := 0
	f := &fakeLifecycle{killOut: connectapi.KillOutcome{ExitCode: &code, DurationMs: 12}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{
		ChildId: "c_1", ShutdownTimeoutMs: 5000, KillTimeoutMs: 2000,
	}))
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if f.killedID != "c_1" || f.gotShutdow != 5000 || f.gotKill != 2000 {
		t.Errorf("kill args wrong: id=%q shutdown=%d kill=%d", f.killedID, f.gotShutdow, f.gotKill)
	}
	if resp.Msg.GetExitCode() != 0 || resp.Msg.GetDurationMs() != 12 {
		t.Errorf("outcome wrong: exit=%d duration=%d", resp.Msg.GetExitCode(), resp.Msg.GetDurationMs())
	}
	if resp.Msg.GetChildId() != "c_1" {
		t.Errorf("ChildId = %q, want c_1", resp.Msg.GetChildId())
	}
}

func TestKillRequiresChildID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{})
	_, err := s.Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestKillWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Kill(context.Background(),
		connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_1"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable", connect.CodeOf(err))
	}
}
