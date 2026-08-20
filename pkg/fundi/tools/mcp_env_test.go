package tools

import (
	"strings"
	"testing"
)

func envMap(kv []string) map[string]string {
	out := make(map[string]string, len(kv))
	for _, e := range kv {
		if i := strings.IndexByte(e, '='); i > 0 {
			out[e[:i]] = e[i+1:]
		}
	}
	return out
}

// An MCP server is a third-party program named in a config file. Handing it the
// daemon's whole environment gave it RAFIKI_DB — a connection string with
// credentials — plus RAFIKI_TOKEN and both provider API keys, for a package
// added to get one tool.
func TestMCPServerEnvStripsWhatRafikiOwns(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://user:pw@host/db")
	t.Setenv("RAFIKI_TOKEN", "rt_secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-secret")
	t.Setenv("PATH", "/usr/bin")

	got := envMap(mcpServerEnv(nil))

	for _, k := range []string{"RAFIKI_DB", "RAFIKI_TOKEN", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s reached the MCP server with value %q", k, v)
		}
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("PATH = %q, want it passed through — an MCP server needs it to exec anything", got["PATH"])
	}
}

// The original bug's exact shape: cmd.Env was only set when the config happened
// to name a variable, so a server with no Env inherited everything. The filter
// must not depend on the config being non-empty.
func TestMCPServerEnvFiltersEvenWithNoConfiguredEnv(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://user:pw@host/db")

	if _, leaked := envMap(mcpServerEnv(nil))["RAFIKI_DB"]; leaked {
		t.Error("RAFIKI_DB leaked when the server config set no env of its own")
	}
	if _, leaked := envMap(mcpServerEnv(map[string]string{}))["RAFIKI_DB"]; leaked {
		t.Error("RAFIKI_DB leaked for an empty (non-nil) env map")
	}
}

// The server's own configured credentials must still arrive.
func TestMCPServerEnvCarriesConfiguredValues(t *testing.T) {
	got := envMap(mcpServerEnv(map[string]string{"GITHUB_TOKEN": "ghp_x"}))
	if got["GITHUB_TOKEN"] != "ghp_x" {
		t.Errorf("GITHUB_TOKEN = %q, want the configured value", got["GITHUB_TOKEN"])
	}
}

// A configured value wins over the daemon's own, so an operator can point one
// server at a different account without changing the daemon's environment.
func TestMCPServerEnvConfiguredValueWins(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_daemon")

	kv := mcpServerEnv(map[string]string{"GITHUB_TOKEN": "ghp_configured"})
	if envMap(kv)["GITHUB_TOKEN"] != "ghp_configured" {
		t.Errorf("GITHUB_TOKEN = %q, want the configured value to win", envMap(kv)["GITHUB_TOKEN"])
	}
}
