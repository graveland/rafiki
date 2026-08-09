package lsp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

// ErrServerGone is returned when the LSP server has crashed or been shut down.
var ErrServerGone = errors.New("lsp: server is gone")

// Client is a JSON-RPC connection to a single LSP server process. It handles
// initialize, document sync, diagnostics, and (in later phases) navigation.
// All methods are safe for concurrent use.
type Client struct {
	name string // language name for logging
	conn atomic.Pointer[jsonrpc2.Conn]
	pid  int // os process PID, for cleanup

	mu sync.Mutex

	// diags caches the last published diagnostics per URI.
	diags map[string][]Diagnostic

	// diagsVersion increments on every publishDiagnostics notification,
	// so callers can poll for new results.
	diagsVersion atomic.Int64

	// serverCaps is the server's InitializeResult capabilities.
	serverCaps ServerCapabilities

	cmd *exec.Cmd // the server process, for restart

	rootURI string

	// posEncoding is the negotiated position encoding, stored separately
	// from serverCaps so the edit path can read it without taking mu.
	posEncoding atomic.Value // string

	// docVersions tracks the last version sent per URI. LSP requires the
	// version to increase on every didChange; a hardcoded constant is a
	// spec violation that tolerant servers happen to ignore.
	docVersions sync.Map // string -> *atomic.Int64

	// dead is set by the single waiter goroutine when the process exits,
	// for any reason. Manager.For used to test cmd.ProcessState instead,
	// which is written by cmd.Wait() on another goroutine with no
	// synchronization — an unguarded read/write pair — and which stays nil
	// forever when nothing calls Wait, so a crashed server was never
	// detected. An atomic flag owned by exactly one waiter fixes both.
	dead atomic.Bool

	// exited closes when the process has been reaped, so callers can wait
	// on it instead of polling.
	exited chan struct{}

	// closed is set atomically when the client has been shut down.
	closed atomic.Bool
}

// Dead reports whether the server process has exited, for any reason.
func (c *Client) Dead() bool { return c.dead.Load() }

// PositionEncoding returns the encoding negotiated at initialize, defaulting
// to utf-16 (the LSP default) before initialize has run.
func (c *Client) PositionEncoding() string {
	if s, ok := c.posEncoding.Load().(string); ok && s != "" {
		return s
	}
	return PositionEncodingUTF16
}

// nextVersion returns a monotonically increasing document version for uri.
func (c *Client) nextVersion(uri string) int {
	v, _ := c.docVersions.LoadOrStore(uri, new(atomic.Int64))
	return int(v.(*atomic.Int64).Add(1))
}

// Request timeouts. Every conn.Call previously took the caller's ctx
// unchanged, and executeBatch applies no per-tool deadline, so a server that
// accepted the connection and then never answered blocked the tool call --
// and therefore the whole agent turn -- forever rather than until a timeout.
const (
	// initializeTimeout bounds the handshake. Indexing a large module can
	// legitimately take a while, so this is generous.
	initializeTimeout = 30 * time.Second
	// requestTimeout bounds every other request.
	requestTimeout = 15 * time.Second
)

