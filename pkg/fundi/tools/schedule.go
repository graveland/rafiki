package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func init() {
	DefaultBlueprint.Register(&CronCreateBlueprint{})
	DefaultBlueprint.Register(&CronDeleteBlueprint{})
	DefaultBlueprint.Register(&CronListBlueprint{})
	DefaultBlueprint.Register(&ScheduleWakeupBlueprint{})
}

const (
	cronCreateDescription = `Schedule a prompt to be delivered to you at a future time, once or on a recurring schedule. Uses standard 5-field cron (minute hour day-of-month month day-of-week) evaluated in your owner's timezone, or the daemon's when your owner has none; the result names the zone. recurring defaults to true; pass false for a one-shot reminder with minute, hour, day-of-month and month pinned. A recurring schedule must not fire more often than every 5 minutes. Jobs persist with this conversation: they survive daemon restarts, are delivered into your message queue whether or not you are mid-turn, never expire, and end only through CronDelete or when this agent is closed. While this agent tree is over its budget, fires are held and delivered together once the budget is raised. Returns a job id for CronDelete.`

	cronDeleteDescription = "Cancel a job you created with CronCreate or ScheduleWakeup, by the id it returned."

	cronListDescription = "List your own pending scheduled jobs — CronCreate jobs and any pending ScheduleWakeup — with each one's next fire time and zone."

	scheduleWakeupDescription = "Wake yourself up later with a prompt. delaySeconds is clamped to 300–3600 (5 minutes to an hour). Each call replaces your pending wakeup; stop: true cancels it. The wakeup persists with this conversation and is delivered into your message queue. reason and noop are accepted for compatibility and not used."
)

// ScheduleInfo is one job as the tools render it.
type ScheduleInfo struct {
	ID        string
	Kind      string // "cron" or "wakeup"
	Cron      string
	TZ        string
	Prompt    string
	Recurring bool
	NextFire  time.Time
	HeldCount int
}

// ScheduleManager is the daemon-side capability behind the schedule tools,
// bound to the calling child at construction — never addressed by argument.
type ScheduleManager interface {
	CronCreate(ctx context.Context, cron, prompt string, recurring bool) (ScheduleInfo, error)
	CronDelete(ctx context.Context, id string) error
	CronList(ctx context.Context) ([]ScheduleInfo, error)
	// ScheduleWakeup replaces any pending wakeup; the daemon clamps delay.
	ScheduleWakeup(ctx context.Context, delay time.Duration, prompt string) (ScheduleInfo, error)
	// CancelWakeup cancels the pending wakeup; false when there was none.
	CancelWakeup(ctx context.Context) (bool, error)
}

// scheduleLoc resolves a job's zone for rendering, falling back to UTC when
// the zone the daemon named cannot be loaded — a rendering failure must never
// turn a scheduled job into an error.
func scheduleLoc(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// truncateSchedulePrompt cuts a prompt to 80 runes, marking the cut with an
// ellipsis. Counting runes, not bytes, keeps multibyte prompts intact.
func truncateSchedulePrompt(s string) string {
	r := []rune(s)
	if len(r) <= 80 {
		return s
	}
	return string(r[:80]) + "…"
}

// --- CronCreate ---

type CronCreateBlueprint struct{}

func (CronCreateBlueprint) Name() string        { return "CronCreate" }
func (CronCreateBlueprint) Description() string { return cronCreateDescription }
func (CronCreateBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "cron", Type: "string",
				Description: "Standard 5-field cron expression (minute hour day-of-month month day-of-week)."},
			{Name: "prompt", Type: "string",
				Description: "The prompt to deliver to you when the job fires."},
			{Name: "recurring", Type: "boolean",
				Description: "true (the default) for a recurring schedule; false for a one-shot reminder with minute, hour, day-of-month and month pinned."},
			{Name: "durable", Type: "boolean",
				Description: "Accepted for compatibility and ignored."},
		},
		Required: []string{"cron", "prompt"},
	}
}

