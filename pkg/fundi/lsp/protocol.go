// Package lsp provides a minimal Language Server Protocol client and manager
// for fundi's code intelligence tools.
//
// Types are a deliberately narrow subset of the LSP 3.17 specification — just
// enough to support initialize, document sync, diagnostics, and navigation.
// Full protocol definitions live in sourcegraph/go-lsp; we vendor only what we
// use to avoid pulling in the entire specification surface.
package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf16"
)

// ---- Common LSP types ----

// Position is a zero-based line and character offset.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range in a specific document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// DiagnosticSeverity mirrors the LSP DiagnosticSeverity enum.
type DiagnosticSeverity int

const (
	SeverityError       DiagnosticSeverity = 1
	SeverityWarning     DiagnosticSeverity = 2
	SeverityInformation DiagnosticSeverity = 3
	SeverityHint        DiagnosticSeverity = 4
)

func (s DiagnosticSeverity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "info"
	case SeverityHint:
		return "hint"
	default:
		return fmt.Sprintf("severity(%d)", s)
	}
}

// Diagnostic is a compiler/linter diagnostic.
type Diagnostic struct {
	Range    Range              `json:"range"`
	Severity DiagnosticSeverity `json:"severity,omitempty"`
	Message  string             `json:"message"`
	Source   string             `json:"source,omitempty"`
}

// TextDocumentIdentifier identifies a document by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier is a TextDocumentIdentifier with a version.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// TextDocumentItem is the full document content sent on didOpen.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// TextDocumentContentChangeEvent is a content change within a document.
// We use full-text sync (TextDocumentSyncKind.Full), so only the "text" variant.
type TextDocumentContentChangeEvent struct {
	Text string `json:"text"`
}

// ---- Request/Notification params ----

// InitializeParams is the initialize request sent by the client.
type InitializeParams struct {
	ProcessID int `json:"processId"`

	// RootURI is the workspace root URI (file:// scheme).
	RootURI string `json:"rootUri,omitempty"`

	// Capabilities declared by the client.
	Capabilities ClientCapabilities `json:"capabilities"`

	// WorkspaceFolders, when non-nil, is the set of workspace folders. LSP
	// requires at least one when the client supports multi-root workspaces.
	WorkspaceFolders []WorkspaceFolder `json:"workspaceFolders,omitempty"`
}

// WorkspaceFolder represents a workspace folder root in an LSP session.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// ClientCapabilities declares what the client supports.
type ClientCapabilities struct {
	TextDocument *TextDocumentClientCapabilities `json:"textDocument,omitempty"`
	Workspace    *WorkspaceClientCapabilities    `json:"workspace,omitempty"`
	General      *GeneralClientCapabilities      `json:"general,omitempty"`
}

// GeneralClientCapabilities declares encoding-level capabilities.
type GeneralClientCapabilities struct {
	// PositionEncodings lists the encodings this client can handle, best
	// first. Declaring utf-8 matters: LSP's default is utf-16, so a
	// Position.Character is a count of UTF-16 code units, not bytes and not
	// runes. Servers that support utf-8 will pick it and let us index by
	// byte directly; the rest answer utf-16 and we convert.
	PositionEncodings []string `json:"positionEncodings,omitempty"`
}

// Position encodings, per LSP 3.17.
const (
	PositionEncodingUTF8  = "utf-8"
	PositionEncodingUTF16 = "utf-16"
)

// TextDocumentClientCapabilities declares textDocument capabilities.
type TextDocumentClientCapabilities struct {
	Synchronization    *SynchronizationCapabilities    `json:"synchronization,omitempty"`
	PublishDiagnostics *PublishDiagnosticsCapabilities `json:"publishDiagnostics,omitempty"`
	Definition         *DefinitionCapabilities         `json:"definition,omitempty"`
	References         *ReferencesCapabilities         `json:"references,omitempty"`
	DocumentSymbol     *DocumentSymbolCapabilities     `json:"documentSymbol,omitempty"`
	Rename             *RenameCapabilities             `json:"rename,omitempty"`
	CallHierarchy      *CallHierarchyCapabilities      `json:"callHierarchy,omitempty"`
}

