package lsp

import (
	"context"
	"encoding/json"
	"fmt"
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

// workspaceConfigurationParams mirrors the params of a server-initiated
// workspace/configuration request: one ConfigurationItem per setting the
// server wants read. We only need Items' length to shape the reply, so the
// item fields themselves (scopeUri, section) are not modeled.
type workspaceConfigurationParams struct {
	Items []json.RawMessage `json:"items"`
}

// HandleWorkspaceConfiguration answers a workspace/configuration request.
// We have no configuration store of our own, so every requested item gets a
// null settings value -- which is a valid, spec-compliant answer, not an
// error. gopls sends this unprompted during startup; the client used to
// answer every server-to-client request with MethodNotFound, and a server
// stricter than gopls could plausibly treat that as fatal to its own
// handshake rather than "the client has no opinion."
//
// The response MUST be an array with exactly one element per requested
// item, in the same order: a bare null or a wrong-length array leaves the
// server unable to match answers back to its questions.
func HandleWorkspaceConfiguration(params *json.RawMessage) ([]any, error) {
	if params == nil {
		return []any{}, nil
	}
	var p workspaceConfigurationParams
	if err := json.Unmarshal(*params, &p); err != nil {
		return nil, fmt.Errorf("lsp: unmarshal workspace/configuration params: %w", err)
	}
	return make([]any, len(p.Items)), nil
}
