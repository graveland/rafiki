package paths_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

func TestIsReservedEnvKey(t *testing.T) {
	reserved := []string{
		"RAFIKI_DB",    // a DSN, credentials included
		"RAFIKI_TOKEN", // the control-plane bearer
		"RAFIKI_EXECUTOR_SELECTOR",
		"FUNDI_MODEL",          // prior spelling
		"PI_CONTROLLER_SOCKET", // prior spelling
		"ANTHROPIC_API_KEY",
		"OPENROUTER_API_KEY",
	}
	for _, k := range reserved {
		if !paths.IsReservedEnvKey(k) {
			t.Errorf("IsReservedEnvKey(%q) = false, want true", k)
		}
	}

	// A blacklist of what rafiki owns, NOT a general secret filter. Everything
	// an MCP server or a spawned child legitimately needs must survive.
	allowed := []string{
		"PATH", "HOME", "LANG", "TMPDIR",
		"HTTPS_PROXY", "NO_PROXY",
		"GITHUB_TOKEN",          // the operator's, not ours
		"AWS_SECRET_ACCESS_KEY", // likewise
		"RAFIKIESQUE",           // prefix match must require the underscore
		"MY_RAFIKI_THING",       // prefix, not substring
	}
	for _, k := range allowed {
		if paths.IsReservedEnvKey(k) {
			t.Errorf("IsReservedEnvKey(%q) = true, want false", k)
		}
	}
}