// SynchronizationCapabilities declares the client's sync capabilities.
type SynchronizationCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
	WillSave            bool `json:"willSave,omitempty"`
	WillSaveWaitUntil   bool `json:"willSaveWaitUntil,omitempty"`
	DidSave             bool `json:"didSave,omitempty"`
}

// PublishDiagnosticsCapabilities declares diagnostic-related capabilities.
type PublishDiagnosticsCapabilities struct {
	RelatedInformation bool `json:"relatedInformation,omitempty"`
	DataSupport        bool `json:"dataSupport,omitempty"`
}

// DefinitionCapabilities declares definition capabilities.
type DefinitionCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// ReferencesCapabilities declares reference capabilities.
type ReferencesCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// DocumentSymbolCapabilities declares document symbol capabilities.
type DocumentSymbolCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
	Hierarchical        bool `json:"hierarchicalDocumentSymbolSupport,omitempty"`
}

// RenameCapabilities declares rename capabilities.
type RenameCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// CallHierarchyCapabilities declares call hierarchy capabilities.
type CallHierarchyCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// WorkspaceClientCapabilities declares workspace capabilities.
type WorkspaceClientCapabilities struct {
	ApplyEdit bool `json:"applyEdit,omitempty"`
}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
}

// ServerCapabilities declares what the server supports.
type ServerCapabilities struct {
	// PositionEncoding is the encoding the server chose. Per spec an absent
	// or empty value means utf-16 — see EffectivePositionEncoding, and note
	// that the zero value here is therefore NOT utf-8.
	PositionEncoding       string                   `json:"positionEncoding,omitempty"`
	TextDocumentSync       *TextDocumentSyncOptions `json:"textDocumentSync,omitempty"`
	DefinitionProvider     bool                     `json:"definitionProvider,omitempty"`
	ReferencesProvider     bool                     `json:"referencesProvider,omitempty"`
	DocumentSymbolProvider bool                     `json:"documentSymbolProvider,omitempty"`
	RenameProvider         bool                     `json:"renameProvider,omitempty"`
	CallHierarchyProvider  bool                     `json:"callHierarchyProvider,omitempty"`
}

// TextDocumentSyncOptions describes the server's document sync model.
type TextDocumentSyncOptions struct {
	OpenClose bool                 `json:"openClose"`
	Change    TextDocumentSyncKind `json:"change"`
	Save      json.RawMessage      `json:"save,omitempty"`
}

// TextDocumentSyncKind is the sync kind enum.
type TextDocumentSyncKind int

const (
	SyncKindNone        TextDocumentSyncKind = 0
	SyncKindFull        TextDocumentSyncKind = 1
	SyncKindIncremental TextDocumentSyncKind = 2
)

// DidOpenTextDocumentParams is sent when a document is opened.
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// DidChangeTextDocumentParams is sent when a document changes.
type DidChangeTextDocumentParams struct {
	TextDocument   VersionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []TextDocumentContentChangeEvent `json:"contentChanges"`
}

// DidCloseTextDocumentParams is sent when a document is closed.
type DidCloseTextDocumentParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// PublishDiagnosticsParams is the notification from server to client.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// ---- Navigation types (for sub-phase C) ----

// DefinitionParams is the request params for textDocument/definition.
type DefinitionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// ReferenceParams is the request params for textDocument/references.
type ReferenceParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      ReferenceContext       `json:"context"`
}

// ReferenceContext controls whether the declaration is included.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// DocumentSymbolParams is the request params for textDocument/documentSymbol.
type DocumentSymbolParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// WorkspaceSymbolParams is the request params for workspace/symbol.
type WorkspaceSymbolParams struct {
	Query string `json:"query"`
}

