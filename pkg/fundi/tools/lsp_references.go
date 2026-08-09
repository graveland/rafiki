package tools

import (
	"context"
	"fmt"
)

const lspReferencesDescription = `Find all references to a symbol at a position in a file.
Input: "path" (file), "line" and "col" (1-based). Returns all reference locations.`

func init() { DefaultBlueprint.Register(&LSPReferencesBlueprint{}) }

type LSPReferencesBlueprint struct{}

func (LSPReferencesBlueprint) Name() string        { return "lsp_references" }
func (LSPReferencesBlueprint) Description() string { return lspReferencesDescription }
func (LSPReferencesBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "File path (absolute or relative to cwd)"},
			{Name: "line", Type: "integer", Description: "1-based line number of the symbol"},
			{Name: "col", Type: "integer", Description: "1-based column number of the symbol"},
		},
		Required: []string{"path", "line", "col"},
	}
}

func (LSPReferencesBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPReferencesBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.LSP == nil {
		return nil, nil
	}
	return &lspReferencesTool{LSPReferencesBlueprint: LSPReferencesBlueprint{}, lsp: opts.LSP, cwd: opts.Cwd}, nil
}

type lspReferencesTool struct {
	LSPReferencesBlueprint
	lsp LSPClient
	cwd string
}

func (lt *lspReferencesTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspPosInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_references: invalid input: %w", err)
	}
	absPath, err := resolveToolPath(in.Path, "", lt.cwd)
	if err != nil {
		return ToolResult{}, err
	}
	locs, err := lt.lsp.References(ctx, absPath, in.Line-1, in.Col-1)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_references: %w", err)
	}
	// Budget: truncate very large results.
	total := len(locs)
	if len(locs) > 100 {
		locs = locs[:100]
	}
	result := formatLSPLocations(locs, "References")
	if total > 100 {
		result += fmt.Sprintf("\n(truncated at 100 of %d total)", total)
	}
	return NewTextResult(result), nil
}
