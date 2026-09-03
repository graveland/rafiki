package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
	tool, err := QuotaStatusBlueprint{}.Materialize(ToolOpts{Agents: &fakeSpawner{}, Quota: &fakeQuotaReader{}})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool == nil {
		t.Fatal("Materialize returned a nil tool with both Agents and Quota set")
	}
}

func TestQuotaStatusExecuteNoDataCaptured(t *testing.T) {
	tool, err := QuotaStatusBlueprint{}.Materialize(ToolOpts{Agents: &fakeSpawner{}, Quota: &fakeQuotaReader{ok: false}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	res, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "no data captured") {
		t.Errorf("Execute text = %q, want it to mention no data captured", res.Text)
	}
}

func TestQuotaStatusExecuteRendersSnapshot(t *testing.T) {
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
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	res, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Text
	for _, want := range []string{"42%", "allowed_warning", "5h", "7d"} {
		if !strings.Contains(out, want) {
			t.Errorf("Execute text missing %q; got:\n%s", want, out)
		}
	}
}

func TestQuotaStatusExecutePropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	tool, err := QuotaStatusBlueprint{}.Materialize(ToolOpts{Agents: &fakeSpawner{}, Quota: &fakeQuotaReader{err: wantErr}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Fatal("Execute did not propagate the reader's error")
	}
}
