package fundi

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// lspClientAdapter adapts *lsp.Manager to tools.LSPClient so the tools
// package does not import the lsp package.
type lspClientAdapter struct {
	mgr *lsp.Manager
	// tracker is shared with every other file tool so lsp_rename's writes
	// take the same per-path locks as edit/write.
	tracker *tools.FileTracker
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
		out[i] = tools.LSPLocation{URI: uriToPath(l.URI), Line: l.Range.Start.Line, Col: l.Range.Start.Character}
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
		out[i] = tools.LSPLocation{URI: uriToPath(l.URI), Line: l.Range.Start.Line, Col: l.Range.Start.Character}
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
	return flattenSymbols(syms, path), nil
}

// flattenSymbols walks the document symbol tree. Name and Kind are carried
// through: dropping them rendered every symbol as a bare ":3:6", which told
// the model a location for something it had no way to identify. path is
// threaded in because documentSymbol results are relative to the requested
// file and carry no URI of their own.
func flattenSymbols(syms []lsp.DocumentSymbol, path string) []tools.LSPLocation {
	var out []tools.LSPLocation
	for _, s := range syms {
		out = append(out, tools.LSPLocation{
			URI:  path,
			Line: s.SelectionRange.Start.Line,
			Col:  s.SelectionRange.Start.Character,
			Name: s.Name,
			Kind: lsp.SymbolKindName(s.Kind),
		})
		out = append(out, flattenSymbols(s.Children, path)...)
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
		out[i] = tools.LSPLocation{
			URI:  uriToPath(s.Location.URI),
			Line: s.Location.Range.Start.Line,
			Col:  s.Location.Range.Start.Character,
			Name: s.Name,
			Kind: lsp.SymbolKindName(s.Kind),
		}
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
	// FileEdits normalizes both WorkspaceEdit shapes. Reading edit.Changes
	// directly is why rename silently did nothing: gopls answers with
	// documentChanges, so Changes was always nil and this loop never ran.
	byFile, err := edit.FileEdits()
	if err != nil {
		return nil, err
	}
	return applyWorkspaceEdit(byFile, client.PositionEncoding(), a.tracker)
}

// applyWorkspaceEdit writes a multi-file edit atomically.
//
// Every file is read, edited in memory and validated BEFORE anything is
// written. The previous version wrote each file as it went while iterating a
// Go map — whose order is random — so a failure on the 4th of 7 files left a
// nondeterministic subset renamed and the rest not: a workspace that no
// longer compiles and cannot be reproduced to debug.
func applyWorkspaceEdit(byFile map[string][]lsp.TextEdit, encoding string, tracker *tools.FileTracker) ([]string, error) {
	paths := make([]string, 0, len(byFile))
	for p := range byFile {
		paths = append(paths, p)
	}
	// Deterministic order for locking (and so errors are reproducible).
	sort.Strings(paths)

	// Hold every per-path lock for the whole read-modify-write. Tool batches
	// run concurrently (up to 6), so a batch containing lsp_rename and edit
	// on the same file is otherwise a lost update between two overlapping
	// read-modify-write cycles.
	if tracker != nil {
		for _, p := range paths {
			unlock := tracker.Lock(p)
			defer unlock()
		}
	}

	staged := make(map[string][]byte, len(paths))
	for _, p := range paths {
		content, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("lsp: read %s: %w", p, err)
		}
		next, err := applyTextEdits(content, byFile[p], encoding)
		if err != nil {
			return nil, fmt.Errorf("lsp: apply edit to %s: %w", p, err)
		}
		staged[p] = next
	}

	var modified []string
	for _, p := range paths {
		if err := os.WriteFile(p, staged[p], 0o644); err != nil {
			return modified, fmt.Errorf("lsp: write %s: %w", p, err)
		}
		if tracker != nil {
			if info, statErr := os.Stat(p); statErr == nil {
				tracker.RecordRead(p, info.ModTime())
			}
		}
		modified = append(modified, p)
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

// applyTextEdits returns content with edits applied, leaving the input
// untouched.
//
// All ranges in a WorkspaceEdit refer to the ORIGINAL document, so they must
// be applied last-first — otherwise each edit shifts the offsets of the ones
// after it. Iterating the slice backwards is only correct if the server
// happened to send it in ascending order, which the spec does not require:
// with two edits arriving as (col 12-15) then (col 4-7), the old code
// produced "aaa QUUX barQUUXo zzz" from "aaa foo bar foo zzz". Sorting a
// copy by descending start position makes it independent of the server.
func applyTextEdits(content []byte, edits []lsp.TextEdit, encoding string) ([]byte, error) {
	ordered := make([]lsp.TextEdit, len(edits))
	copy(ordered, edits)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i].Range.Start, ordered[j].Range.Start
		if a.Line != b.Line {
			return a.Line > b.Line
		}
		return a.Character > b.Character
	})

	text := string(content)
	// PositionToOffset clamps to end-of-text, which is right for a column
	// running past end-of-line but wrong for a line that does not exist: an
	// edit at line 99 of a 3-line file would silently become an append
	// rather than an error. Validate lines up front so a stale range — the
	// server's view lagging the file on disk is exactly the case that
	// produces one — refuses the whole edit instead of corrupting the file.
	lineCount := strings.Count(text, "\n") + 1
	for _, e := range ordered {
		if e.Range.Start.Line >= lineCount || e.Range.End.Line >= lineCount {
			return nil, fmt.Errorf(
				"edit range %v is outside the file (%d lines); the server's view is stale",
				e.Range, lineCount)
		}
	}

	out := content
	prevStart := len(out) + 1
	for _, edit := range ordered {
		// Offsets are computed against the ORIGINAL text, which is what the
		// server's ranges refer to; applying last-first keeps them valid.
		start := lsp.PositionToOffset(text, edit.Range.Start, encoding)
		end := lsp.PositionToOffset(text, edit.Range.End, encoding)
		if start < 0 || end < start || end > len(out) {
			return nil, fmt.Errorf("invalid range %v", edit.Range)
		}
		if end > prevStart {
			return nil, fmt.Errorf("overlapping edits at %v", edit.Range)
		}
		prevStart = start
		next := make([]byte, 0, len(out)-(end-start)+len(edit.NewText))
		next = append(next, out[:start]...)
		next = append(next, edit.NewText...)
		next = append(next, out[end:]...)
		out = next
	}
	return out, nil
}

func uriToPath(uri string) string {
	p, err := lsp.URIToPath(uri)
	if err != nil {
		return ""
	}
	return p
}