// callCtx derives a bounded context for one request, preserving any earlier
// deadline the caller already set.
func callCtx(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// ClientConfig holds the configuration for starting an LSP server.
type ClientConfig struct {
	// Name is the human-readable name for logging (e.g. "go").
	Name string
	// Command is the server binary path.
	Command string
	// Args are additional arguments passed to the server.
	Args []string
	// Cwd is the working directory (project root) for the server.
	Cwd string
}

// NewClient starts a new LSP server process and returns a Client connected to it.
// The Client must be initialized with [Client.Initialize] before use.
// The ctx passed here governs the SERVER PROCESS LIFETIME, not one request.
// Callers must pass a long-lived, manager-owned context: exec.CommandContext
// kills the process when its ctx is done, and passing a tool call's ctx meant
// gopls was SIGKILLed the moment the turn that spawned it ended. The next
// turn then got the same dead client back from the manager and every call
// failed with "jsonrpc2: connection is closed" for the rest of the session.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	c := &Client{
		name:   cfg.Name,
		diags:  make(map[string][]Diagnostic),
		exited: make(chan struct{}),
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Cwd

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	// The server's own logging goes through slog rather than straight to the
	// daemon's stderr, where it interleaved with rafiki's output unattributed.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stderr pipe: %w", err)
	}

	// We need a ReadWriteCloser that combines stdin/stdout.
	rw := &readWriteCloser{ReadCloser: stdout, WriteCloser: stdin}
	// VSCodeObjectCodec is the Content-Length-prefixed framing that LSP uses.
	stream := jsonrpc2.NewBufferedStream(rw, jsonrpc2.VSCodeObjectCodec{})

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", cfg.Command, err)
	}

	c.cmd = cmd
	c.pid = cmd.Process.Pid
	c.rootURI = pathToURI(cfg.Cwd)

	handler := &clientHandler{c: c}
	conn := jsonrpc2.NewConn(ctx, stream, handler)
	c.conn.Store(conn)

	go c.drainStderr(stderr)

	// Exactly one goroutine owns cmd.Wait(), so the process is always reaped
	// and liveness is published through an atomic rather than by peeking at
	// ProcessState from another goroutine. Shutdown waits on c.exited rather
	// than calling Wait itself, which would be a second concurrent Wait.
	go func() {
		err := cmd.Wait()
		c.dead.Store(true)
		close(c.exited)
		if err != nil && !c.closed.Load() {
			slog.Warn("lsp: server exited", "name", cfg.Name, "pid", c.pid, "err", err)
		} else {
			slog.Debug("lsp: server exited", "name", cfg.Name, "pid", c.pid)
		}
	}()

	slog.Debug("lsp: client started", "name", cfg.Name, "pid", c.pid, "cwd", cfg.Cwd)
	return c, nil
}

// drainStderr forwards the server's stderr to slog at debug, tagged with the
// server name. Without this the pipe would also fill and block the server
// once it had written a pipe buffer's worth of logs.
func (c *Client) drainStderr(r io.ReadCloser) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 8*1024), 256*1024)
	for sc.Scan() {
		slog.Debug("lsp: server stderr", "name", c.name, "line", sc.Text())
	}
}

// readWriteCloser implements io.ReadWriteCloser from a separate Reader and Writer.
type readWriteCloser struct {
	io.ReadCloser
	io.WriteCloser
}

func (r *readWriteCloser) Close() error {
	r.ReadCloser.Close()
	return r.WriteCloser.Close()
}

// Initialize performs the LSP initialize handshake and sends the initialized
// notification. It must be called once before any other method.
func (c *Client) Initialize(ctx context.Context, root string) error {
	conn := c.conn.Load()
	if conn == nil {
		return ErrServerGone
	}

	params := InitializeParams{
		ProcessID: os.Getpid(),
		RootURI:   pathToURI(root),
		Capabilities: ClientCapabilities{
			// utf-8 first: when the server can index by byte the UTF-16
			// conversion is skipped entirely. Servers that cannot answer
			// utf-16, or omit the field, which per spec means the same
			// thing. Declaring nothing at all left us on the utf-16 default
			// while the code indexed by byte, which is what corrupted files
			// containing non-ASCII earlier on an edited line.
			General: &GeneralClientCapabilities{
				PositionEncodings: []string{PositionEncodingUTF8, PositionEncodingUTF16},
			},
			TextDocument: &TextDocumentClientCapabilities{
				Synchronization: &SynchronizationCapabilities{
					DidSave: false,
				},
				PublishDiagnostics: &PublishDiagnosticsCapabilities{},
				Definition:         &DefinitionCapabilities{},
				References:         &ReferencesCapabilities{},
				DocumentSymbol: &DocumentSymbolCapabilities{
					Hierarchical: true,
				},
			},
		},
	}

	var result InitializeResult
	initCtx, cancelInit := callCtx(ctx, initializeTimeout)
	defer cancelInit()
	if err := conn.Call(initCtx, "initialize", params, &result); err != nil {
		return fmt.Errorf("lsp: initialize: %w", err)
	}

	c.mu.Lock()
	c.serverCaps = result.Capabilities
	c.mu.Unlock()
	c.posEncoding.Store(result.Capabilities.EffectivePositionEncoding())

	// Send initialized notification.
	if err := conn.Notify(ctx, "initialized", struct{}{}); err != nil {
		return fmt.Errorf("lsp: initialized notify: %w", err)
	}

	slog.Debug("lsp: initialized", "name", c.name, "root", root)
	return nil
}

