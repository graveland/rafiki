package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/sourcegraph/jsonrpc2"
)

// FakeServer is an in-process LSP server used for testing. It implements
// jsonrpc2.Handler and responds to LSP requests with canned or programmable
// responses. Start it with [NewFakeServerConn], which returns a connected
// jsonrpc2.Conn pair (client conn, server conn) over an in-memory pipe.
type FakeServer struct {
	mu          sync.Mutex
	diagnostics []PublishDiagnosticsParams
	notified    []string // method names that were notified

	// Programmable responses set by tests before the request arrives.
	initializeResult *InitializeResult

	// ShutdownCh is closed when the server receives a shutdown request.
	ShutdownCh chan struct{}
}

// NewFakeServer creates a FakeServer with sensible defaults.
func NewFakeServer() *FakeServer {
	return &FakeServer{
		ShutdownCh: make(chan struct{}),
		initializeResult: &InitializeResult{
			Capabilities: ServerCapabilities{
				TextDocumentSync: &TextDocumentSyncOptions{
					OpenClose: true,
					Change:    SyncKindFull,
				},
				DefinitionProvider:     true,
				ReferencesProvider:     true,
				DocumentSymbolProvider: true,
			},
		},
	}
}

// SetInitializeResult overrides the canned initialize response.
func (fs *FakeServer) SetInitializeResult(r *InitializeResult) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.initializeResult = r
}

// DiagnosticsSnapshot returns a copy of all published diagnostics.
func (fs *FakeServer) DiagnosticsSnapshot() []PublishDiagnosticsParams {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]PublishDiagnosticsParams, len(fs.diagnostics))
	copy(out, fs.diagnostics)
	return out
}

// Notifications returns the method names of all received notifications.
func (fs *FakeServer) Notifications() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]string, len(fs.notified))
	copy(out, fs.notified)
	return out
}

// Handle implements jsonrpc2.Handler.
func (fs *FakeServer) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if req.Notif {
		fs.handleNotification(ctx, conn, req)
		return
	}

	result, err := fs.handleRequest(ctx, req)
	if err != nil {
		_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInternalError,
			Message: err.Error(),
		})
		return
	}
	if result != nil {
		_ = conn.Reply(ctx, req.ID, result)
	}
}

func (fs *FakeServer) handleNotification(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	fs.mu.Lock()
	fs.notified = append(fs.notified, req.Method)
	fs.mu.Unlock()

	switch req.Method {
	case "textDocument/didOpen":
		var params DidOpenTextDocumentParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return
		}
		diag := PublishDiagnosticsParams{
			URI: params.TextDocument.URI,
			Diagnostics: []Diagnostic{
				{
					Range: Range{
						Start: Position{Line: 0, Character: 0},
						End:   Position{Line: 0, Character: 10},
					},
					Severity: SeverityError,
					Message:  fmt.Sprintf("fake diagnostic for %s", params.TextDocument.URI),
					Source:   "fake",
				},
			},
		}
		fs.publishDiag(diag)
		_ = conn.Notify(ctx, "textDocument/publishDiagnostics", diag)
	case "textDocument/didChange":
		var params DidChangeTextDocumentParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return
		}
		diag := PublishDiagnosticsParams{
			URI: params.TextDocument.URI,
			Diagnostics: []Diagnostic{
				{
					Range: Range{
						Start: Position{Line: 1, Character: 0},
						End:   Position{Line: 1, Character: 5},
					},
					Severity: SeverityWarning,
					Message:  fmt.Sprintf("changed: %s", params.TextDocument.URI),
					Source:   "fake",
				},
			},
		}
		fs.publishDiag(diag)
		_ = conn.Notify(ctx, "textDocument/publishDiagnostics", diag)
	case "textDocument/didClose":
		var params DidCloseTextDocumentParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return
		}
		diag := PublishDiagnosticsParams{
			URI:         params.TextDocument.URI,
			Diagnostics: nil,
		}
		fs.publishDiag(diag)
		_ = conn.Notify(ctx, "textDocument/publishDiagnostics", diag)
	case "exit":
		fs.mu.Lock()
		if fs.ShutdownCh != nil {
			select {
			case <-fs.ShutdownCh:
			default:
				close(fs.ShutdownCh)
			}
		}
		fs.mu.Unlock()
	}
}

func (fs *FakeServer) handleRequest(_ context.Context, req *jsonrpc2.Request) (any, error) {
	switch req.Method {
	case "initialize":
		fs.mu.Lock()
		result := fs.initializeResult
		fs.mu.Unlock()
		return result, nil
	case "initialized":
		return nil, nil
	case "shutdown":
		fs.mu.Lock()
		if fs.ShutdownCh != nil {
			select {
			case <-fs.ShutdownCh:
			default:
				close(fs.ShutdownCh)
			}
		}
		fs.mu.Unlock()
		return ShutdownResult{}, nil
	case "textDocument/definition":
		var params DefinitionParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			return nil, fmt.Errorf("unmarshal definition params: %w", err)
		}
		return []Location{{
			URI: params.TextDocument.URI,
			Range: Range{
				Start: Position{Line: 42, Character: 0},
				End:   Position{Line: 42, Character: 10},
			},
		}}, nil
	case "textDocument/references":
		return []Location{{
			URI: "file:///fake/ref.go",
			Range: Range{
				Start: Position{Line: 7, Character: 0},
				End:   Position{Line: 7, Character: 5},
			},
		}}, nil
	default:
		return nil, fmt.Errorf("unhandled method: %s", req.Method)
	}
}

func (fs *FakeServer) publishDiag(p PublishDiagnosticsParams) {
	fs.mu.Lock()
	fs.diagnostics = append(fs.diagnostics, p)
	fs.mu.Unlock()
}

// NewFakeServerConn creates a connected pair of jsonrpc2.Conns speaking to
// each other over an in-memory pipe. The server conn is driven by fs, and the
// returned client conn is what a Client wraps. clientHandler receives
// server-to-client notifications (diagnostics, showMessage, etc.).
func NewFakeServerConn(ctx context.Context, fs *FakeServer, clientHandler jsonrpc2.Handler) (*jsonrpc2.Conn, *jsonrpc2.Conn, io.Closer) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()

	clientRW := &pipeRW{ReadCloser: clientRead, WriteCloser: clientWrite}
	serverRW := &pipeRW{ReadCloser: serverRead, WriteCloser: serverWrite}

	clientStream := jsonrpc2.NewBufferedStream(clientRW, jsonrpc2.VSCodeObjectCodec{})
	serverStream := jsonrpc2.NewBufferedStream(serverRW, jsonrpc2.VSCodeObjectCodec{})

	serverConn := jsonrpc2.NewConn(ctx, serverStream, fs)

	return jsonrpc2.NewConn(ctx, clientStream, clientHandler), serverConn, &pipeCloser{
		clientRead, clientWrite, serverRead, serverWrite,
	}
}

// pipeRW implements io.ReadWriteCloser over two pipes.
type pipeRW struct {
	io.ReadCloser
	io.WriteCloser
}

func (p *pipeRW) Close() error {
	p.ReadCloser.Close()
	return p.WriteCloser.Close()
}

// pipeCloser closes all four pipe halves.
type pipeCloser struct {
	cr, cw, sr, sw io.Closer
}

func (pc *pipeCloser) Close() error {
	pc.cr.Close()
	pc.cw.Close()
	pc.sr.Close()
	pc.sw.Close()
	return nil
}