// DocumentSymbol is a symbol within a document (hierarchical).
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// SymbolInformation is a flat symbol result for workspace/symbol.
type SymbolInformation struct {
	Name     string   `json:"name"`
	Kind     int      `json:"kind"`
	Location Location `json:"location"`
}

// CallHierarchyItem is an item in the call hierarchy.
type CallHierarchyItem struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	URI            string `json:"uri"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
}

// CallHierarchyIncomingCall is an incoming call edge.
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCall is an outgoing call edge.
type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyPrepareParams is the request params for textDocument/prepareCallHierarchy.
type CallHierarchyPrepareParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// CallHierarchyIncomingCallsParams is the request params for callHierarchy/incomingCalls.
type CallHierarchyIncomingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

// CallHierarchyOutgoingCallsParams is the request params for callHierarchy/outgoingCalls.
type CallHierarchyOutgoingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

// ---- Rename types (for sub-phase D) ----

// RenameParams is the request params for textDocument/rename.
type RenameParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	NewName      string                 `json:"newName"`
}

// WorkspaceEdit is the server's response to a rename request.
//
// The two fields are alternatives, and decoding only Changes is why rename
// silently did nothing: modern gopls always answers with documentChanges
// (its protocol.NewWorkspaceEdit builds only that field, and does not
// downgrade based on client capability), so Changes was always nil, the
// apply loop never ran, and the tool returned "no files were modified" as a
// success string. Other servers still send changes, so both must be handled.
type WorkspaceEdit struct {
	Changes         map[string][]TextEdit `json:"changes,omitempty"`
	DocumentChanges []DocumentChange      `json:"documentChanges,omitempty"`
}

// DocumentChange is one entry of WorkspaceEdit.documentChanges. The array is
// heterogeneous: an entry is either a TextDocumentEdit (textDocument+edits)
// or a resource operation (create/rename/delete, tagged by Kind). Only the
// TextDocumentEdit form is applied; the rest are refused loudly rather than
// dropped, because silently skipping a file delete or rename would leave a
// half-finished refactor that still compiles as something the model did not
// ask for.
type DocumentChange struct {
	// Kind is set only for resource operations: "create", "rename", "delete".
	Kind         string                           `json:"kind,omitempty"`
	TextDocument *VersionedTextDocumentIdentifier `json:"textDocument,omitempty"`
	Edits        []TextEdit                       `json:"edits,omitempty"`
}

// FileEdits normalizes the two WorkspaceEdit shapes into one path-keyed map,
// preferring documentChanges when both are present (the spec says a client
// that supports documentChanges must ignore changes). It returns an error
// naming any resource operation it cannot apply so the caller can refuse the
// whole edit instead of applying part of it.
func (w WorkspaceEdit) FileEdits() (map[string][]TextEdit, error) {
	out := make(map[string][]TextEdit)
	if len(w.DocumentChanges) > 0 {
		for _, dc := range w.DocumentChanges {
			if dc.Kind != "" {
				return nil, fmt.Errorf(
					"lsp: workspace edit contains an unsupported %q file operation; "+
						"refusing to apply a partial refactor", dc.Kind)
			}
			if dc.TextDocument == nil {
				continue
			}
			path, err := URIToPath(dc.TextDocument.URI)
			if err != nil {
				return nil, err
			}
			out[path] = append(out[path], dc.Edits...)
		}
		return out, nil
	}
	for uri, edits := range w.Changes {
		path, err := URIToPath(uri)
		if err != nil {
			return nil, err
		}
		out[path] = append(out[path], edits...)
	}
	return out, nil
}

// TextEdit is a single text replacement in a document.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// ---- Shutdown/Exit ----

// ShutdownResult is the empty result of a shutdown request.
type ShutdownResult struct{}

// ---- URI helpers ----

// PathToURI converts a filesystem path to a file:// URI.
//
// String concatenation is not good enough: a workspace under
// "/Users/me/My Projects/app" produces an invalid URI with a raw space, and
// the properly-escaped "file:///Users/me/My%20Projects/app/main.go" the
// server sends back then has to survive the trip home. url.URL escapes the
// path on the way out and URIToPath unescapes it on the way in, so the round
// trip is lossless.
func PathToURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// URIToPath converts a file:// URI back to a filesystem path, undoing any
// percent-encoding. A non-file URI is an error rather than a silently
// mangled path: strings.TrimPrefix used to leave "%20" sitting literally in
// the path, so os.ReadFile failed with ENOENT — and in the middle of a
// multi-file rename, after other files had already been written.
func URIToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("lsp: invalid document uri %q: %w", uri, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("lsp: expected a file:// uri, got %q", uri)
	}
	return u.Path, nil
}

// pathToURI is the unexported spelling used throughout this package.
func pathToURI(path string) string { return PathToURI(path) }

// EffectivePositionEncoding reports the encoding to use for this server.
// An absent value means utf-16 per LSP 3.17, so this must never be read as
// "empty implies bytes" — doing that is what corrupted files whose lines
// contained non-ASCII before the edited column.
func (c ServerCapabilities) EffectivePositionEncoding() string {
	if c.PositionEncoding == PositionEncodingUTF8 {
		return PositionEncodingUTF8
	}
	return PositionEncodingUTF16
}

// PositionToOffset converts an LSP Position into a byte offset into text.
//
// The naive version added Position.Character straight to the byte index of
// the line start. That is only correct for an all-ASCII line. Under the
// default utf-16 encoding a Character is a count of UTF-16 code units, so
// for `x := "日本語" + foo` the column of `foo` counted 3 units for the
// three CJK runes while each occupies 3 bytes — the offset landed 6 bytes
// early, in the middle of a rune, and the resulting file was no longer
// valid UTF-8.
//
// Offsets are clamped to the line's end rather than running into the next
// line, so a server that reports a column past end-of-line truncates
// harmlessly instead of corrupting the following line.
func PositionToOffset(text string, pos Position, encoding string) int {
	lineStart := 0
	line := 0
	for line < pos.Line {
		idx := strings.IndexByte(text[lineStart:], '\n')
		if idx < 0 {
			return len(text)
		}
		lineStart += idx + 1
		line++
	}

	lineEnd := lineStart + len(text[lineStart:])
	if idx := strings.IndexByte(text[lineStart:], '\n'); idx >= 0 {
		lineEnd = lineStart + idx
	}
	lineText := text[lineStart:lineEnd]

	if encoding == PositionEncodingUTF8 {
		if pos.Character >= len(lineText) {
			return lineEnd
		}
		return lineStart + pos.Character
	}

	// utf-16: walk runes, counting code units (2 for anything outside the
	// BMP, which is what utf16.RuneLen reports).
	units := 0
	for off, r := range lineText {
		if units >= pos.Character {
			return lineStart + off
		}
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1 // unpaired surrogate / invalid rune: count it as one
		}
		units += n
	}
	return lineEnd
}

// symbolKindNames maps the LSP SymbolKind enum (3.17, §Document Symbols) to
// its spelling. The wire type is a bare int, so without this the tools layer
// had nothing human-readable to show and dropped the kind entirely.
var symbolKindNames = [...]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class",
	6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum",
	11: "interface", 12: "function", 13: "variable", 14: "constant",
	15: "string", 16: "number", 17: "boolean", 18: "array", 19: "object",
	20: "key", 21: "null", 22: "enum-member", 23: "struct", 24: "event",
	25: "operator", 26: "type-parameter",
}

// SymbolKindName returns the spelling of an LSP SymbolKind value. An
// unrecognized kind renders as "kind(N)" rather than "" so a newer server's
// value is still visible.
func SymbolKindName(kind int) string {
	if kind >= 0 && kind < len(symbolKindNames) && symbolKindNames[kind] != "" {
		return symbolKindNames[kind]
	}
	return fmt.Sprintf("kind(%d)", kind)
}
