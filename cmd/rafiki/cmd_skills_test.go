// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestSplitQualified(t *testing.T) {
	for _, tc := range []struct{ in, ns, name string }{
		{"rafiki:coordinating", "rafiki", "coordinating"},
		{"pg:design-postgres-tables", "pg", "design-postgres-tables"},
		{"bare", "rafiki", "bare"},
	} {
		ns, name := splitQualified(tc.in)
		assert.NewCollecting(t).False(ns != tc.ns || name != tc.name, "%q: got (%q,%q), want (%q,%q)", tc.in, ns, name, tc.ns, tc.name)
	}
}

// A skill with supporting files names them after its body: mode, size, path —
// an inventory, not the content. Without files there is no section at all, so
// the common case's output is unchanged.
func TestSkillsShowPrintsFiles(t *testing.T) {
	c := assert.NewCollecting(t)

	var buf bytes.Buffer
	row := &rafikiv1.SkillRow{
		Body: "the body",
		Files: []*rafikiv1.SkillFileInfo{
			{Path: "reference/a.md", Mode: 0o644, Size: 12},
			{Path: "scripts/run.sh", Mode: 0o755, Size: 34},
		},
	}
	c.Require().NoError(renderSkillShow(&buf, row), "renderSkillShow")
	c.Eq("the body\nfiles:\n0644 12 reference/a.md\n0755 34 scripts/run.sh\n", buf.String(), "show output")

	buf.Reset()
	c.Require().NoError(renderSkillShow(&buf, &rafikiv1.SkillRow{Body: "plain"}), "renderSkillShow without files")
	c.Eq("plain\n", buf.String(), "a skill without files prints only its body")
	c.NotStrContains(buf.String(), "files:", "an empty file list printed a files: header")
}

// The inventory says which layer a row belongs to and whether a higher layer
// overrides it — the two facts a plain name list could not show.
func TestSkillsListRendersLayerAndShadowed(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	rows := []*rafikiv1.SkillRow{{
		Namespace: "rafiki", Name: "x", Layer: "core", Source: "rafiki-core",
		Enabled: true, Shadowed: true, Description: "a core skill",
	}}
	c.Require().NoError(renderSkillsTable(&buf, rows, false), "renderSkillsTable")
	out := buf.String()
	for _, want := range []string{"NAME", "LAYER", "SOURCE", "ENABLED", "SHADOWED", "DESCRIPTION",
		"rafiki:x", "core", "rafiki-core", "true", "a core skill"} {
		c.StrContains(out, want, "inventory output missing %q", want)
	}
	c.NotStrContains(out, "\x1b", "useColor=false must emit no ANSI escapes:\n%s", out)
}
