package lsp

import (
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

	// closed is set atomically when the client has been shut down.
	closed atomic.Bool
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
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	c := &Client{
		name:  cfg.Name,
		diags: make(map[string][]Diagnostic),
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Cwd
	cmd.Stderr = os.Stderr // server log output

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
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

	slog.Debug("lsp: client started", "name", cfg.Name, "pid", c.pid, "cwd", cfg.Cwd)
	return c, nil
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
	if err := conn.Call(ctx, "initialize", params, &result); err != nil {
		return fmt.Errorf("lsp: initialize: %w", err)
	}

	c.mu.Lock()
	c.serverCaps = result.Capabilities
	c.mu.Unlock()

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

	params := DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:     pathToURI(path),
			Version: 1,
			Text:    content,
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

	params := DidChangeTextDocumentParams{
		TextDocument: VersionedTextDocumentIdentifier{
			URI:     pathToURI(path),
			Version: 2, // simple counter for full-text sync
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
		// Give the process a moment to exit, then kill if needed.
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-done
		}
	}

	slog.Debug("lsp: client shut down", "name", c.name)
	return nil
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
