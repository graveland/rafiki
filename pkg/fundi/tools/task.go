package tools

import (
	"context"
	"fmt"

	"go.graveland.dev/rafiki/pkg/tasks"
)

func init() {
	DefaultBlueprint.Register(&TaskAddBlueprint{})
	DefaultBlueprint.Register(&TaskUpdateBlueprint{})
	DefaultBlueprint.Register(&TaskDropBlueprint{})
	DefaultBlueprint.Register(&TaskListBlueprint{})
}

const (
	taskAddDescription = "Add tasks to your task list (task tracking / todo list). " +
		"Use this to decompose work into trackable units at the start of a multi-step " +
		"job, and to add tasks you discover along the way. Each item takes `content` " +
		"(imperative: \"Run the tests\") and `active_form` (present continuous: " +
		"\"Running the tests\"). Pass `parent` (a handle like \"2\") to nest new tasks " +
		"under an existing one. Returns your full list with each task's handle. " +
		"One task per meaningful unit of work — not one per tool call."

	taskUpdateDescription = "Change the status of tasks in your list (mark complete, " +
		"mark in progress). Pass a list of {handle, status} changes; this touches only " +
		"the tasks you name and leaves every other task exactly as it was. Valid " +
		"statuses: pending, in_progress, blocked, completed, failed. Use `blocked` and " +
		"say what you are blocked on rather than leaving work in_progress indefinitely. " +
		"Use `failed` when you tried and could not — not `task_drop`."

	taskDropDescription = "Abandon a task you are not going to do. Requires a " +
		"`reason`, because \"decomposed wrong\", \"turned out unnecessary\" and " +
		"\"superseded by a better approach\" are legitimate and quietly giving up is " +
		"not — if you tried and could not, use task_update with status=failed instead. " +
		"Dropping a task also drops its subtasks. Refuses if the task or any subtask " +
		"is assigned to another agent."

	taskListDescription = "Show your task list. With no arguments, shows your own " +
		"tasks and any task assigned to you. Filter by `status`, by `metadata` key=value " +
		"pairs, or by `assignee` to inspect an agent you spawned. Dropped tasks are " +
		"hidden unless you pass include_dropped."
)

// --- task_add ---

type TaskAddBlueprint struct{}

func (TaskAddBlueprint) Name() string        { return "task_add" }
func (TaskAddBlueprint) Description() string { return taskAddDescription }
func (TaskAddBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{
				Name: "items", Type: "array",
				Description: "Tasks to add. One per meaningful unit of work.",
				Items: &Schema{
					Type: "object",
					Properties: []SchemaProperty{
						{Name: "content", Type: "string", Description: "Imperative form: \"Run the tests\""},
						{Name: "active_form", Type: "string", Description: "Present continuous: \"Running the tests\""},
						{Name: "metadata", Type: "object", Description: "Optional key=value annotations (project, priority, etc.)"},
					},
					Required: []string{"content", "active_form"},
				},
			},
			{
				Name: "parent", Type: "string",
				Description: "Handle of the parent task (e.g. \"2\") to nest new tasks under. Omit for top-level tasks.",
			},
		},
		Required: []string{"items"},
	}
}

