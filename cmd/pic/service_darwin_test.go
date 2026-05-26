//go:build darwin

package main

import (
	"strings"
	"testing"
)

// testSpec returns a minimal serviceSpec for template rendering tests.
func testSpec() serviceSpec {
	return serviceSpec{
		DaemonBinary: "/usr/local/bin/pi-controller",
		HomeEnv:      "/Users/testuser",
		PathEnv:      "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
	}
}

func TestRenderPlist_ContainsSpecFields(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	for _, want := range []string{
		"/usr/local/bin/pi-controller",
		"/Users/testuser",
		"/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"controller.log",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plist missing %q\ngot:\n%s", want, content)
		}
	}
}

func TestRenderPlist_Format(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	checks := []string{
		"<?xml version=\"1.0\"",
		"<plist version=\"1.0\">",
		"dev.graveland.pi-controller",
		"<key>Label</key>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
		"<key>EnvironmentVariables</key>",
		"<key>HOME</key>",
		"<key>PATH</key>",
	}
	for _, c := range checks {
		if !strings.Contains(content, c) {
			t.Errorf("plist missing %q", c)
		}
	}
}
