// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
)

func TestSplitQualified(t *testing.T) {
	for _, tc := range []struct{ in, ns, name string }{
		{"rafiki:coordinating", "rafiki", "coordinating"},
		{"pg:design-postgres-tables", "pg", "design-postgres-tables"},
		{"bare", "rafiki", "bare"},
	} {
		ns, name := splitQualified(tc.in)
		if ns != tc.ns || name != tc.name {
			t.Errorf("%q: got (%q,%q), want (%q,%q)", tc.in, ns, name, tc.ns, tc.name)
		}
	}
}

func TestDerivePluginNamespaceFallsBackToTheDirectoryName(t *testing.T) {
	dir := t.TempDir() + "/mycorpus"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := derivePluginNamespace(dir); got != "mycorpus" {
		t.Errorf("got %q, want %q", got, "mycorpus")
	}
}

func TestDerivePluginNamespaceReadsAMarketplaceManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/.claude-plugin", 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"aiguide","plugins":[{"name":"pg","source":"./"}]}`
	if err := os.WriteFile(dir+"/.claude-plugin/marketplace.json", []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := derivePluginNamespace(dir); got != "pg" {
		t.Errorf("got %q, want %q — the PLUGIN name, not the marketplace name", got, "pg")
	}
}