// DidOpen notifies the server that a document has been opened.
func (c *Client) DidOpen(ctx context.Context, path, content string) error {
	conn := c.conn.Load()
	if conn == nil {
		return ErrServerGone
	}

	uri := pathToURI(path)
	params := DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI: uri,
			// gopls falls back to the file extension when languageId is
			// empty, but other servers key off it, so send the configured
			// server name.
			LanguageID: c.name,
			Version:    c.nextVersion(uri),
			Text:       content,
		},
	}

	return conn.Notify(ctx, "textDocument/didOpen", params)
}

// DidChange notifies the server that a document's content has changed
// (full-text sync).
func (c *Client) DidChange(ctx context.Context, path, content string) error {
	conn := c.conn.Load()
	if conn == nil {
		return ErrServerGone
	}

	uri := pathToURI(path)
	params := DidChangeTextDocumentParams{
		TextDocument: VersionedTextDocumentIdentifier{
			URI: uri,
			// A real per-document counter. This was hardcoded to 2 for every
			// change to every document; LSP requires the version to increase
			// on each didChange, and a server that enforces that would reject
			// or mis-order the second edit to a file.
			Version: c.nextVersion(uri),
		},
		ContentChanges: []TextDocumentContentChangeEvent{
			{Text: content},
		},
	}

	return conn.Notify(ctx, "textDocument/didChange", params)
}

// DidClose notifies the server that a document has been closed.
func (c *Client) DidClose(ctx context.Context, path string) error {
	conn := c.conn.Load()
	if conn == nil {
		return ErrServerGone
	}

	params := DidCloseTextDocumentParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(path)},
	}

	return conn.Notify(ctx, "textDocument/didClose", params)
}

// Diagnostics returns the cached diagnostics for a given file path.
func (c *Client) Diagnostics(_ context.Context, path string) ([]Diagnostic, error) {
	uri := pathToURI(path)
	c.mu.Lock()
	diags, ok := c.diags[uri]
	c.mu.Unlock()

	if !ok {
		return nil, nil
	}
	out := make([]Diagnostic, len(diags))
	copy(out, diags)
	return out, nil
}

// DiagnosticsVersion returns the current diagnostics version, incrementing
// on every publishDiagnostics notification. A caller can poll this to detect
// new diagnostics.
func (c *Client) DiagnosticsVersion() int64 {
	return c.diagsVersion.Load()
}

// WaitForInitialDiagnostics waits up to timeout for diagnostic notifications
// to arrive after version startVersion. It returns immediately if the current
// version is already greater than startVersion.
func (c *Client) WaitForInitialDiagnostics(ctx context.Context, startVersion int64, timeout time.Duration) error {
	// Check immediately — diagnostics may have arrived before this call.
	if c.diagsVersion.Load() > startVersion {
		return nil
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("lsp: timeout waiting for diagnostics after %v", timeout)
		case <-ticker.C:
			if c.diagsVersion.Load() > startVersion {
				return nil
			}
		}
	}
}

// Shutdown gracefully shuts down the LSP server (shutdown + exit).
func (c *Client) Shutdown(ctx context.Context) error {
	if c.closed.Swap(true) {
		return nil // already shut down
	}

	conn := c.conn.Load()
	if conn != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		// Try graceful shutdown.
		var result ShutdownResult
		if err := conn.Call(shutdownCtx, "shutdown", nil, &result); err != nil {
			slog.Warn("lsp: graceful shutdown failed", "name", c.name, "error", err)
		}
		_ = conn.Notify(shutdownCtx, "exit", nil)

		conn.Close()
	}

	if c.cmd != nil && c.cmd.Process != nil {
		// Wait on the owning goroutine's channel rather than calling
		// cmd.Wait() here: a second concurrent Wait on the same Cmd is a
		// data race on ProcessState and returns an error for whichever
		// loses. Give the process a moment to exit on its own, then kill.
		select {
		case <-c.exited:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.exited
		}
	}

	slog.Debug("lsp: client shut down", "name", c.name)
	return nil
}

