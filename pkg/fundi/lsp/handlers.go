package lsp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/sourcegraph/jsonrpc2"
)

// HandlePublishDiagnostics is the handler for textDocument/publishDiagnostics
// notifications from the LSP server. It updates the client's diagnostic cache.
func HandlePublishDiagnostics(client *Client, params *json.RawMessage) error {
	var diagParams PublishDiagnosticsParams
	if err := json.Unmarshal(*params, &diagParams); err != nil {
		return err
	}

	client.mu.Lock()
	if len(diagParams.Diagnostics) == 0 {
		delete(client.diags, diagParams.URI)
	} else {
		client.diags[diagParams.URI] = diagParams.Diagnostics
	}
	client.mu.Unlock()
	client.diagsVersion.Add(1)
	return nil
}

// HandleShowMessage handles window/showMessage and window/logMessage.
func HandleShowMessage(params *json.RawMessage) {
	var msg struct {
		Type    int    `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(*params, &msg); err != nil {
		slog.Debug("lsp: unmarshal show message", "error", err)
		return
	}

	switch msg.Type {
	case 1: // Error
		slog.Error("lsp server", "message", msg.Message)
	case 2: // Warning
		slog.Warn("lsp server", "message", msg.Message)
	case 3: // Info
		slog.Info("lsp server", "message", msg.Message)
	case 4: // Log
		slog.Debug("lsp server", "message", msg.Message)
	default:
		slog.Debug("lsp server", "message", msg.Message)
	}
}

// HandleShowMessageRequest handles window/showMessageRequest (requests that
// expect a client response). We always respond with a default/decline action.
func HandleShowMessageRequest(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	_ = conn.Reply(ctx, req.ID, nil)
}
