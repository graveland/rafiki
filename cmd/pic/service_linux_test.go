//go:build linux

package main

import (
	"strings"
	"testing"
)

// testSpec returns a minimal serviceSpec for template rendering tests.
func testSpec() serviceSpec {
	return serviceSpec{
		DaemonBinary: "/usr/local/bin/pi-controller",
		HomeEnv:      "/home/testuser",
		PathEnv:      "/usr/local/bin:/usr/bin:/bin",
	}
}

func TestRenderUnit_ContainsSpecFields(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	for _, want := range []string{
		"/usr/local/bin/pi-controller",
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
		"Description=pi-controller daemon",
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
