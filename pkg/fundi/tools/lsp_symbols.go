package tools

import (
	"context"
	"fmt"
)

const lspSymbolsDescription = `Search for symbols in a file or across the workspace.
Input: "path" (file, for document symbols) OR "query" (for workspace-wide symbol search). 
Provide at most one of path or query. Document symbols return the outline of a file; 
workspace symbols search all files by name.`

func init() { DefaultBlueprint.Register(&LSPSymbolsBlueprint{}) }

type LSPSymbolsBlueprint struct{}

func (LSPSymbolsBlueprint) Name() string        { return "lsp_symbols" }
func (LSPSymbolsBlueprint) Description() string { return lspSymbolsDescription }
func (LSPSymbolsBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "File path for document symbols (relative or absolute)"},
			{Name: "query", Type: "string", Description: "Symbol name query for workspace-wide search"},
		},
	}
}

func (LSPSymbolsBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPSymbolsBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.LSP == nil {
		return nil, nil
	}
	return &lspSymbolsTool{LSPSymbolsBlueprint: LSPSymbolsBlueprint{}, lsp: opts.LSP, cwd: opts.Cwd}, nil
}

type lspSymbolsTool struct {
	LSPSymbolsBlueprint
	lsp LSPClient
	cwd string
}

type lspSymInput struct {
	Path  string `json:"path"`
	Query string `json:"query"`
}

func (lt *lspSymbolsTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspSymInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_symbols: invalid input: %w", err)
	}

	if in.Query != "" {
		locs, err := lt.lsp.WorkspaceSymbols(ctx, in.Query)
		if err != nil {
			return ToolResult{}, fmt.Errorf("lsp_symbols: workspace: %w", err)
		}
		if len(locs) > 100 {
			locs = locs[:100]
		}
		return NewTextResult(formatLSPLocations(locs, fmt.Sprintf("Workspace symbols matching %q", in.Query))), nil
	}

	if in.Path != "" {
		absPath, err := resolveToolPath(in.Path, "", lt.cwd)
		if err != nil {
			return ToolResult{}, err
		}
		// Open the file so the server indexes it.
		_ = lt.lsp.DidOpen(ctx, absPath, "")
		_ = lt.lsp.WaitForDiagnostics(ctx, absPath, 5)
		locs, err := lt.lsp.DocumentSymbols(ctx, absPath)
		if err != nil {
			return ToolResult{}, fmt.Errorf("lsp_symbols: document: %w", err)
		}
		return NewTextResult(formatLSPLocations(locs, fmt.Sprintf("Symbols in %s", absPath))), nil
	}

	return ToolResult{}, fmt.Errorf("lsp_symbols: provide either path or query")
}
