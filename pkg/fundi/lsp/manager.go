package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ServerConfig describes a single LSP server definition from the user config.
type ServerConfig struct {
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	Extensions []string `json:"extensions,omitempty"` // e.g. [".go", ".mod"]
}

// Config holds the full LSP configuration from the user's JSON file.
type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

// Manager owns the lifecycle of LSP clients: one client per language server,
// started lazily on first use, shut down with the runtime.
type Manager struct {
	cfg     Config
	cwd     string
	clients map[string]*Client // language name -> client
	mu      sync.Mutex
	closed  bool
}

// NewManager creates a Manager from the given configuration and working directory.
func NewManager(cfg Config, cwd string) *Manager {
	return &Manager{
		cfg:     cfg,
		cwd:     cwd,
		clients: make(map[string]*Client),
	}
}

// For returns the Client responsible for the file at the given path.
// It lazily starts the appropriate LSP server if one matches and hasn't
// been started yet. Returns nil, nil if no configured server matches.
func (m *Manager) For(ctx context.Context, path string) (*Client, error) {
	name, cfg, ok := m.serverFor(path)
	if !ok {
		return nil, nil
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("lsp: manager is shut down")
	}

	client, exists := m.clients[name]
	if exists {
		// Check if the client's server process is still alive.
		if client.cmd != nil && client.cmd.Process != nil {
			// Non-blocking check: if the process already exited, restart.
			if client.cmd.ProcessState != nil && client.cmd.ProcessState.Exited() {
				slog.Info("lsp: server exited, restarting", "name", name)
				_ = client.Shutdown(ctx)
				delete(m.clients, name)
				exists = false
			}
		}
	}
	m.mu.Unlock()

	if exists {
		return client, nil
	}

	// Start a new server.
	client, err := NewClient(ctx, ClientConfig{
		Name:    name,
		Command: cfg.Command,
		Args:    cfg.Args,
		Cwd:     m.cwd,
	})
	if err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", name, err)
	}

	if err := client.Initialize(ctx, m.cwd); err != nil {
		_ = client.Shutdown(ctx)
		return nil, fmt.Errorf("lsp: initialize %s: %w", name, err)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = client.Shutdown(ctx)
		return nil, fmt.Errorf("lsp: manager is shut down")
	}
	// Check if another goroutine already started this server.
	if existing, ok := m.clients[name]; ok {
		m.mu.Unlock()
		_ = client.Shutdown(ctx)
		return existing, nil
	}
	m.clients[name] = client
	m.mu.Unlock()

	return client, nil
}

// serverFor finds the matching server config for a file path.
func (m *Manager) serverFor(path string) (string, ServerConfig, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "", ServerConfig{}, false
	}
	// Ensure the extension has a leading dot for matching.
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	for name, cfg := range m.cfg.Servers {
		for _, e := range cfg.Extensions {
			ee := strings.ToLower(e)
			if !strings.HasPrefix(ee, ".") {
				ee = "." + ee
			}
			if ee == ext {
				return name, cfg, true
			}
		}
	}
	return "", ServerConfig{}, false
}

// NotifyChange sends a didChange notification for a file to the appropriate
// LSP server. It implements the FileChangeNotifier interface expected by
// the tools package.
func (m *Manager) NotifyChange(ctx context.Context, path string) error {
	client, err := m.For(ctx, path)
	if err != nil {
		return err
	}
	if client == nil {
		return nil // no matching server
	}
	return client.DidChange(ctx, path, "") // server reads from disk
}

// Shutdown stops all running LSP servers.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return
	}
	m.closed = true

	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for name, client := range m.clients {
		slog.Debug("lsp: shutting down server", "name", name)
		if err := client.Shutdown(shutdownCtx); err != nil {
			slog.Warn("lsp: shutdown error", "name", name, "error", err)
		}
	}
	m.clients = nil
}

// checkServerInstalled verifies that the server command exists on PATH.
