package tools

import (
	"context"
	"fmt"
)

const lspCallHierarchyDescription = `Explore the call graph: find callers (incoming) or callees (outgoing) of a function.
Input: "path" (file), "line" and "col" (1-based), and "direction" ("incoming" or "outgoing").
Resolves the call hierarchy at the given position and returns one level of the graph.`

func init() { DefaultBlueprint.Register(&LSPCallHierarchyBlueprint{}) }

type LSPCallHierarchyBlueprint struct{}

func (LSPCallHierarchyBlueprint) Name() string        { return "lsp_call_hierarchy" }
func (LSPCallHierarchyBlueprint) Description() string { return lspCallHierarchyDescription }
func (LSPCallHierarchyBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "File path (absolute or relative to cwd)"},
			{Name: "line", Type: "integer", Description: "1-based line number of the function"},
			{Name: "col", Type: "integer", Description: "1-based column number of the function name"},
			{Name: "direction", Type: "string", Description: "\"incoming\" for callers, \"outgoing\" for callees"},
		},
		Required: []string{"path", "line", "col", "direction"},
	}
}

func (LSPCallHierarchyBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPCallHierarchyBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if lspUnavailable(opts) {
		return nil, nil
	}
	return &lspCallHierarchyTool{LSPCallHierarchyBlueprint: LSPCallHierarchyBlueprint{}, lsp: opts.LSP, cwd: opts.Cwd}, nil
}

type lspCallHierarchyTool struct {
	LSPCallHierarchyBlueprint
	lsp LSPClient
	cwd string
}

type lspCHInput struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	Direction string `json:"direction"`
}

func (lt *lspCallHierarchyTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspCHInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_call_hierarchy: invalid input: %w", err)
	}

	absPath, err := resolveToolPath(in.Path, "", lt.cwd)
	if err != nil {
		return ToolResult{}, err
	}

	items, err := lt.lsp.PrepareCallHierarchy(ctx, absPath, in.Line-1, in.Col-1)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_call_hierarchy: prepare: %w", err)
	}
	if len(items) == 0 {
		return NewTextResult("Call hierarchy: no item found at position"), nil
	}

	switch in.Direction {
	case "incoming":
		calls, err := lt.lsp.IncomingCalls(ctx, items[0])
		if err != nil {
			return ToolResult{}, fmt.Errorf("lsp_call_hierarchy: incoming: %w", err)
		}
		label := fmt.Sprintf("Incoming calls to %s", items[0].Name)
		return NewTextResult(formatLSPCallHierarchy(calls, label)), nil
	case "outgoing":
		calls, err := lt.lsp.OutgoingCalls(ctx, items[0])
		if err != nil {
			return ToolResult{}, fmt.Errorf("lsp_call_hierarchy: outgoing: %w", err)
		}
		label := fmt.Sprintf("Outgoing calls from %s", items[0].Name)
		return NewTextResult(formatLSPCallHierarchy(calls, label)), nil
	default:
		return ToolResult{}, fmt.Errorf("lsp_call_hierarchy: direction must be \"incoming\" or \"outgoing\", got %q", in.Direction)
	}
}
