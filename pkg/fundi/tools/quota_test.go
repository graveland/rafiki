package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

type fakeQuotaReader struct {
	status QuotaStatus
	ok     bool
	err    error
}

func (f *fakeQuotaReader) RateLimitStatus(context.Context) (QuotaStatus, bool, error) {
	return f.status, f.ok, f.err
}

func TestQuotaStatusMaterializeDeclinesWithoutAgentsOrQuota(t *testing.T) {
	bp := QuotaStatusBlueprint{}

	if tool, err := bp.Materialize(ToolOpts{}); err != nil || tool != nil {
		t.Errorf("Materialize with neither Agents nor Quota = (%v, %v), want (nil, nil)", tool, err)
	}
	if tool, err := bp.Materialize(ToolOpts{Agents: &fakeSpawner{}}); err != nil || tool != nil {
		t.Errorf("Materialize with Agents but no Quota = (%v, %v), want (nil, nil)", tool, err)
	}
	if tool, err := bp.Materialize(ToolOpts{Quota: &fakeQuotaReader{}}); err != nil || tool != nil {
		t.Errorf("Materialize with Quota but no Agents = (%v, %v), want (nil, nil)", tool, err)
	}
}

func TestQuotaStatusMaterializesWhenBothPresent(t *testing.T) {
	c := assert.NewAborting(t)
	tool, err := QuotaStatusBlueprint{}.Materialize(ToolOpts{Agents: &fakeSpawner{}, Quota: &fakeQuotaReader{}})
	c.NoError(err, "Materialize")
	c.NotNil(tool, "Materialize returned a nil tool with both Agents and Quota set")
}

func TestQuotaStatusExecuteNoDataCaptured(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := QuotaStatusBlueprint{}.Materialize(ToolOpts{Agents: &fakeSpawner{}, Quota: &fakeQuotaReader{ok: false}})
	c.Require().False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	res, err := tool.Execute(context.Background(), nil)
	c.Require().NoError(err, "Execute")
	c.StrContains(res.Text, "no data captured", "Execute text")
}

func TestQuotaStatusExecuteRendersSnapshot(t *testing.T) {
	c := assert.NewCollecting(t)
	util := 0.42
	reset := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	reader := &fakeQuotaReader{
		ok: true,
		status: QuotaStatus{
			OrganizationID: "org_123",
			FiveH:          QuotaWindow{Utilization: &util, ResetAt: &reset, Status: "allowed"},
			SevenD:         QuotaWindow{Status: "allowed_warning"},
			OverallStatus:  "allowed_warning",
			UpdatedAt:      time.Now(),
		},
	}
	tool, err := QuotaStatusBlueprint{}.Materialize(ToolOpts{Agents: &fakeSpawner{}, Quota: reader})
	c.Require().False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	res, err := tool.Execute(context.Background(), nil)
	c.Require().NoError(err, "Execute")
	out := res.Text
	for _, want := range []string{"42%", "allowed_warning", "5h", "7d"} {
		c.StrContains(out, want, "Execute text missing")
	}
}

func TestQuotaStatusExecutePropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	tool, err := QuotaStatusBlueprint{}.Materialize(ToolOpts{Agents: &fakeSpawner{}, Quota: &fakeQuotaReader{err: wantErr}})
	assert.NewAborting(t).False(err != nil || tool == nil, "Materialize: tool=%v err=%v", tool, err)
	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Fatal("Execute did not propagate the reader's error")
	}
}
