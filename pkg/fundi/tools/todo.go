package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const (
	todoDescription = "Manage a structured task list for your current coding session. " +
		"Use this to track progress, organize complex tasks, and demonstrate " +
		"thoroughness to the user. Each call replaces the entire list — pass the " +
		"complete desired state. Include `content` (imperative form: \"Run the tests\"), " +
		"`status` (one of: pending, in_progress, completed), and `active_form` " +
		"(present continuous: \"Running the tests\"). More than one item may be " +
		"in_progress at once."

	validStatuses = "pending, in_progress, completed"
)

func init() { DefaultBlueprint.Register(&TodoBlueprint{}) }

// TodoBlueprint is the static Tool blueprint for the todo tool.
type TodoBlueprint struct{}

func (TodoBlueprint) Name() string        { return "todo" }
func (TodoBlueprint) Description() string { return todoDescription }
func (TodoBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{
				Name: "todos",
				Type: "array",
				Description: "The complete todo list. Each call replaces the entire list — " +
					"pass the full desired state, not just changes.",
				Items: &Schema{
					Type: "object",
					Properties: []SchemaProperty{
						{Name: "content", Type: "string", Description: "Imperative form: \"Run the tests\""},
						{Name: "status", Type: "string", Description: "One of: " + validStatuses},
						{Name: "active_form", Type: "string", Description: "Present continuous: \"Running the tests\""},
					},
					Required: []string{"content", "status", "active_form"},
				},
			},
		},
		Required: []string{"todos"},
	}
}

func (TodoBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

// Materialize returns a concrete todo tool with its own isolated state.
func (TodoBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &todoTool{TodoBlueprint: TodoBlueprint{}}, nil
}

type todoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form"`
}

type todoInput struct {
	Todos []todoItem `json:"todos"`
}

type todoTool struct {
	TodoBlueprint
	mu    sync.Mutex
	items []todoItem
}

var validStatusSet = map[string]bool{
	"pending":     true,
	"in_progress": true,
	"completed":   true,
}

func (tt *todoTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in todoInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("todo: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	for i, item := range in.Todos {
		if !validStatusSet[item.Status] {
			return ToolResult{}, fmt.Errorf(
				"todo: invalid status %q for item %d; valid statuses are: %s",
				item.Status, i, validStatuses)
		}
	}

	// Store, then render from the stored copy rather than from in.Todos.
	// Rendering the input directly left tt.items write-only — nothing in
	// the repo read it — so the per-agent isolation this tool exists to
	// provide was asserted by a test that could not fail: replacing the
	// field with a package-level global kept every isolation assertion
	// green, because each call just echoed its own input back. Reading
	// through the stored state is what makes the guarantee testable.
	tt.mu.Lock()
	tt.items = in.Todos
	items := tt.items
	tt.mu.Unlock()

	var sb strings.Builder
	counts := map[string]int{"pending": 0, "in_progress": 0, "completed": 0}
	for _, item := range items {
		counts[item.Status]++
	}

	fmt.Fprintf(&sb, "%d todo(s):\n", len(items))
	for i, item := range items {
		icon := iconForStatus(item.Status)
		fmt.Fprintf(&sb, "  %s %s", icon, item.Content)
		if item.ActiveForm != "" {
			fmt.Fprintf(&sb, " (%s)", item.ActiveForm)
		}
		if i < len(items)-1 {
			sb.WriteByte('\n')
		}
	}

	fmt.Fprintf(&sb, "\n[%d pending, %d in_progress, %d completed]",
		counts["pending"], counts["in_progress"], counts["completed"])

	return NewTextResult(sb.String()), nil
}

func iconForStatus(status string) string {
	switch status {
	case "pending":
		return "☐"
	case "in_progress":
		return "▣"
	case "completed":
		return "☑"
	default:
		return "?"
	}
}
