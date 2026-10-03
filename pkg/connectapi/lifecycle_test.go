// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

type fakeLifecycle struct {
	got        connectapi.SpawnParams
	spawnErr   error
	killOut    connectapi.KillOutcome
	killErr    error
	killedID   string
	gotShutdow int64
	gotKill    int64
	closedID   string
	closeErr   error

	budgetChildID string
	budgetMaxCost float64
	budgetErr     error

	routingChildID string
	routingDelta   string
	routingResult  string
	routingErr     error
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

func (f *fakeLifecycle) Close(_ context.Context, childID string) error {
	f.closedID = childID
	return f.closeErr
}

func (f *fakeLifecycle) SetBudget(_ context.Context, childID string, maxCost float64) error {
	f.budgetChildID = childID
	f.budgetMaxCost = maxCost
	return f.budgetErr
}

func (f *fakeLifecycle) SetRouting(_ context.Context, childID, delta string) (string, error) {
	f.routingChildID = childID
	f.routingDelta = delta
	return f.routingResult, f.routingErr
}

func TestSpawnPassesFieldsThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/work", Name: "scout", Model: "claude-opus-5", Kind: "fundi",
		Preset:        "reviewer",
		ParentChildId: "c_0", ExecutorSelector: "kind=native",
		Labels: map[string]string{"team": "a"},
	}))
	c.Require().NoError(err, "Spawn")
	c.Eq("c_new", resp.Msg.GetChildId(), "ChildId")
	if f.got.Cwd != "/work" || f.got.Name != "scout" || f.got.Kind != "fundi" {
		t.Errorf("params wrong: %+v", f.got)
	}
	c.Eq("reviewer", f.got.Preset, "Preset")
	if f.got.ParentChildID != "c_0" || f.got.ExecutorSelector != "kind=native" {
		t.Errorf("lineage/selector wrong: %+v", f.got)
	}
	c.Eq("a", f.got.Labels["team"], "labels wrong: %+v", f.got.Labels)
}

// TestSpawnUnsetBudgetsStayNil is the important one: unset must NOT become
// zero. An unset MaxCost means unlimited; a zero means "spend nothing", and
// collapsing them makes every unbudgeted agent refuse its first spawn.
func TestSpawnUnsetBudgetsStayNil(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
	assert.NewAborting(t).NoError(err, "Spawn")
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
	c := assert.NewCollecting(t)
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	zeroI := int32(0)
	zeroF := float64(0)
	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/work", MaxDepth: &zeroI, MaxCost: &zeroF, MaxChildren: &zeroI,
	}))
	c.Require().NoError(err, "Spawn")
	c.False(f.got.MaxDepth == nil || *f.got.MaxDepth != 0, "explicit MaxDepth=0 was lost")
	c.False(f.got.MaxCost == nil || *f.got.MaxCost != 0, "explicit MaxCost=0 was lost")
	c.False(f.got.MaxChildren == nil || *f.got.MaxChildren != 0, "explicit MaxChildren=0 was lost")
}

func TestSpawnRequiresCwd(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{})
	_, err := s.Spawn(context.Background(), connect.NewRequest(&rafikiv1.SpawnRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestSpawnWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
	assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

func TestSpawnErrorBecomesInternal(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{spawnErr: errors.New("budget exceeded")})
	_, err := s.Spawn(context.Background(),
		connect.NewRequest(&rafikiv1.SpawnRequest{Cwd: "/work"}))
	assert.NewCollecting(t).Eq(connect.CodeInternal, connect.CodeOf(err), "code")
}

func TestKillPassesTimeouts(t *testing.T) {
	c := assert.NewCollecting(t)
	code := 0
	f := &fakeLifecycle{killOut: connectapi.KillOutcome{ExitCode: &code, DurationMs: 12}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{
		ChildId: "c_1", ShutdownTimeoutMs: 5000, KillTimeoutMs: 2000,
	}))
	c.Require().NoError(err, "Kill")
	if f.killedID != "c_1" || f.gotShutdow != 5000 || f.gotKill != 2000 {
		t.Errorf("kill args wrong: id=%q shutdown=%d kill=%d", f.killedID, f.gotShutdow, f.gotKill)
	}
	if resp.Msg.GetExitCode() != 0 || resp.Msg.GetDurationMs() != 12 {
		t.Errorf("outcome wrong: exit=%d duration=%d", resp.Msg.GetExitCode(), resp.Msg.GetDurationMs())
	}
	c.Eq("c_1", resp.Msg.GetChildId(), "ChildId")
}

func TestKillRequiresChildID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(&fakeLifecycle{})
	_, err := s.Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestKillWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Kill(context.Background(),
		connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_1"}))
	assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}
