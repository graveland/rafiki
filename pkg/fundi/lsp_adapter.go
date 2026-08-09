package fundi

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// lspClientAdapter adapts *lsp.Manager to tools.LSPClient so the tools
// package does not import the lsp package.
type lspClientAdapter struct {
	mgr *lsp.Manager
}

func (a *lspClientAdapter) Diagnostics(ctx context.Context, path string) ([]tools.LSPDiagnostic, error) {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	diags, err := client.Diagnostics(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]tools.LSPDiagnostic, len(diags))
	for i, d := range diags {
		out[i] = tools.LSPDiagnostic{
			Path:     path,
			Line:     d.Range.Start.Line,
			Column:   d.Range.Start.Character,
			Severity: d.Severity.String(),
			Message:  d.Message,
		}
	}
	return out, nil
}

func (a *lspClientAdapter) DidOpen(ctx context.Context, path, content string) error {
	if content == "" {
		var err error
		contentBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("lsp: read %s: %w", path, err)
		}
		content = string(contentBytes)
	}
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return err
	}
	if client == nil {
		return nil
	}
	return client.DidOpen(ctx, path, content)
}

func (a *lspClientAdapter) DidChange(ctx context.Context, path, content string) error {
	if content == "" {
		var err error
		contentBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("lsp: read %s: %w", path, err)
		}
		content = string(contentBytes)
	}
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return err
	}
	if client == nil {
		return nil
	}
	return client.DidChange(ctx, path, content)
}

func (a *lspClientAdapter) WaitForDiagnostics(ctx context.Context, path string, timeoutSec int) error {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return err
	}
	if client == nil {
		return nil
	}
	startVer := client.DiagnosticsVersion()
	return client.WaitForInitialDiagnostics(ctx, startVer, time.Duration(timeoutSec)*time.Second)
}

func (a *lspClientAdapter) Definition(ctx context.Context, path string, line, col int) ([]tools.LSPLocation, error) {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	locs, err := client.Definition(ctx, path, line, col)
	if err != nil {
		return nil, err
	}
	out := make([]tools.LSPLocation, len(locs))
	for i, l := range locs {
		out[i] = tools.LSPLocation{URI: l.URI, Line: l.Range.Start.Line, Col: l.Range.Start.Character}
	}
	return out, nil
}

func (a *lspClientAdapter) References(ctx context.Context, path string, line, col int) ([]tools.LSPLocation, error) {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	locs, err := client.References(ctx, path, line, col)
	if err != nil {
		return nil, err
	}
	out := make([]tools.LSPLocation, len(locs))
	for i, l := range locs {
		out[i] = tools.LSPLocation{URI: l.URI, Line: l.Range.Start.Line, Col: l.Range.Start.Character}
	}
	return out, nil
}

func (a *lspClientAdapter) DocumentSymbols(ctx context.Context, path string) ([]tools.LSPLocation, error) {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	syms, err := client.DocumentSymbols(ctx, path)
	if err != nil {
		return nil, err
	}
	return flattenSymbols(syms), nil
}

func flattenSymbols(syms []lsp.DocumentSymbol) []tools.LSPLocation {
	var out []tools.LSPLocation
	for _, s := range syms {
		out = append(out, tools.LSPLocation{
			URI:  "",
			Line: s.SelectionRange.Start.Line,
			Col:  s.SelectionRange.Start.Character,
		})
		out = append(out, flattenSymbols(s.Children)...)
	}
	return out
}

