package tools

import (
	"context"
	"fmt"
	"strings"
)

const lspDiagnosticsDescription = `Get compiler and linter errors from the language server for a file. 
Input: "path" (optional — omit or use "." to include all open files).
Output: path:line:col: severity: message, one per diagnostic. 
After editing a file, call this to check for errors without running a build.
The language server must be configured (lsp.json) for your language.`

func init() { DefaultBlueprint.Register(&LSPDiagnosticsBlueprint{}) }

type LSPDiagnosticsBlueprint struct{}

func (LSPDiagnosticsBlueprint) Name() string        { return "lsp_diagnostics" }
func (LSPDiagnosticsBlueprint) Description() string { return lspDiagnosticsDescription }
func (LSPDiagnosticsBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "File path to check (absolute or relative to cwd). Omit to check all currently-open files."},
		},
	}
}

func (LSPDiagnosticsBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPDiagnosticsBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.LSP == nil {
		return nil, nil // decline: no language server configured
	}
	return &lspDiagnosticsTool{LSPDiagnosticsBlueprint: LSPDiagnosticsBlueprint{}, lsp: opts.LSP, cwd: opts.Cwd}, nil
}

type lspDiagnosticsTool struct {
	LSPDiagnosticsBlueprint
	lsp LSPClient
	cwd string
}

type lspDiagInput struct {
	Path string `json:"path"`
}

func (lt *lspDiagnosticsTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspDiagInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_diagnostics: invalid input: %w", err)
	}

	absPath, err := resolveToolPath(in.Path, "", lt.cwd)
	if err != nil {
		return ToolResult{}, err
	}

	// Ensure the file is open in the language server, waiting briefly
	// for diagnostics to arrive. This is the bounded-wait guard against
	// returning "no errors" when the server simply hasn't finished yet.
	if err := lt.lsp.DidOpen(ctx, absPath, ""); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_diagnostics: open %s: %w", absPath, err)
	}
	// Wait up to 5s for diagnostics to arrive.
	_ = lt.lsp.WaitForDiagnostics(ctx, absPath, 5)

	diags, err := lt.lsp.Diagnostics(ctx, absPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_diagnostics: %w", err)
	}

	if len(diags) == 0 {
		return NewTextResult(fmt.Sprintf("%s: no diagnostics", absPath)), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d diagnostic(s) for %s:\n", len(diags), absPath)
	for _, d := range diags {
		fmt.Fprintf(&sb, "%s:%d:%d: %s: %s\n", d.Path, d.Line+1, d.Column+1, d.Severity, d.Message)
	}
	// Note: ClipBudget is applied by the agentloop via TurnOutputBudget.
	// The raw text is returned and the framework clips it if needed.
	// For very large diagnostic sets, a future improvement could add
	// explicit truncation here.
	return NewTextResult(strings.TrimRight(sb.String(), "\n")), nil
}
