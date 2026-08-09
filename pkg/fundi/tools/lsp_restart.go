package tools

import (
	"context"
	"fmt"
)

const lspRestartDescription = `Restart the language server for a file's language. 
Use when the language server appears to be returning stale or incorrect results.
Input: "path" — any file in the workspace; the server for that file's language is restarted.`

func init() { DefaultBlueprint.Register(&LSPRestartBlueprint{}) }

type LSPRestartBlueprint struct{}

func (LSPRestartBlueprint) Name() string        { return "lsp_restart" }
func (LSPRestartBlueprint) Description() string { return lspRestartDescription }
func (LSPRestartBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "Any file path in the workspace whose language server should be restarted"},
		},
		Required: []string{"path"},
	}
}

func (LSPRestartBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPRestartBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.LSP == nil {
		return nil, nil
	}
	return &lspRestartTool{LSPRestartBlueprint: LSPRestartBlueprint{}, lsp: opts.LSP, cwd: opts.Cwd}, nil
}

type lspRestartTool struct {
	LSPRestartBlueprint
	lsp LSPClient
	cwd string
}

type lspRestartInput struct {
	Path string `json:"path"`
}

func (lt *lspRestartTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspRestartInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_restart: invalid input: %w", err)
	}

	absPath, err := resolveToolPath(in.Path, "", lt.cwd)
	if err != nil {
		return ToolResult{}, err
	}

	if err := lt.lsp.Restart(ctx, absPath); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_restart: %w", err)
	}

	return NewTextResult(fmt.Sprintf("Language server restarted for %s", absPath)), nil
}