func (a *lspClientAdapter) WorkspaceSymbols(ctx context.Context, query string) ([]tools.LSPLocation, error) {
	client, err := a.mgr.FirstClient(ctx)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	syms, err := client.WorkspaceSymbols(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make([]tools.LSPLocation, len(syms))
	for i, s := range syms {
		out[i] = tools.LSPLocation{URI: s.Location.URI, Line: s.Location.Range.Start.Line, Col: s.Location.Range.Start.Character}
	}
	return out, nil
}

func (a *lspClientAdapter) PrepareCallHierarchy(ctx context.Context, path string, line, col int) ([]tools.LSPCallHierarchyItem, error) {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	items, err := client.PrepareCallHierarchy(ctx, path, line, col)
	if err != nil {
		return nil, err
	}
	out := make([]tools.LSPCallHierarchyItem, len(items))
	for i, it := range items {
		out[i] = tools.LSPCallHierarchyItem{Name: it.Name, URI: it.URI, Line: it.Range.Start.Line, Col: it.Range.Start.Character}
	}
	return out, nil
}

func (a *lspClientAdapter) IncomingCalls(ctx context.Context, item tools.LSPCallHierarchyItem) ([]tools.LSPCallHierarchyItem, error) {
	client, err := a.mgr.For(ctx, item.URI)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	lspItem := lsp.CallHierarchyItem{
		Name: item.Name,
		URI:  item.URI,
		Range: lsp.Range{
			Start: lsp.Position{Line: item.Line, Character: item.Col},
			End:   lsp.Position{Line: item.Line, Character: item.Col},
		},
		SelectionRange: lsp.Range{
			Start: lsp.Position{Line: item.Line, Character: item.Col},
			End:   lsp.Position{Line: item.Line, Character: item.Col},
		},
	}
	calls, err := client.IncomingCalls(ctx, lspItem)
	if err != nil {
		return nil, err
	}
	out := make([]tools.LSPCallHierarchyItem, len(calls))
	for i, c := range calls {
		out[i] = tools.LSPCallHierarchyItem{Name: c.From.Name, URI: c.From.URI, Line: c.From.Range.Start.Line, Col: c.From.Range.Start.Character}
	}
	return out, nil
}

func (a *lspClientAdapter) OutgoingCalls(ctx context.Context, item tools.LSPCallHierarchyItem) ([]tools.LSPCallHierarchyItem, error) {
	client, err := a.mgr.For(ctx, item.URI)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, nil
	}
	lspItem := lsp.CallHierarchyItem{
		Name: item.Name,
		URI:  item.URI,
		Range: lsp.Range{
			Start: lsp.Position{Line: item.Line, Character: item.Col},
			End:   lsp.Position{Line: item.Line, Character: item.Col},
		},
		SelectionRange: lsp.Range{
			Start: lsp.Position{Line: item.Line, Character: item.Col},
			End:   lsp.Position{Line: item.Line, Character: item.Col},
		},
	}
	calls, err := client.OutgoingCalls(ctx, lspItem)
	if err != nil {
		return nil, err
	}
	out := make([]tools.LSPCallHierarchyItem, len(calls))
	for i, c := range calls {
		out[i] = tools.LSPCallHierarchyItem{Name: c.To.Name, URI: c.To.URI, Line: c.To.Range.Start.Line, Col: c.To.Range.Start.Character}
	}
	return out, nil
}

func (a *lspClientAdapter) Rename(ctx context.Context, path string, line, col int, newName string) ([]string, error) {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("lsp: no language server for %s", path)
	}
	edit, err := client.Rename(ctx, path, line, col, newName)
	if err != nil {
		return nil, err
	}
	// Apply the workspace edit to disk.
	var modified []string
	for uri, textEdits := range edit.Changes {
		filePath := uriToPath(uri)
		if err := applyTextEdits(filePath, textEdits); err != nil {
			return modified, fmt.Errorf("lsp: apply edit to %s: %w", filePath, err)
		}
		modified = append(modified, filePath)
	}
	return modified, nil
}

func (a *lspClientAdapter) Restart(ctx context.Context, path string) error {
	client, err := a.mgr.For(ctx, path)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("lsp: no language server for %s", path)
	}
	_ = client.Shutdown(ctx)
	// For will lazily restart on next call.
	return nil
}

func applyTextEdits(path string, edits []lsp.TextEdit) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Apply edits in reverse order so offsets remain valid.
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		start := positionToOffset(string(content), edit.Range.Start)
		end := positionToOffset(string(content), edit.Range.End)
		if start < 0 || end < 0 || start > end || end > len(content) {
			return fmt.Errorf("invalid range %v for %q", edit.Range, path)
		}
		content = append(content[:start], append([]byte(edit.NewText), content[end:]...)...)
	}
	return os.WriteFile(path, content, 0o644)
}

func positionToOffset(text string, pos lsp.Position) int {
	line := 0
	for i, ch := range text {
		if line == pos.Line {
			// Count characters from line start; approximate for now.
			offset := i + pos.Character
			if offset > len(text) {
				offset = len(text)
			}
			return offset
		}
		if ch == '\n' {
			line++
		}
	}
	return len(text)
}

func uriToPath(uri string) string {
	return strings.TrimPrefix(uri, "file://")
}

// workspacePath returns a plausible path to use for workspace-level requests.