func (CronCreateBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (CronCreateBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Schedules == nil {
		return nil, nil
	}
	return &cronCreateTool{schedules: opts.Schedules}, nil
}

type cronCreateTool struct {
	CronCreateBlueprint
	schedules ScheduleManager
}

func (t *cronCreateTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Cron      string `json:"cron"`
		Prompt    string `json:"prompt"`
		Recurring *bool  `json:"recurring"`
		Durable   bool   `json:"durable"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("CronCreate: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Cron == "" || params.Prompt == "" {
		return ToolResult{}, errors.New("CronCreate: cron and prompt are required")
	}
	// The default lives ONLY here at the tool — a nil *bool is "the model
	// said nothing", which means recurring, and only the tool can tell that
	// apart from an explicit false.
	recurring := true
	if params.Recurring != nil {
		recurring = *params.Recurring
	}
	info, err := t.schedules.CronCreate(ctx, params.Cron, params.Prompt, recurring)
	if err != nil {
		return ToolResult{}, fmt.Errorf("CronCreate: %w", err)
	}
	loc := scheduleLoc(info.TZ)
	kind := "one-shot"
	if recurring {
		kind = "recurring"
	}
	return NewTextResult(fmt.Sprintf("scheduled %s: %q in %s, next fire %s, %s",
		info.ID, info.Cron, info.TZ, info.NextFire.In(loc).Format(time.RFC3339), kind)), nil
}

// --- CronDelete ---

type CronDeleteBlueprint struct{}

func (CronDeleteBlueprint) Name() string        { return "CronDelete" }
func (CronDeleteBlueprint) Description() string { return cronDeleteDescription }
func (CronDeleteBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "id", Type: "string",
				Description: "The job id returned by CronCreate or ScheduleWakeup."},
		},
		Required: []string{"id"},
	}
}

func (CronDeleteBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (CronDeleteBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Schedules == nil {
		return nil, nil
	}
	return &cronDeleteTool{schedules: opts.Schedules}, nil
}

type cronDeleteTool struct {
	CronDeleteBlueprint
	schedules ScheduleManager
}

func (t *cronDeleteTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("CronDelete: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.ID == "" {
		return ToolResult{}, errors.New("CronDelete: id is required")
	}
	if err := t.schedules.CronDelete(ctx, params.ID); err != nil {
		return ToolResult{}, fmt.Errorf("CronDelete: %w", err)
	}
	return NewTextResult(fmt.Sprintf("deleted %s", params.ID)), nil
}

// --- CronList ---

type CronListBlueprint struct{}

func (CronListBlueprint) Name() string        { return "CronList" }
func (CronListBlueprint) Description() string { return cronListDescription }
func (CronListBlueprint) InputSchema() Schema {
	return Schema{Type: "object"}
}

func (CronListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (CronListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Schedules == nil {
		return nil, nil
	}
	return &cronListTool{schedules: opts.Schedules}, nil
}

type cronListTool struct {
	CronListBlueprint
	schedules ScheduleManager
}

func (t *cronListTool) Execute(ctx context.Context, _ ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	jobs, err := t.schedules.CronList(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("CronList: %w", err)
	}
	if len(jobs) == 0 {
		return NewTextResult("no scheduled jobs"), nil
	}
	lines := make([]string, 0, len(jobs))
	for _, info := range jobs {
		job := info.Cron
		if info.Kind == "wakeup" {
			job = "wakeup"
		}
		kind := "one-shot"
		if info.Recurring {
			kind = "recurring"
		}
		loc := scheduleLoc(info.TZ)
		line := fmt.Sprintf("%s  %s  %s  next %s  %s",
			info.ID, job, info.TZ, info.NextFire.In(loc).Format(time.RFC3339), kind)
		if info.HeldCount > 0 {
			line += fmt.Sprintf("  held %d", info.HeldCount)
		}
		line += "  — " + truncateSchedulePrompt(info.Prompt)
		lines = append(lines, line)
	}
	return NewTextResult(strings.Join(lines, "\n")), nil
}

// --- ScheduleWakeup ---

type ScheduleWakeupBlueprint struct{}

func (ScheduleWakeupBlueprint) Name() string        { return "ScheduleWakeup" }
func (ScheduleWakeupBlueprint) Description() string { return scheduleWakeupDescription }
func (ScheduleWakeupBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "delaySeconds", Type: "number",
				Description: "How long until the wakeup fires, in seconds. Clamped to 300–3600."},
			{Name: "prompt", Type: "string",
				Description: "The prompt to deliver to you when you wake."},
			{Name: "reason", Type: "string",
				Description: "Accepted for compatibility and not used."},
			{Name: "noop", Type: "boolean",
				Description: "Accepted for compatibility and not used."},
			{Name: "stop", Type: "boolean",
				Description: "true cancels your pending wakeup instead of scheduling one."},
		},
	}
}

func (ScheduleWakeupBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (ScheduleWakeupBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Schedules == nil {
		return nil, nil
	}
	return &scheduleWakeupTool{schedules: opts.Schedules}, nil
}

type scheduleWakeupTool struct {
	ScheduleWakeupBlueprint
	schedules ScheduleManager
}

func (t *scheduleWakeupTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		DelaySeconds float64 `json:"delaySeconds"`
		Prompt       string  `json:"prompt"`
		Reason       string  `json:"reason"`
		Noop         bool    `json:"noop"`
		Stop         bool    `json:"stop"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("ScheduleWakeup: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Stop {
		cancelled, err := t.schedules.CancelWakeup(ctx)
		if err != nil {
			return ToolResult{}, fmt.Errorf("ScheduleWakeup: %w", err)
		}
		if cancelled {
			return NewTextResult("wakeup cancelled"), nil
		}
		return NewTextResult("no pending wakeup"), nil
	}
	if params.DelaySeconds <= 0 {
		return ToolResult{}, errors.New("ScheduleWakeup: delaySeconds is required unless stop is true")
	}
	if params.Prompt == "" {
		return ToolResult{}, errors.New("ScheduleWakeup: prompt is required unless stop is true")
	}
	delay := time.Duration(params.DelaySeconds * float64(time.Second))
	info, err := t.schedules.ScheduleWakeup(ctx, delay, params.Prompt)
	if err != nil {
		return ToolResult{}, fmt.Errorf("ScheduleWakeup: %w", err)
	}
	loc := scheduleLoc(info.TZ)
	return NewTextResult(fmt.Sprintf("wakeup %s scheduled for %s",
		info.ID, info.NextFire.In(loc).Format(time.RFC3339))), nil
}
