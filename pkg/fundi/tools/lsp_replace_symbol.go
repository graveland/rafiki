package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const lspReplaceSymbolDescription = `Replace, insert around, or delete an entire named symbol in a file, using the
language server to find the symbol's exact range.
Input: "path" (file), "symbol" (the symbol's name), "action" ("replace" default,
"add_before", "add_after", or "delete") and "replacement" (the new text; required
unless action is "delete").
Because the range comes from the language server, no old_string is needed and
whitespace can never mismatch — prefer this over edit when rewriting a whole
function, method or type. The change is applied to the file on disk and the
FileTracker is refreshed for it afterward.`

func init() { DefaultBlueprint.Register(&LSPReplaceSymbolBlueprint{}) }

type LSPReplaceSymbolBlueprint struct{}

func (LSPReplaceSymbolBlueprint) Name() string        { return "lsp_replace_symbol" }
func (LSPReplaceSymbolBlueprint) Description() string { return lspReplaceSymbolDescription }
func (LSPReplaceSymbolBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "File path (absolute or relative to cwd)"},
			{Name: "symbol", Type: "string", Description: "Name of the symbol to target (function, method, type, ...)"},
			{Name: "action", Type: "string", Description: "replace (default), add_before, add_after, or delete"},
			{Name: "replacement", Type: "string", Description: "Replacement text; required unless action is delete"},
		},
		Required: []string{"path", "symbol"},
	}
}

func (LSPReplaceSymbolBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LSPReplaceSymbolBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if lspUnavailable(opts) {
		return nil, nil
	}
	return &lspReplaceSymbolTool{
		LSPReplaceSymbolBlueprint: LSPReplaceSymbolBlueprint{},
		lsp:                       opts.LSP,
		tr:                        opts.FileTracker,
		cwd:                       opts.Cwd,
		changed:                   opts.FileChanged,
	}, nil
}

type lspReplaceSymbolTool struct {
	LSPReplaceSymbolBlueprint
	lsp     LSPClient
	tr      *FileTracker
	cwd     string
	changed FileChangeNotifier
}

type lspReplaceSymbolInput struct {
	Path        string `json:"path"`
	Symbol      string `json:"symbol"`
	Action      string `json:"action"`
	Replacement string `json:"replacement"`
}

func (lt *lspReplaceSymbolTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lspReplaceSymbolInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: invalid input: %w", err)
	}
	if in.Symbol == "" {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: symbol is required")
	}

	action := in.Action
	if action == "" {
		action = "replace"
	}
	switch action {
	case "replace", "add_before", "add_after", "delete":
	default:
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: invalid action %q: must be replace, add_before, add_after, or delete", action)
	}
	if action != "delete" && in.Replacement == "" {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: replacement is required for action %q", action)
	}

	absPath, err := resolveToolPath(in.Path, "", lt.cwd)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: %w", err)
	}

	syms, err := lt.lsp.DocumentSymbols(ctx, absPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: document symbols: %w", err)
	}
	target, ok := findSymbolByName(syms, in.Symbol)
	if !ok {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: symbol %q not found in %s", in.Symbol, absPath)
	}

	raw, err := os.ReadFile(absPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: %w", err)
	}
	rawStr := string(raw)
	hasBOM := strings.HasPrefix(rawStr, "\uFEFF")
	lfContent, origLE := prepContent(rawStr)

	lines := strings.Split(lfContent, "\n")
	startLine := target.Line
	endLine := target.EndLine
	if endLine < startLine {
		// A client that carried no range end: treat the symbol as one line
		// rather than producing a zero- or negative-width span.
		endLine = startLine
	}
	if startLine < 0 || startLine >= len(lines) || endLine >= len(lines) {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: symbol %q range (%d-%d) exceeds file length (%d lines)", in.Symbol, startLine+1, endLine+1, len(lines))
	}

	var newLines []string
	switch action {
	case "replace":
		newLines = append(newLines, lines[:startLine]...)
		newLines = append(newLines, strings.Split(normalizeToLF(in.Replacement), "\n")...)
		newLines = append(newLines, lines[endLine+1:]...)
	case "add_before":
		newLines = append(newLines, lines[:startLine]...)
		newLines = append(newLines, strings.Split(normalizeToLF(in.Replacement), "\n")...)
		newLines = append(newLines, lines[startLine:]...)
	case "add_after":
		newLines = append(newLines, lines[:endLine+1]...)
		newLines = append(newLines, strings.Split(normalizeToLF(in.Replacement), "\n")...)
		newLines = append(newLines, lines[endLine+1:]...)
	case "delete":
		newLines = append(newLines, lines[:startLine]...)
		newLines = append(newLines, lines[endLine+1:]...)
	}

	final := restoreLineEndings(strings.Join(newLines, "\n"), origLE)
	if hasBOM {
		final = "\uFEFF" + final
	}
	if err := writeFinal(absPath, rawStr, final); err != nil {
		return ToolResult{}, fmt.Errorf("lsp_replace_symbol: %w", err)
	}

	// Force-refresh the FileTracker, mirroring lsp_rename: a language-server
	// range is authoritative in a way a blind text edit is not, and requiring a
	// prior read would make the tool no easier than edit.
	lt.tr.RecordRead(absPath, fileMtime(absPath))
	notifyFileChanged(ctx, lt.changed, absPath)

	var summary string
	switch action {
	case "replace":
		summary = fmt.Sprintf("Replaced symbol %q in %s (lines %d-%d)", in.Symbol, absPath, startLine+1, endLine+1)
	case "add_before":
		summary = fmt.Sprintf("Inserted before symbol %q in %s (before line %d)", in.Symbol, absPath, startLine+1)
	case "add_after":
		summary = fmt.Sprintf("Inserted after symbol %q in %s (after line %d)", in.Symbol, absPath, endLine+1)
	case "delete":
		summary = fmt.Sprintf("Deleted symbol %q from %s (lines %d-%d)", in.Symbol, absPath, startLine+1, endLine+1)
	}
	return NewTextResult(summary), nil
}

// findSymbolByName returns the first document symbol whose name matches, in
// the depth-first order DocumentSymbols produced (outermost/earliest first).
func findSymbolByName(syms []LSPLocation, name string) (LSPLocation, bool) {
	for _, s := range syms {
		if s.Name == name {
			return s, true
		}
	}
	return LSPLocation{}, false
}
