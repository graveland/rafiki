package lsp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/multigres/testkit/assert"
)

func TestClient_Initialize(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()

	client := &Client{
		name:  "fake",
		diags: make(map[string][]Diagnostic),
	}

	clientConn, serverConn, closer := NewFakeServerConn(ctx, fs, &clientHandler{c: client})
	defer closer.Close()
	defer clientConn.Close()
	defer serverConn.Close()

	client.conn.Store(clientConn)

	ck.Require().NoError(client.Initialize(ctx, "/fake/root"), "Initialize")

	ck.True(client.serverCaps.DefinitionProvider, "expected DefinitionProvider to be true")
}

func TestClient_DidOpen(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	connPair(ctx, t, fs, func(client *Client) {
		c.Require().NoError(client.DidOpen(ctx, "/fake/root/main.go", "package main"), "DidOpen")

		// Wait briefly for the notification to be processed.
		time.Sleep(50 * time.Millisecond)

		// The fake server publishes a diagnostic on didOpen.
		diags, err := client.Diagnostics(ctx, "/fake/root/main.go")
		c.Require().NoError(err, "Diagnostics")
		c.Require().Len(diags, 1, "expected 1 diagnostic, got %d", len(diags))
		c.Eq(SeverityError, diags[0].Severity, "expected error severity, got")
	})
}

func TestClient_DidChange(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	connPair(ctx, t, fs, func(client *Client) {
		// Open first to trigger the fake diagnostic.
		c.Require().NoError(client.DidOpen(ctx, "/fake/root/main.go", "package main"), "DidOpen")
		time.Sleep(50 * time.Millisecond)

		// Change triggers a different diagnostic.
		c.Require().NoError(client.DidChange(ctx, "/fake/root/main.go", "package main\n// changed"), "DidChange")
		time.Sleep(50 * time.Millisecond)

		diags, err := client.Diagnostics(ctx, "/fake/root/main.go")
		c.Require().NoError(err, "Diagnostics")
		c.Require().Len(diags, 1, "expected 1 diagnostic after change, got %d", len(diags))
		c.Eq(SeverityWarning, diags[0].Severity, "expected warning severity, got")
	})
}

func TestClient_DiagnosticsEmptyVsNotYetPublished(t *testing.T) {
	c := assert.NewCollecting(t)
	// Diagnostics on a never-opened file must return nil (no diags have
	// been published for it), but Diagnostics on a file that was opened
	// and for which the server hasn't published yet must also return nil.
	// The distinction matters because callers use DiagnosticsVersion to
	// wait for the first publish.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()

	connPair(ctx, t, fs, func(client *Client) {
		// Never-opened file: diagnostics should be empty.
		diags, err := client.Diagnostics(ctx, "/fake/root/never_opened.go")
		c.Require().NoError(err, "Diagnostics")
		c.Empty(diags, "expected 0 diagnostics for never-opened file, got %d", len(diags))

		// DiagnosticsVersion at start.
		startVer := client.DiagnosticsVersion()

		// Open a file.
		c.Require().NoError(client.DidOpen(ctx, "/fake/root/other.go", "package other"), "DidOpen")

		// Wait for diagnostics to be published.
		c.Require().NoError(client.WaitForInitialDiagnostics(ctx, startVer, 2*time.Second), "WaitForInitialDiagnostics")

		endVer := client.DiagnosticsVersion()
		c.Greater(startVer, endVer, "DiagnosticsVersion should have incremented after publish")
	})
}

func TestClient_Shutdown(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()

	client := &Client{
		name:  "fake",
		diags: make(map[string][]Diagnostic),
	}

	clientConn, _, closer := NewFakeServerConn(ctx, fs, &clientHandler{c: client})
	defer closer.Close()

	client.conn.Store(clientConn)

	ck.Require().NoError(client.Initialize(ctx, "/fake/root"), "Initialize")

	ck.Require().NoError(client.Shutdown(ctx), "Shutdown")

	// Check that the server received shutdown.
	select {
	case <-fs.ShutdownCh:
		// ok
	case <-time.After(1 * time.Second):
		t.Error("server did not receive shutdown")
	}

	// Operations after shutdown should fail.
	err := client.DidOpen(ctx, "/x.go", "x")
	ck.Error(err, "expected error after shutdown, got nil")
}

func TestClient_WaitForInitialDiagnostics_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	connPair(ctx, t, fs, func(client *Client) {
		// Don't open any file — WaitForInitialDiagnostics should time out.
		waitCtx, waitCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer waitCancel()

		err := client.WaitForInitialDiagnostics(waitCtx, client.DiagnosticsVersion(), 500*time.Millisecond)
		assert.NewCollecting(t).Error(err, "expected timeout error, got nil")
	})
}

