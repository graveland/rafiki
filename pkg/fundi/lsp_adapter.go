package fundi

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// lspClientAdapter adapts *lsp.Manager to tools.LSPClient so the tools
// package does not import the lsp package.
type lspClientAdapter struct {
	mgr *lsp.Manager
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
