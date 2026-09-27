package tools

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)

	got := envMap(mcpServerEnv(nil))

	for _, k := range []string{"RAFIKI_DB", "RAFIKI_TOKEN", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		v, ok := got[k]
		c.False(ok, "%s reached the MCP server with value %q", k, v)
	}
	c.Eq("/usr/bin", got["PATH"], "PATH")
}

// The original bug's exact shape: cmd.Env was only set when the config happened
// to name a variable, so a server with no Env inherited everything. The filter
// must not depend on the config being non-empty.
func TestMCPServerEnvFiltersEvenWithNoConfiguredEnv(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://user:pw@host/db")

	if _, leaked := envMap(mcpServerEnv(nil))["RAFIKI_DB"]; leaked {
		t.Error("RAFIKI_DB leaked when the server config set no env of its own")
	}
	_, leaked := envMap(mcpServerEnv(map[string]string{}))["RAFIKI_DB"]
	assert.NewCollecting(t).False(leaked, "RAFIKI_DB leaked for an empty (non-nil) env map")
}

// The server's own configured credentials must still arrive.
func TestMCPServerEnvCarriesConfiguredValues(t *testing.T) {
	got := envMap(mcpServerEnv(map[string]string{"GITHUB_TOKEN": "ghp_x"}))
	assert.NewCollecting(t).Eq("ghp_x", got["GITHUB_TOKEN"], "GITHUB_TOKEN")
}

// A configured value wins over the daemon's own, so an operator can point one
// server at a different account without changing the daemon's environment.
func TestMCPServerEnvConfiguredValueWins(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_daemon")

	kv := mcpServerEnv(map[string]string{"GITHUB_TOKEN": "ghp_configured"})
	assert.NewCollecting(t).Eq("ghp_configured", envMap(kv)["GITHUB_TOKEN"], "GITHUB_TOKEN")
}
