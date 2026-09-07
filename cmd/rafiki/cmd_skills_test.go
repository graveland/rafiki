// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
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
	got, err := derivePluginNamespace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "mycorpus" {
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
	got, err := derivePluginNamespace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "pg" {
		t.Errorf("got %q, want %q — the PLUGIN name, not the marketplace name", got, "pg")
	}
}

// `rafiki skills import .` used to fall back to a namespace literally named
// "." and upsert the whole corpus into it — rows that render as ".:skill",
// deduplicate against nothing, and can only be cleaned out with rm. The
// fallback must refuse the two spellings that cannot name a directory.
func TestDerivePluginNamespaceRefusesDotAndDotDot(t *testing.T) {
	isolateProfiles(t)
	for _, dir := range []string{".", ".."} {
		if _, err := derivePluginNamespace(dir); err == nil {
			t.Errorf("derivePluginNamespace(%q) = nil error, want a refusal naming --namespace", dir)
		}
	}

	// The CLI must refuse before it talks to the daemon or walks the tree.
	cmd := newSkillsImportCmd()
	cmd.SetArgs([]string{"."})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--namespace") {
		t.Fatalf("skills import . = %v, want an error naming --namespace", err)
	}

	// --namespace is the documented escape hatch and still works.
	cmd = newSkillsImportCmd()
	cmd.SetArgs([]string{"--namespace", "corp", "."})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	// No skills under "." for a corpus-shaped import to find; the namespace
	// error must NOT be the failure — this only pins that --namespace bypasses
	// derivation, so the expected error is about skills, not about a namespace.
	if err := cmd.Execute(); err == nil || strings.Contains(err.Error(), "cannot derive a namespace") {
		t.Fatalf("--namespace . : err = %v, want derivation bypassed (any other outcome)", err)
	}
}
