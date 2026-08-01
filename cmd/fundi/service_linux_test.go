//go:build linux

package main

import (
	"strings"
	"testing"
)

// testSpec returns a minimal serviceSpec for template rendering tests.
func testSpec() serviceSpec {
	return serviceSpec{
		DaemonBinary: "/usr/local/bin/fundi",
		HomeEnv:      "/home/testuser",
		LogPath:      "/home/testuser/.local/state/fundi/controller.log",
		PathEnv:      "/usr/local/bin:/usr/bin:/bin",
	}
}

func TestRenderUnit_ContainsSpecFields(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	for _, want := range []string{
		"/usr/local/bin/fundi",
		"/home/testuser",
		"/usr/local/bin:/usr/bin:/bin",
		"controller.log",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("unit file missing %q\ngot:\n%s", want, content)
		}
	}
}

func TestRenderUnit_Format(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	checks := []string{
		"[Unit]",
		"[Service]",
		"[Install]",
		"Description=fundi daemon",
		"After=default.target",
		"Restart=on-failure",
		"WantedBy=default.target",
		"Environment=HOME=",
		"Environment=PATH=",
	}
	for _, c := range checks {
		if !strings.Contains(content, c) {
			t.Errorf("unit file missing %q", c)
		}
	}
}

func TestRenderUnit_IncludesCapturedEnv(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"FUNDI_AGENT_DB": "postgres://postgres@localhost:5432/rafiki?sslmode=disable",
	}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	want := "Environment=FUNDI_AGENT_DB=postgres://postgres@localhost:5432/rafiki?sslmode=disable"
	if !strings.Contains(out, want) {
		t.Errorf("unit missing %q\n---\n%s", want, out)
	}
}

// systemd splits an unquoted Environment= assignment on whitespace, so a value
// containing a space would truncate at the first one and the remainder would be
// parsed as a second malformed assignment.
func TestRenderUnit_QuotesValuesWithSpaces(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{"FUNDI_INSTRUCTIONS": "/home/u/My Docs/instructions.md"}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	want := `Environment="FUNDI_INSTRUCTIONS=/home/u/My Docs/instructions.md"`
	if !strings.Contains(out, want) {
		t.Errorf("unit missing quoted assignment %q\n---\n%s", want, out)
	}
}

func TestUnitQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"K=simple", "K=simple"},
		{"K=with space", `"K=with space"`},
		{`K=has"quote`, `"K=has\"quote"`},
		{`K=has\backslash`, `"K=has\\backslash"`},
	} {
		if got := unitQuote(tc.in); got != tc.want {
			t.Errorf("unitQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderUnit_Deterministic(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{"FUNDI_AGENT_DB": "db", "FUNDI_SOCKET": "/s", "FUNDI_PI_BINARY": "/pi"}
	first, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	for range 20 {
		again, err := renderServiceConfig(spec)
		if err != nil {
			t.Fatalf("renderServiceConfig: %v", err)
		}
		if again != first {
			t.Fatal("unit rendering is not deterministic")
		}
	}
}
