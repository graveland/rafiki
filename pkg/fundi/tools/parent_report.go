package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

func init() {
	DefaultBlueprint.Register(&AgentReportBlueprint{})
	DefaultBlueprint.Register(&AgentResultBlueprint{})
}

const (
	agentReportDescription = "Send a short note to the agent that spawned you, without ending your turn. It reaches your parent at its next turn boundary, batched with other agents' notes. Use it ONLY for something that changes what your parent should do next: a plan or interface problem that affects other work, a shared file you must touch that others may also be editing, an assumption you are proceeding on that they may want to overrule (kind \"question\"), or a blocking finding they can act on before you finish. Do not use it for routine progress or for your final summary — put the final summary in agent_result. kind is one of message (the default), warning, question, progress; a progress note replaces your previous unread progress note, the other kinds all arrive."

	agentResultDescription = "Record your result: the short verdict your parent sees in the notification it gets when your turn ends, so it can act without opening your report. Give the status line and the key points — for example DONE, DONE_WITH_CONCERNS, NEEDS_CONTEXT or BLOCKED, then one line per finding or concern. The full detail still belongs in your report file. Calling it again replaces the previous result, and the result is cleared when your next turn starts, so set it again before any turn whose outcome your parent should see."
)

// ParentReporter is the daemon-side capability behind agent_report and
// agent_result. Both act on the caller's OWN position in the agent tree,
// resolved from the binding this value was built for — never from an
// argument. The daemon provides the implementation.
type ParentReporter interface {
	// Report sends a note to the caller's parent (a top-level caller's
	// note goes to its own event log). kind is one of ReportKinds.
	Report(ctx context.Context, kind, message string) error
	// SetResult records the caller's result for its next settle.
	SetResult(ctx context.Context, result string) error
}

// ReportKinds are the kinds agent_report accepts. "progress" replaces the
// caller's previous unread progress note; every other kind accumulates.
var ReportKinds = []string{"message", "warning", "question", "progress"}

// --- agent_report ---

type AgentReportBlueprint struct{}

func (AgentReportBlueprint) Name() string        { return "agent_report" }
func (AgentReportBlueprint) Description() string { return agentReportDescription }
func (AgentReportBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "kind", Type: "string",
				Description: "One of " + strings.Join(ReportKinds, ", ") + ". Defaults to message."},
			{Name: "message", Type: "string",
				Description: "The note to send to your parent."},
		},
		Required: []string{"message"},
	}
}

func (AgentReportBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentReportBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Parent == nil {
		return nil, nil
	}
	return &agentReportTool{reporter: opts.Parent}, nil
}

type agentReportTool struct {
	AgentReportBlueprint
	reporter ParentReporter
}

func (t *agentReportTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_report: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	// The empty kind means "message" ONLY here, at the tool: the daemon
	// never fills in a kind, so an omitted one would otherwise reach it as
	// an empty string it has no default for.
	if params.Kind == "" {
		params.Kind = "message"
	}
	if !slices.Contains(ReportKinds, params.Kind) {
		return ToolResult{}, fmt.Errorf("agent_report: kind must be one of %s", strings.Join(ReportKinds, ", "))
	}
	if params.Message == "" {
		return ToolResult{}, errors.New("agent_report: message is required")
	}
	if err := t.reporter.Report(ctx, params.Kind, params.Message); err != nil {
		return ToolResult{}, fmt.Errorf("agent_report: %w", err)
	}
	return NewTextResult("reported\n"), nil
}

// --- agent_result ---

type AgentResultBlueprint struct{}

func (AgentResultBlueprint) Name() string        { return "agent_result" }
func (AgentResultBlueprint) Description() string { return agentResultDescription }
func (AgentResultBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "result", Type: "string",
				Description: "The short verdict your parent sees when your turn ends: a status line, then one line per finding or concern."},
		},
		Required: []string{"result"},
	}
}

func (AgentResultBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentResultBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Parent == nil {
		return nil, nil
	}
	return &agentResultTool{reporter: opts.Parent}, nil
}

type agentResultTool struct {
	AgentResultBlueprint
	reporter ParentReporter
}

func (t *agentResultTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Result string `json:"result"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_result: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Result == "" {
		return ToolResult{}, errors.New("agent_result: result is required")
	}
	if err := t.reporter.SetResult(ctx, params.Result); err != nil {
		return ToolResult{}, fmt.Errorf("agent_result: %w", err)
	}
	return NewTextResult("result recorded; it rides your next settle notification\n"), nil
}
