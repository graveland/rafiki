package tools

import (
	"context"
	"fmt"
	"strings"
)

func init() { DefaultBlueprint.Register(&ExecutorAnnotateBlueprint{}) }

const executorAnnotate_description = "Record a fact about the machine you are " +
	"running on, so a later agent does not have to redo expensive setup. A " +
	"clone-plus-build costs twenty minutes to establish and one string to record.\n\n" +
	"You can only annotate the machine you are on — there is no way to annotate " +
	"another one. Annotations are HINTS: one means \"this was true when it was " +
	"written\", never \"this is true now\". Verify before you rely on it. They " +
	"disappear when the machine does, which is the intended garbage collection."

// ExecutorAnnotator is the interface the executor_annotate tool materialises
// against. It annotates the executor the caller is currently running on — the
// executor id is not an argument, because an LLM can be talked into changing
// an argument.
type ExecutorAnnotator interface {
	Annotate(ctx context.Context, set map[string]string, remove []string) error
}

const maxAnnotationValueLen = 4096

// ExecutorAnnotateBlueprint is the static metadata for the executor_annotate tool.
type ExecutorAnnotateBlueprint struct{}

func (ExecutorAnnotateBlueprint) Name() string        { return "executor_annotate" }
func (ExecutorAnnotateBlueprint) Description() string { return executorAnnotate_description }
func (ExecutorAnnotateBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "set", Type: "object", Description: "key=value pairs to set. Keys must not start with rafiki/. Values are capped at 4096 bytes."},
			{Name: "remove", Type: "array", Description: "keys to remove"},
		},
	}
}
func (ExecutorAnnotateBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (b ExecutorAnnotateBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	ann := opts.ExecutorAnnotator
	if ann == nil {
		return nil, nil // decline
	}
	return &executorAnnotateTool{ann: ann, desc: b.Description()}, nil
}

type executorAnnotateTool struct {
	ann  ExecutorAnnotator
	desc string
}

func (t *executorAnnotateTool) Name() string        { return "executor_annotate" }
func (t *executorAnnotateTool) Description() string { return t.desc }
func (t *executorAnnotateTool) InputSchema() Schema { return ExecutorAnnotateBlueprint{}.InputSchema() }

func (t *executorAnnotateTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	var raw struct {
		Set    map[string]string `json:"set"`
		Remove []string          `json:"remove"`
	}
	if err := in.Unmarshal(&raw); err != nil {
		return ToolResult{}, fmt.Errorf("executor_annotate: %w", err)
	}
	for k, v := range raw.Set {
		if len(v) > maxAnnotationValueLen {
			return ToolResult{}, fmt.Errorf("value for key %q exceeds %d bytes (annotations are hints, not data stores)", k, maxAnnotationValueLen)
		}
		if strings.HasPrefix(k, "rafiki/") {
			return ToolResult{}, fmt.Errorf("the rafiki/ prefix is reserved for trust labels set by the daemon; annotations are hints, not access grants")
		}
	}
	if err := t.ann.Annotate(ctx, raw.Set, raw.Remove); err != nil {
		return ToolResult{}, fmt.Errorf("annotate: %w", err)
	}
	result := "annotation applied"
	if len(raw.Set) > 0 {
		keys := make([]string, 0, len(raw.Set))
		for k := range raw.Set {
			keys = append(keys, k)
		}
		result = fmt.Sprintf("set %d annotation(s): %s", len(raw.Set), strings.Join(keys, ", "))
	}
	if len(raw.Remove) > 0 {
		result += fmt.Sprintf("; removed %d: %s", len(raw.Remove), strings.Join(raw.Remove, ", "))
	}
	return ToolResult{Text: result}, nil
}
