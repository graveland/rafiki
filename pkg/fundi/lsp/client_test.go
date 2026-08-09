package lsp

import (
	"context"
	"testing"
	"time"
)

func TestClient_Initialize(t *testing.T) {
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

	if err := client.Initialize(ctx, "/fake/root"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if !client.serverCaps.DefinitionProvider {
		t.Error("expected DefinitionProvider to be true")
	}
}

func TestClient_DidOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	connPair(ctx, t, fs, func(client *Client) {
		if err := client.DidOpen(ctx, "/fake/root/main.go", "package main"); err != nil {
			t.Fatalf("DidOpen: %v", err)
		}

		// Wait briefly for the notification to be processed.
		time.Sleep(50 * time.Millisecond)

		// The fake server publishes a diagnostic on didOpen.
		diags, err := client.Diagnostics(ctx, "/fake/root/main.go")
		if err != nil {
			t.Fatalf("Diagnostics: %v", err)
		}
		if len(diags) != 1 {
			t.Fatalf("expected 1 diagnostic, got %d", len(diags))
		}
		if diags[0].Severity != SeverityError {
			t.Errorf("expected error severity, got %v", diags[0].Severity)
		}
	})
}

func TestClient_DidChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fs := NewFakeServer()
	connPair(ctx, t, fs, func(client *Client) {
		// Open first to trigger the fake diagnostic.
		if err := client.DidOpen(ctx, "/fake/root/main.go", "package main"); err != nil {
			t.Fatalf("DidOpen: %v", err)
		}
		time.Sleep(50 * time.Millisecond)

		// Change triggers a different diagnostic.
		if err := client.DidChange(ctx, "/fake/root/main.go", "package main\n// changed"); err != nil {
			t.Fatalf("DidChange: %v", err)
		}
		time.Sleep(50 * time.Millisecond)

		diags, err := client.Diagnostics(ctx, "/fake/root/main.go")
		if err != nil {
			t.Fatalf("Diagnostics: %v", err)
		}
		if len(diags) != 1 {
			t.Fatalf("expected 1 diagnostic after change, got %d", len(diags))
		}
		if diags[0].Severity != SeverityWarning {
			t.Errorf("expected warning severity, got %v", diags[0].Severity)
		}
	})
}

func TestClient_DiagnosticsEmptyVsNotYetPublished(t *testing.T) {
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
		if err != nil {
			t.Fatalf("Diagnostics: %v", err)
		}
		if len(diags) != 0 {
			t.Errorf("expected 0 diagnostics for never-opened file, got %d", len(diags))
		}

		// DiagnosticsVersion at start.
		startVer := client.DiagnosticsVersion()

		// Open a file.
		if err := client.DidOpen(ctx, "/fake/root/other.go", "package other"); err != nil {
			t.Fatalf("DidOpen: %v", err)
		}

		// Wait for diagnostics to be published.
		if err := client.WaitForInitialDiagnostics(ctx, startVer, 2*time.Second); err != nil {
			t.Fatalf("WaitForInitialDiagnostics: %v", err)
		}

		endVer := client.DiagnosticsVersion()
		if endVer <= startVer {
			t.Error("DiagnosticsVersion should have incremented after publish")
		}
	})
}

func TestClient_Shutdown(t *testing.T) {
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

	if err := client.Initialize(ctx, "/fake/root"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Check that the server received shutdown.
	select {
	case <-fs.ShutdownCh:
		// ok
	case <-time.After(1 * time.Second):
		t.Error("server did not receive shutdown")
	}

	// Operations after shutdown should fail.
	err := client.DidOpen(ctx, "/x.go", "x")
	if err == nil {
		t.Error("expected error after shutdown, got nil")
	}
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
		if err == nil {
			t.Error("expected timeout error, got nil")
		}
	})
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

	if err := client.Initialize(ctx, "/fake/root"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	fn(client)
}