// TestClientHandler_WorkspaceConfiguration is the regression test for
// Finding 11: the client used to answer EVERY server-to-client request with
// MethodNotFound, including workspace/configuration -- which gopls issues
// unprompted during startup. Using the fake server harness to drive a real
// server-to-client request (rather than calling HandleWorkspaceConfiguration
// directly) proves the request actually reaches the handler through
// clientHandler.Handle and gets a well-formed reply, not just that the
// unmarshal-and-shape helper works in isolation.
func TestClientHandler_WorkspaceConfiguration(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	client := &Client{name: "fake", diags: make(map[string][]Diagnostic)}
	clientConn, serverConn, closer := NewFakeServerConn(ctx, fs, &clientHandler{c: client})
	defer closer.Close()
	defer clientConn.Close()
	defer serverConn.Close()
	client.conn.Store(clientConn)

	// The shape gopls actually sends: one item per setting it wants read.
	params := struct {
		Items []struct {
			Section string `json:"section"`
		} `json:"items"`
	}{Items: []struct {
		Section string `json:"section"`
	}{{Section: "gopls"}, {Section: "go"}}}

	var result []any
	ck.Require().NoError(serverConn.Call(ctx, "workspace/configuration", params, &result), "workspace/configuration returned an error, want a result")
	ck.Require().Len(result, 2, "got %d results, want 2 (one per requested item, in order)", len(result))
	for i, r := range result {
		ck.Nil(r, "item %d: got %v, want null (we have no configuration store)", i, r)
	}
}

// TestClientHandler_RegisterCapability covers the other startup request
// gopls issues unprompted: it used to get MethodNotFound like everything
// else, and now gets a plain acknowledgement.
func TestClientHandler_RegisterCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	client := &Client{name: "fake", diags: make(map[string][]Diagnostic)}
	clientConn, serverConn, closer := NewFakeServerConn(ctx, fs, &clientHandler{c: client})
	defer closer.Close()
	defer clientConn.Close()
	defer serverConn.Close()
	client.conn.Store(clientConn)

	for _, method := range []string{"client/registerCapability", "client/unregisterCapability"} {
		var result any
		assert.NewCollecting(t).NoError(serverConn.Call(ctx, method, struct{}{}, &result), "%s returned an error, want an acknowledgement", method)
	}
}

// TestClientHandler_ShowMessageRequestIsReachable is the regression test for
// the dead-code half of Finding 11: window/showMessageRequest arrives as a
// REQUEST, not a notification, so the old "case window/showMessageRequest"
// in handleNotification could never run -- it was short-circuited by
// Handle's blanket MethodNotFound for every non-notification request. This
// proves the method is now routed to HandleShowMessageRequest and gets a
// decline (null) reply instead of an error.
func TestClientHandler_ShowMessageRequestIsReachable(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	client := &Client{name: "fake", diags: make(map[string][]Diagnostic)}
	clientConn, serverConn, closer := NewFakeServerConn(ctx, fs, &clientHandler{c: client})
	defer closer.Close()
	defer clientConn.Close()
	defer serverConn.Close()
	client.conn.Store(clientConn)

	params := struct {
		Type    int    `json:"type"`
		Message string `json:"message"`
	}{Type: 1, Message: "retry?"}

	var result any
	ck.Require().NoError(serverConn.Call(ctx, "window/showMessageRequest", params, &result), "window/showMessageRequest returned an error, want a decline reply")
	ck.Nil(result, "got")
}

// TestClientHandler_UnknownRequestIsMethodNotFound pins that a genuinely
// unsupported server-to-client request still gets MethodNotFound: the fix
// for Finding 11 must not turn Handle into an always-succeeds stub.
func TestClientHandler_UnknownRequestIsMethodNotFound(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	client := &Client{name: "fake", diags: make(map[string][]Diagnostic)}
	clientConn, serverConn, closer := NewFakeServerConn(ctx, fs, &clientHandler{c: client})
	defer closer.Close()
	defer clientConn.Close()
	defer serverConn.Close()
	client.conn.Store(clientConn)

	var result any
	err := serverConn.Call(ctx, "workspace/definitelyNotARealMethod", struct{}{}, &result)
	ck.Error(err, "expected an error for a genuinely unknown method, got nil")
	var rpcErr *jsonrpc2.Error
	ck.False(!errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc2.CodeMethodNotFound, "got %v, want a jsonrpc2.Error with code CodeMethodNotFound", err)
}

// connPair creates a client and fake server, initializes the client, and
// calls fn with the initialized client.
func connPair(ctx context.Context, t *testing.T, fs *FakeServer, fn func(*Client)) {
	t.Helper()

	client := &Client{
		name:  "fake",
		diags: make(map[string][]Diagnostic),
	}

	clientConn, _, closer := NewFakeServerConn(ctx, fs, &clientHandler{c: client})
	defer closer.Close()
	defer clientConn.Close()

	client.conn.Store(clientConn)

	assert.NewAborting(t).NoError(client.Initialize(ctx, "/fake/root"), "Initialize")

	fn(client)
}