func (TaskAddBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (TaskAddBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &taskAddTool{store: opts.Tasks}, nil
}

type taskAddTool struct {
	TaskAddBlueprint
	store tasks.Store
}

func (t *taskAddTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	type addItem struct {
		Content    string            `json:"content"`
		ActiveForm string            `json:"active_form"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}
	var params struct {
		Items  []addItem `json:"items"`
		Parent string    `json:"parent,omitempty"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("task_add: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	convID := ConversationIDFromContext(ctx)

	newItems := make([]tasks.NewTask, len(params.Items))
	for i, it := range params.Items {
		newItems[i] = tasks.NewTask{
			Content:    it.Content,
			ActiveForm: it.ActiveForm,
			Metadata:   it.Metadata,
		}
	}
	_, err := t.store.Add(ctx, convID, params.Parent, newItems)
	if err != nil {
		return ToolResult{}, fmt.Errorf("task_add: %w", err)
	}
	all, err := t.store.List(ctx, tasks.ListFilter{ConversationID: convID, IncludeDropped: false})
	if err != nil {
		return ToolResult{}, fmt.Errorf("task_add: list: %w", err)
	}
	return NewTextResult(tasks.Render(all, false)), nil
}

// --- task_update ---

type TaskUpdateBlueprint struct{}

func (TaskUpdateBlueprint) Name() string        { return "task_update" }
func (TaskUpdateBlueprint) Description() string { return taskUpdateDescription }
func (TaskUpdateBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{
				Name: "changes", Type: "array",
				Description: "Status changes to apply. Each names a handle and the new status.",
				Items: &Schema{
					Type: "object",
					Properties: []SchemaProperty{
						{Name: "handle", Type: "string", Description: "Task handle, e.g. \"2\" or \"2.1\""},
						{Name: "status", Type: "string", Description: "One of: pending, in_progress, blocked, completed, failed"},
					},
					Required: []string{"handle", "status"},
				},
			},
		},
		Required: []string{"changes"},
	}
}

func (TaskUpdateBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (TaskUpdateBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &taskUpdateTool{store: opts.Tasks}, nil
}

type taskUpdateTool struct {
	TaskUpdateBlueprint
	store tasks.Store
}

func (t *taskUpdateTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	type updateChange struct {
		Handle string `json:"handle"`
		Status string `json:"status"`
	}
	var params struct {
		Changes []updateChange `json:"changes"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("task_update: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	changes := make([]tasks.Change, len(params.Changes))
	for i, ch := range params.Changes {
		st := tasks.Status(ch.Status)
		if !st.UserSettable() {
			return ToolResult{}, fmt.Errorf("task_update: invalid status %q; valid: pending, in_progress, blocked, completed, failed", ch.Status)
		}
		changes[i] = tasks.Change{Handle: ch.Handle, Status: st}
	}

	convID := ConversationIDFromContext(ctx)
	_, err := t.store.Update(ctx, convID, changes)
	if err != nil {
		return ToolResult{}, fmt.Errorf("task_update: %w", err)
	}
	all, err := t.store.List(ctx, tasks.ListFilter{ConversationID: convID, IncludeDropped: false})
	if err != nil {
		return ToolResult{}, fmt.Errorf("task_update: list: %w", err)
	}
	return NewTextResult(tasks.Render(all, false)), nil
}

// --- task_drop ---

type TaskDropBlueprint struct{}

func (TaskDropBlueprint) Name() string        { return "task_drop" }
func (TaskDropBlueprint) Description() string { return taskDropDescription }
func (TaskDropBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "handle", Type: "string", Description: "Handle of the task to abandon, e.g. \"2\" or \"2.1\""},
			{Name: "reason", Type: "string", Description: "Why this task is being dropped. Required."},
		},
		Required: []string{"handle", "reason"},
	}
}

func (TaskDropBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (TaskDropBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &taskDropTool{store: opts.Tasks}, nil
}

type taskDropTool struct {
	TaskDropBlueprint
	store tasks.Store
}

func (t *taskDropTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Handle string `json:"handle"`
		Reason string `json:"reason"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("task_drop: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Reason == "" {
		return ToolResult{}, tasks.ErrDropReason
	}
	convID := ConversationIDFromContext(ctx)
	_, err := t.store.Drop(ctx, convID, params.Handle, params.Reason)
	if err != nil {
		return ToolResult{}, fmt.Errorf("task_drop: %w", err)
	}
	all, err := t.store.List(ctx, tasks.ListFilter{ConversationID: convID, IncludeDropped: true})
	if err != nil {
		return ToolResult{}, fmt.Errorf("task_drop: list: %w", err)
	}
	return NewTextResult(tasks.Render(all, true)), nil
}

// --- task_list ---

type TaskListBlueprint struct{}

func (TaskListBlueprint) Name() string        { return "task_list" }
func (TaskListBlueprint) Description() string { return taskListDescription }
func (TaskListBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "status", Type: "string", Description: "Filter by status (pending, in_progress, blocked, completed, failed, orphaned)"},
			{Name: "assignee", Type: "string", Description: "Filter by assignee child id"},
			{Name: "metadata", Type: "object", Description: "Filter by metadata key=value pairs"},
			{Name: "include_dropped", Type: "boolean", Description: "Include dropped tasks (hidden by default)"},
		},
	}
}

func (TaskListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (TaskListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &taskListTool{store: opts.Tasks, childID: opts.ChildID}, nil
}

type taskListTool struct {
	TaskListBlueprint
	store   tasks.Store
	childID string
}

func (t *taskListTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Status         string            `json:"status,omitempty"`
		Assignee       string            `json:"assignee,omitempty"`
		Metadata       map[string]string `json:"metadata,omitempty"`
		IncludeDropped bool              `json:"include_dropped,omitempty"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("task_list: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	convID := ConversationIDFromContext(ctx)
	all, err := t.store.List(ctx, tasks.ListFilter{
		ConversationID: convID,
		Status:         tasks.Status(params.Status),
		Assignee:       params.Assignee,
		Metadata:       params.Metadata,
		IncludeDropped: params.IncludeDropped,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("task_list: %w", err)
	}
	return NewTextResult(tasks.Render(all, params.IncludeDropped)), nil
}
