package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/quotafmt"
)

func init() {
	DefaultBlueprint.Register(&QuotaStatusBlueprint{})
}

// QuotaWindow is one rate-limit window's snapshot (the "5h" or "7d" family).
// Utilization and ResetAt are pointers because a value Anthropic did not
// report must stay distinguishable from a real zero or a real timestamp.
type QuotaWindow struct {
	Utilization *float64
	ResetAt     *time.Time
	Status      string
}

// QuotaStatus is the caller's latest captured Anthropic subscription
// rate-limit snapshot.
type QuotaStatus struct {
	OrganizationID string
	FiveH          QuotaWindow
	SevenD         QuotaWindow
	OverallStatus  string
	UpdatedAt      time.Time
}

// QuotaReader answers the CALLER's own latest captured snapshot. Bound to one
// user at construction, the same reasoning as AgentSpawner being bound to one
// child: a caller-supplied id would let one agent read another user's usage.
type QuotaReader interface {
	RateLimitStatus(ctx context.Context) (QuotaStatus, bool, error)
}

const quotaStatusDescription = "Check your Anthropic subscription's current rate-limit " +
	"usage (5h and 7d rolling windows). Only reports data once a passthrough call has " +
	"actually billed your subscription -- if that has never happened this returns " +
	"'no data captured yet', which is normal, not an error. Use this before spawning a " +
	"subagent on an Anthropic model when you want to avoid running the subscription into " +
	"its limit: prefer an OpenRouter (or other non-Anthropic) model for new work once " +
	"utilization is high, rather than guessing."

// --- quota_status ---

type QuotaStatusBlueprint struct{}

func (QuotaStatusBlueprint) Name() string        { return "quota_status" }
func (QuotaStatusBlueprint) Description() string { return quotaStatusDescription }
func (QuotaStatusBlueprint) InputSchema() Schema {
	return Schema{Type: "object", Properties: []SchemaProperty{}}
}

func (QuotaStatusBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

// Materialize declines (returns nil, nil) when this agent has no spawn
// capability (opts.Agents == nil): the tool's whole purpose is informing a
// spawn/model choice, and a tool that can only ever be looked at costs a turn
// to learn nothing (the SkillBlueprint rule). It also declines when no quota
// source is configured at all (a DB-less daemon, or the standalone
// `rafikid fundi` process).
func (QuotaStatusBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil || opts.Quota == nil {
		return nil, nil
	}
	return &quotaStatusTool{quota: opts.Quota}, nil
}

type quotaStatusTool struct {
	QuotaStatusBlueprint
	quota QuotaReader
}

func (t *quotaStatusTool) Execute(ctx context.Context, _ ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	st, ok, err := t.quota.RateLimitStatus(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("quota_status: %w", err)
	}
	if !ok {
		return NewTextResult("no data captured yet -- this account has not made a passthrough call to Anthropic"), nil
	}
	return NewTextResult(renderQuotaStatus(st)), nil
}

func renderQuotaStatus(st QuotaStatus) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "updated %s ago\n", time.Since(st.UpdatedAt).Round(time.Second))
	renderQuotaWindow(&sb, "5h", st.FiveH)
	renderQuotaWindow(&sb, "7d", st.SevenD)
	fmt.Fprintf(&sb, "overall: %s\n", orDash(st.OverallStatus))
	return sb.String()
}

func renderQuotaWindow(sb *strings.Builder, label string, w QuotaWindow) {
	util := quotafmt.Utilization(w.Utilization)
	reset := "unknown"
	if w.ResetAt != nil {
		reset = w.ResetAt.Format(time.RFC3339)
	}
	fmt.Fprintf(sb, "%s: %s used, resets %s, status %s\n", label, util, reset, orDash(w.Status))
}
