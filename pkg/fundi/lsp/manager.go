package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	// starting holds an in-flight start per language so concurrent callers
	// await one spawn instead of racing. Without it, a first turn issuing
	// lsp_diagnostics + lsp_definition + lsp_references in one batch started
	// THREE gopls processes indexing the same module simultaneously and then
	// threw two away — hundreds of MB and a lot of CPU for nothing.
	starting map[string]*startInFlight
	// restarts counts spawns per language so a server that crashes on
	// startup cannot become an unbounded spawn loop.
	restarts map[string]int
	mu       sync.Mutex
	closed   bool

	// procCtx governs SERVER PROCESS lifetime and is deliberately not any
	// caller's context: exec.CommandContext kills the process when its ctx
	// is done, so passing a tool call's ctx killed gopls at the end of the
	// turn that happened to start it.
	procCtx    context.Context
	cancelProc context.CancelFunc
}

// startInFlight lets concurrent For callers wait on a single spawn.
type startInFlight struct {
	done   chan struct{}
	client *Client
	err    error
}

// maxServerStarts bounds spawns per language for the manager's lifetime.
const maxServerStarts = 5

// NewManager creates a Manager from the given configuration and working directory.
func NewManager(cfg Config, cwd string) *Manager {
	procCtx, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg:        cfg,
		cwd:        cwd,
		clients:    make(map[string]*Client),
		starting:   make(map[string]*startInFlight),
		restarts:   make(map[string]int),
		procCtx:    procCtx,
		cancelProc: cancel,
	}
}

// Cwd returns the working directory the manager is rooted at.
func (m *Manager) Cwd() string { return m.cwd }

// FirstClient returns the first configured client, or nil.
// Useful for workspace-level requests that don't target a specific file.
func (m *Manager) FirstClient(ctx context.Context) (*Client, error) {
	// Find the first configured extension.
	for _, cfg := range m.cfg.Servers {
		if len(cfg.Extensions) > 0 {
			ext := cfg.Extensions[0]
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			return m.For(ctx, filepath.Join(m.cwd, "_ws"+ext))
		}
	}
	return nil, nil
}

// For returns the Client responsible for the file at the given path.
// It lazily starts the appropriate LSP server if one matches and hasn't
// been started yet. Returns nil, nil if no configured server matches.
func (m *Manager) For(ctx context.Context, path string) (*Client, error) {
	name, cfg, ok := m.serverFor(path)
	if !ok {
		return nil, nil
	}

	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, fmt.Errorf("lsp: manager is shut down")
		}

		// Evict a dead client. Dead() is an atomic set by the client's own
		// single waiter goroutine; the previous check peeked at
		// cmd.ProcessState, which is written by cmd.Wait() on another
		// goroutine (a data race) and stays nil forever when nothing calls
		// Wait — so a crashed server was never actually noticed and every
		// later call returned "connection is closed".
		if client, ok := m.clients[name]; ok {
			if !client.Dead() {
				m.mu.Unlock()
				return client, nil
			}
			slog.Info("lsp: server exited, will restart", "name", name)
			delete(m.clients, name)
		}

		// Someone else is already starting this server: wait for them.
		if inflight, ok := m.starting[name]; ok {
			m.mu.Unlock()
			select {
			case <-inflight.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if inflight.err != nil {
				return nil, inflight.err
			}
			if inflight.client != nil && !inflight.client.Dead() {
				return inflight.client, nil
			}
			continue // it died immediately; re-evaluate under the lock
		}

		if m.restarts[name] >= maxServerStarts {
			m.mu.Unlock()
			return nil, fmt.Errorf(
				"lsp: %s has failed to stay running after %d starts; not restarting it again",
				name, maxServerStarts)
		}
		m.restarts[name]++
		inflight := &startInFlight{done: make(chan struct{})}
		m.starting[name] = inflight
		m.mu.Unlock()

		client, err := m.start(name, cfg)

		m.mu.Lock()
		delete(m.starting, name)
		if err == nil && !m.closed {
			m.clients[name] = client
		}
		closed := m.closed
		m.mu.Unlock()

		inflight.client, inflight.err = client, err
		close(inflight.done)

		if err != nil {
			return nil, err
		}
		if closed {
			_ = client.Shutdown(ctx)
			return nil, fmt.Errorf("lsp: manager is shut down")
		}
		return client, nil
	}
}

// start spawns and initializes one server. The process is tied to procCtx,
// never to a caller's context.
func (m *Manager) start(name string, cfg ServerConfig) (*Client, error) {
	client, err := NewClient(m.procCtx, ClientConfig{
		Name:    name,
		Command: cfg.Command,
		Args:    cfg.Args,
		Cwd:     m.cwd,
	})
	if err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", name, err)
	}
	// Initialize is bounded by the client's own initializeTimeout rather
	// than a caller's ctx, so a turn ending mid-handshake does not leave a
	// half-initialized server in the map.
	if err := client.Initialize(m.procCtx, m.cwd); err != nil {
		_ = client.Shutdown(context.Background())
		return nil, fmt.Errorf("lsp: initialize %s: %w", name, err)
	}
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
// The content is read from disk and sent in full. The previous version sent
// an empty string with the comment "server reads from disk" — it does not:
// this is full-text sync, so contentChanges [{"text": ""}] tells the server
// the document is now EMPTY. Verified against gopls, which responded with
// "expected ';', found 'EOF'" and then answered every position query with
// "line number N out of range 0-1". The whole file vanished from its view.
func (m *Manager) NotifyChange(ctx context.Context, path string) error {
	client, err := m.For(ctx, path)
	if err != nil {
		return err
	}
	if client == nil {
		return nil // no matching server
	}
	content, err := os.ReadFile(path)
	if err != nil {
		// A file that was deleted, or is unreadable, is not worth failing
		// the caller's write over — the tool already succeeded.
		slog.Debug("lsp: skipping change notification", "path", path, "err", err)
		return nil
	}
	return client.DidChange(ctx, path, string(content))
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

	// Backstop: kill anything the graceful path missed, including a server
	// still mid-handshake in a concurrent start.
	m.cancelProc()
}

// HasInstalledServer reports whether at least one configured server binary
// resolves on PATH.
//
// Without this the eight LSP tools materialized whenever an lsp.json merely
// existed, so a config naming a server that is not installed — or an empty
// {"servers":{}} — put eight tools in tools[] that could only ever fail with
// `exec: "gopls": executable file not found in $PATH`, burning a turn each
// time the model reached for one.
func (m *Manager) HasInstalledServer() bool {
	for _, cfg := range m.cfg.Servers {
		if cfg.Command == "" {
			continue
		}
		if _, err := exec.LookPath(cfg.Command); err == nil {
			return true
		}
	}
	return false
}