// Definition resolves the definition location for the symbol at the given
// 0-based line and character.
func (c *Client) Definition(ctx context.Context, path string, line, character int) ([]Location, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := DefinitionParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(path)},
		Position:     Position{Line: line, Character: character},
	}
	var result []Location
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "textDocument/definition", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: definition: %w", err)
	}
	return result, nil
}

// References finds all references to the symbol at the given position.
func (c *Client) References(ctx context.Context, path string, line, character int) ([]Location, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := ReferenceParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(path)},
		Position:     Position{Line: line, Character: character},
		Context:      ReferenceContext{IncludeDeclaration: true},
	}
	var result []Location
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "textDocument/references", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: references: %w", err)
	}
	return result, nil
}

// DocumentSymbols returns the structured symbol outline for a file.
func (c *Client) DocumentSymbols(ctx context.Context, path string) ([]DocumentSymbol, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := DocumentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(path)},
	}
	var result []DocumentSymbol
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "textDocument/documentSymbol", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: documentSymbol: %w", err)
	}
	return result, nil
}

// WorkspaceSymbols searches the workspace for symbols matching query.
func (c *Client) WorkspaceSymbols(ctx context.Context, query string) ([]SymbolInformation, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := WorkspaceSymbolParams{Query: query}
	var result []SymbolInformation
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "workspace/symbol", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: workspace/symbol: %w", err)
	}
	return result, nil
}

// PrepareCallHierarchy prepares the call hierarchy at the given position.
func (c *Client) PrepareCallHierarchy(ctx context.Context, path string, line, character int) ([]CallHierarchyItem, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := CallHierarchyPrepareParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(path)},
		Position:     Position{Line: line, Character: character},
	}
	var result []CallHierarchyItem
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "textDocument/prepareCallHierarchy", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: prepareCallHierarchy: %w", err)
	}
	return result, nil
}

// IncomingCalls returns callers of the given call hierarchy item.
func (c *Client) IncomingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyIncomingCall, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := CallHierarchyIncomingCallsParams{Item: item}
	var result []CallHierarchyIncomingCall
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "callHierarchy/incomingCalls", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: incomingCalls: %w", err)
	}
	return result, nil
}

// OutgoingCalls returns callees of the given call hierarchy item.
func (c *Client) OutgoingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyOutgoingCall, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := CallHierarchyOutgoingCallsParams{Item: item}
	var result []CallHierarchyOutgoingCall
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "callHierarchy/outgoingCalls", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: outgoingCalls: %w", err)
	}
	return result, nil
}

// Rename requests a workspace-wide rename of the symbol at the given position.
func (c *Client) Rename(ctx context.Context, path string, line, character int, newName string) (*WorkspaceEdit, error) {
	conn := c.conn.Load()
	if conn == nil {
		return nil, ErrServerGone
	}
	params := RenameParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(path)},
		Position:     Position{Line: line, Character: character},
		NewName:      newName,
	}
	var result WorkspaceEdit
	reqCtx, cancelReq := callCtx(ctx, requestTimeout)
	defer cancelReq()
	if err := conn.Call(reqCtx, "textDocument/rename", params, &result); err != nil {
		return nil, fmt.Errorf("lsp: rename: %w", err)
	}
	return &result, nil
}

// clientHandler handles incoming JSON-RPC requests/notifications from the LSP server.
type clientHandler struct {
	c *Client
}

func (h *clientHandler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if req.Notif {
		h.handleNotification(ctx, req)
		return
	}
	// We don't expect server-to-client requests in our minimal usage.
	_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
		Code:    jsonrpc2.CodeMethodNotFound,
		Message: fmt.Sprintf("unexpected server request: %s", req.Method),
	})
}

func (h *clientHandler) handleNotification(ctx context.Context, req *jsonrpc2.Request) {
	conn := h.c.conn.Load()
	switch req.Method {
	case "textDocument/publishDiagnostics":
		if req.Params == nil {
			return
		}
		if err := HandlePublishDiagnostics(h.c, req.Params); err != nil {
			slog.Warn("lsp: unmarshal diagnostics", "error", err)
		}
	case "window/showMessage", "window/logMessage":
		if req.Params != nil {
			HandleShowMessage(req.Params)
		}
	case "window/showMessageRequest":
		// We don't support interactive message requests.
		if conn != nil {
			HandleShowMessageRequest(ctx, conn, req)
		}
	}
}
