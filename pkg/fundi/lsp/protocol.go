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
}

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
type WorkspaceEdit struct {
	Changes map[string][]TextEdit `json:"changes,omitempty"`
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

// pathToURI converts a filesystem path to a file:// URI.
func pathToURI(path string) string {
	return "file://" + path
}
