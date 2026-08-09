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
	// restarts counts UNHEALTHY spawns per language so a server that crashes
	// on startup cannot become an unbounded spawn loop. It must not count
	// every spawn: For used to increment it on every start regardless of
	// cause, so the initial start plus four deliberate lsp_restart calls
	// exhausted maxServerStarts and every LSP tool failed for the rest of
	// the process with a message ("failed to stay running") that was a lie
	// — the server had never failed on its own. It is forgiven in two ways,
	// both applied under mu in For: an exit after healthyUptime resets it
	// (see the eviction block), and an operator-initiated restart consumes
	// its one free respawn via forgiveRestart instead of incrementing it.
	restarts map[string]int
	// forgiveRestart marks a language whose NEXT spawn must not count
	// against restarts, set by Restart (the lsp_restart tool) before it
	// shuts the current client down. It is consumed (deleted) the moment
	// that spawn is attempted in For, so it exempts exactly the one respawn
	// the deliberate restart caused — nothing more.
	forgiveRestart map[string]bool
	mu             sync.Mutex
	closed         bool

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

// maxServerStarts bounds UNHEALTHY spawns per language for the manager's
// lifetime — see the restarts field comment for what counts.
const maxServerStarts = 5

// healthyUptime is how long a server must stay up before its eventual exit
// stops counting as a "failed to stay running" strike: it resets restarts
// for that language to zero. A process that has been serving requests this
// long has proven it starts and works, so a crash after that point is an
// ordinary flake, not evidence the server itself is broken — and without
// this a long-lived server that happens to crash once an hour would still
// eventually exhaust maxServerStarts and lock LSP out for the rest of the
// session. Set comfortably above initializeTimeout (30s): indexing a large
// module can itself take close to that long, and this must never mistake a
// slow-but-legitimate startup for a startup crash.
//
// A var, not a const, so tests can shrink it rather than sleep 2 real
// minutes to prove the forgiveness path fires.
var healthyUptime = 2 * time.Minute

// NewManager creates a Manager from the given configuration and working directory.
func NewManager(cfg Config, cwd string) *Manager {
	procCtx, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg:            cfg,
		cwd:            cwd,
		clients:        make(map[string]*Client),
		starting:       make(map[string]*startInFlight),
		restarts:       make(map[string]int),
		forgiveRestart: make(map[string]bool),
		procCtx:        procCtx,
		cancelProc:     cancel,
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
			// A server that ran long enough to prove itself healthy should
			// not have this exit — for whatever reason — count against
			// future starts. See healthyUptime.
			if uptime := client.Uptime(); uptime >= healthyUptime {
				slog.Info("lsp: server had a healthy run before exiting, forgiving restart budget",
					"name", name, "uptime", uptime)
				m.restarts[name] = 0
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

		forgiven := m.forgiveRestart[name]
		if !forgiven && m.restarts[name] >= maxServerStarts {
			m.mu.Unlock()
			return nil, fmt.Errorf(
				"lsp: %s has failed to stay running after %d starts; not restarting it again",
				name, maxServerStarts)
		}
		if forgiven {
			delete(m.forgiveRestart, name)
		} else {
			m.restarts[name]++
		}
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

// Restart forces the language server responsible for path to respawn, for
// the lsp_restart tool. It must NOT consume the restarts budget in For:
// that budget exists to stop an unbounded loop of a server that fails on
// its own, and an operator asking for a restart says nothing about the
// server's health. This is what Finding 5 was — every spawn, deliberate or
// not, used to increment restarts, so the initial start plus four
// lsp_restart calls exhausted maxServerStarts and every LSP tool failed
// for the rest of the process.
//
// If no server is currently running for path, this is equivalent to a
// plain lazy start via For: there is no existing process to discard, and a
// fresh start is not a "restart" the budget needs to forgive anything for.
func (m *Manager) Restart(ctx context.Context, path string) error {
	name, _, ok := m.serverFor(path)
	if !ok {
		return fmt.Errorf("lsp: no language server for %s", path)
	}

	m.mu.Lock()
	client := m.clients[name]
	if client != nil {
		// Consumed by For the moment it attempts the respawn this shutdown
		// causes, so it exempts exactly this one restart.
		m.forgiveRestart[name] = true
	}
	m.mu.Unlock()

	if client == nil {
		_, err := m.For(ctx, path)
		return err
	}

	// For lazily respawns on its next call for this name; forgiveRestart
	// above ensures that respawn is free.
	return client.Shutdown(ctx)
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
