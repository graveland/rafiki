// Package lspadapter makes an *lsp.Manager satisfy tools.LSPClient.
//
// It is its own package rather than living in either neighbour because it
// needs both, and making pkg/fundi/lsp import pkg/fundi/tools (or the reverse)
// would couple the tool vocabulary to the LSP transport in one direction or
// the other. It must also stay free of pgx: pkg/executor builds one of these
// and is required to link no database driver at all.
package lspadapter

import (
	"context"
	"fmt"
	"log/slog"
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

// New returns a tools.LSPClient backed by mgr.
//
// tracker is shared with every other file tool so lsp_rename's writes take the
// same per-path locks as edit and write. Passing a tracker that other tools do
// not share reintroduces the interleaved-write race those locks exist for.
func New(mgr *lsp.Manager, tracker *tools.FileTracker) tools.LSPClient {
	return &lspClientAdapter{mgr: mgr, tracker: tracker}
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
	modified, err := applyWorkspaceEdit(byFile, client.PositionEncoding(), a.tracker)
	if err != nil {
		return modified, err
	}
	// edit and write both call notifyFileChanged (registry.go) after a
	// successful write; rename wrote files and updated the FileTracker but
	// never told the server, so a subsequent navigation query answered from
	// the server's stale parse. A rename is exactly the case registry.go's
	// FileChangeNotifier doc warns is silent: old name and new name are
	// almost always the same length, so the line count is unchanged and the
	// server returns a confidently wrong location with no error at all.
	//
	// Best-effort like notifyFileChanged: the files are already renamed on
	// disk, so a sync failure here must not turn a successful rename into a
	// tool error — it is logged, not swallowed silently, so a server that
	// has stopped accepting notifications is still diagnosable.
	for _, p := range modified {
		if nerr := a.mgr.NotifyChange(ctx, p); nerr != nil {
			slog.Debug("lsp: rename change notification failed", "path", p, "err", nerr)
		}
	}
	return modified, nil
}

// applyWorkspaceEdit writes a multi-file edit, narrowing the window in
// which a write failure can leave a partial rename on disk.
//
// Every file is read, edited in memory and validated BEFORE anything is
// written — that part was always true. But "write every file in a second
// loop" is not the same guarantee as the comment here used to imply: a
// failure on write 4 of 7 (disk full, a path that turned read-only, a
// directory that vanished) still left files 1-3 renamed and 4-7 not, a
// workspace that no longer compiles. Each staged file is now written to a
// ".rename-tmp" sibling of its target first; only once every temp write has
// succeeded do we os.Rename them into place one by one. A rename within the
// same directory is atomic and — barring the filesystem itself failing —
// cannot fail the way a fresh write can, so an ordinary failure (the kind
// this function actually needs to guard against) is now caught in the temp
// phase, before any real file has been touched, and every temp file it
// created is removed.
//
// This is still not a cross-file transaction: an OS crash or kill -9
// between the third and fifth os.Rename can still leave a partial result,
// because there is no way to make N independent renames atomic as a group
// without filesystem support this code doesn't have. What changed is that
// the common failure mode — a write that fails for an ordinary reason
// partway through — no longer corrupts the workspace.
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
	modes := make(map[string]os.FileMode, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("lsp: stat %s: %w", p, err)
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("lsp: read %s: %w", p, err)
		}
		next, err := applyTextEdits(content, byFile[p], encoding)
		if err != nil {
			return nil, fmt.Errorf("lsp: apply edit to %s: %w", p, err)
		}
		staged[p] = next
		modes[p] = info.Mode()
	}

	// Phase 1: write every staged file to a temp sibling. tmpFor tracks only
	// the temp files actually created, so cleanup on an early failure never
	// tries to remove one that was never written.
	tmpFor := make(map[string]string, len(paths))
	cleanupTmp := func() {
		for _, tmp := range tmpFor {
			_ = os.Remove(tmp)
		}
	}
	for _, p := range paths {
		tmp := p + ".rename-tmp"
		if err := os.WriteFile(tmp, staged[p], modes[p]); err != nil {
			cleanupTmp()
			return nil, fmt.Errorf("lsp: write %s: %w", tmp, err)
		}
		tmpFor[p] = tmp
	}

	// Phase 2: rename every temp file into place. Each individual rename is
	// atomic, but a failure partway still leaves the earlier ones applied —
	// see the function comment on what guarantee this does and does not
	// provide.
	var modified []string
	for _, p := range paths {
		if err := os.Rename(tmpFor[p], p); err != nil {
			// tmpFor[p] is left in the map on purpose: the rename failed, so
			// that temp file is presumed to still exist and cleanupTmp must
			// remove it too, not just the ones not yet attempted.
			cleanupTmp()
			return modified, fmt.Errorf("lsp: rename into %s: %w", p, err)
		}
		delete(tmpFor, p)
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
	// Manager.Restart owns the restart-budget bookkeeping: a deliberate,
	// tool-initiated restart must not consume the same crash budget a
	// server that keeps dying on its own is measured against. See
	// forgiveRestart in manager.go.
	return a.mgr.Restart(ctx, path)
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
