//go:build darwin

package main

import (
	"encoding/xml"
	"strings"
	"testing"
)

// testSpec returns a minimal serviceSpec for template rendering tests.
func testSpec() serviceSpec {
	return serviceSpec{
		DaemonBinary: "/usr/local/bin/rafikid",
		HomeEnv:      "/Users/testuser",
		LogPath:      "/Users/testuser/.local/state/rafiki/controller.log",
		PathEnv:      "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
	}
}

func TestRenderPlist_ContainsSpecFields(t *testing.T) {
	content, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}

	for _, want := range []string{
		"/usr/local/bin/rafikid",
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
		"dev.graveland.rafiki",
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

func TestRenderPlist_IncludesCapturedEnv(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"RAFIKI_DB":     "postgres://postgres@localhost:5432/rafiki?sslmode=disable",
		"RAFIKI_SOCKET": "/tmp/rafiki.sock",
	}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	for _, want := range []string{
		"<key>RAFIKI_DB</key>",
		"<string>postgres://postgres@localhost:5432/rafiki?sslmode=disable</string>",
		"<key>RAFIKI_SOCKET</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q\n---\n%s", want, out)
		}
	}
}

// A DSN routinely carries a query string, so an unescaped ampersand would
// produce a plist launchd refuses to parse — the service would become
// uninstallable over the very variable this mechanism exists to carry.
func TestRenderPlist_EscapesXML(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"RAFIKI_DB": "postgres://h/db?sslmode=disable&application_name=rafiki<1>",
	}
	out, err := renderServiceConfig(spec)
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	if strings.Contains(out, "&application_name") {
		t.Error("raw ampersand emitted; the plist would not parse")
	}
	if !strings.Contains(out, "&amp;application_name") {
		t.Errorf("ampersand not escaped\n---\n%s", out)
	}
	if strings.Contains(out, "rafiki<1>") {
		t.Error("raw angle brackets emitted")
	}
	// The real check: it must actually parse as XML.
	if err := xml.Unmarshal([]byte(out), new(any)); err != nil {
		t.Errorf("rendered plist is not well-formed XML: %v\n---\n%s", err, out)
	}
}

// Reinstalling with an unchanged environment must produce a byte-identical
// plist; a map ranged directly would reorder keys at random and make every
// reinstall look like a change.
func TestRenderPlist_Deterministic(t *testing.T) {
	spec := testSpec()
	spec.ExtraEnv = map[string]string{
		"RAFIKI_DB": "db", "RAFIKI_SOCKET": "/s", "RAFIKI_PI_BINARY": "/pi",
		"RAFIKI_DEFAULT_MODEL": "m", "RAFIKI_MCP_CONFIG": "/c",
	}
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
			t.Fatal("plist rendering is not deterministic")
		}
	}
}

// The plist is 0644. Whatever else changes, a DSN must never appear in it.
func TestRenderServiceConfig_NeverContainsADSN(t *testing.T) {
	unit, secret, _ := captureDaemonEnv([]string{
		"RAFIKI_DB=postgres://u:hunter2@localhost/rafiki",
		"RAFIKI_DEFAULT_MODEL=anthropic/opus-latest",
	})
	out, err := renderServiceConfig(serviceSpec{
		DaemonBinary: "/usr/local/bin/rafikid",
		PathEnv:      "/usr/bin",
		HomeEnv:      "/Users/test",
		LogPath:      "/tmp/rafiki.log",
		ExtraEnv:     unit,
		SecretEnv:    secret,
	})
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	for _, forbidden := range []string{"hunter2", "postgres://", "RAFIKI_DB"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("rendered plist contains %q:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "RAFIKI_DEFAULT_MODEL") {
		t.Error("a non-secret variable stopped being baked into the plist")
	}
}

// No captured environment must still render a valid plist — the pre-existing
// HOME/PATH-only shape.
func TestRenderPlist_EmptyExtraEnv(t *testing.T) {
	out, err := renderServiceConfig(testSpec())
	if err != nil {
		t.Fatalf("renderServiceConfig: %v", err)
	}
	if strings.Contains(out, "RAFIKI_") {
		t.Errorf("unexpected RAFIKI_ key with no captured env\n---\n%s", out)
	}
	if err := xml.Unmarshal([]byte(out), new(any)); err != nil {
		t.Errorf("not well-formed XML: %v", err)
	}
}
