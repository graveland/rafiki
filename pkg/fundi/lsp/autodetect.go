package lsp

import "os/exec"

// knownServers lists well-known language servers and their canonical file
// extensions. Each server self-identifies via the LSP initialize handshake, so
// the extension list is for routing requests to the right server (serverFor),
// not for correctness — a server asked about an unsupported file returns an
// error rather than silently misbehaving.
//
// A server's map key doubles as its human-readable name in logs.
var knownServers = []struct {
	Command    string
	Extensions []string
}{
	{Command: "gopls", Extensions: []string{".go", ".mod"}},
	{Command: "rust-analyzer", Extensions: []string{".rs"}},
	{Command: "typescript-language-server", Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}},
	{Command: "pyright", Extensions: []string{".py", ".pyi"}},
	{Command: "lua-language-server", Extensions: []string{".lua"}},
}

// AutoDetect returns a Config populated with any well-known language servers
// found on PATH. It is the fallback when no lsp.json is configured.
//
// A nil or empty Servers map means nothing was found, and callers should skip
// LSP init exactly as they would for an empty config file.
func AutoDetect() Config {
	cfg := Config{Servers: make(map[string]ServerConfig)}
	for _, s := range knownServers {
		if _, err := exec.LookPath(s.Command); err == nil {
			cfg.Servers[s.Command] = ServerConfig{
				Command:    s.Command,
				Extensions: s.Extensions,
			}
		}
	}
	return cfg
}
