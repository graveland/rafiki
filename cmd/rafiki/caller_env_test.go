package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// The caller's environment travels to the daemon in a SpawnRequest, so anything
// rafiki owns must be stripped before it can override what the daemon injects
// per-child — and the caller's provider keys must not displace the daemon's.
func TestCollectCallerEnvStripsReservedKeys(t *testing.T) {
	t.Setenv(paths.URL, "https://example.dev")
	t.Setenv("FUNDI_MODEL", "stale")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-caller")
	t.Setenv("PATH", "/usr/bin")

	env := collectCallerEnv()

	for _, k := range []string{paths.URL, "FUNDI_MODEL", "ANTHROPIC_API_KEY"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s reached the SpawnRequest with value %q", k, v)
		}
	}
	if env["PATH"] != "/usr/bin" {
		t.Errorf("PATH = %q, want it forwarded", env["PATH"])
	}
}
