package tools

import (
	"context"
	"fmt"
)

const lspDefinitionDescription = `Go to the definition of a symbol at a position in a file. 
Input: "path" (file), "line" and "col" (1-based). Returns the file and position of the definition.`

func init() { DefaultBlueprint.Register(&LSPDefinitionBlueprint{}) }

type LSPDefinitionBlueprint struct{}

func (LSPDefinitionBlueprint) Name() string        { return "lsp_definition" }
func (LSPDefinitionBlueprint) Description() string { return lspDefinitionDescription }
func (LSPDefinitionBlueprint) InputSchema() Schema {
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

func (LSPDefinitionBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPDefinitionBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.LSP == nil {
		return nil, nil
	}
	return &lspDefinitionTool{LSPDefinitionBlueprint: LSPDefinitionBlueprint{}, lsp: opts.LSP, cwd: opts.Cwd}, nil
}

type lspDefinitionTool struct {
	LSPDefinitionBlueprint
	lsp LSPClient
	cwd string
}

type lspPosInput struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

func (lt *lspDefinitionTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspPosInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_definition: invalid input: %w", err)
	}
	absPath, err := resolveToolPath(in.Path, "", lt.cwd)
	if err != nil {
		return ToolResult{}, err
	}
	// Convert 1-based to 0-based.
	locs, err := lt.lsp.Definition(ctx, absPath, in.Line-1, in.Col-1)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_definition: %w", err)
	}
	return NewTextResult(formatLSPLocations(locs, "Definition")), nil
}
