// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/skills"
)

func TestLoadCoreSkillsParsesTheEmbeddedCorpus(t *testing.T) {
	recs, err := loadCoreSkills()
	if err != nil {
		t.Fatalf("loadCoreSkills: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("no core skills embedded; the corpus under skills/ is empty or unreadable")
	}
	for _, r := range recs {
		if r.Namespace != skills.DefaultNamespace {
			t.Errorf("%s: namespace %q, want %q", r.Name, r.Namespace, skills.DefaultNamespace)
		}
		if r.Source != skills.CoreSource {
			t.Errorf("%s: source %q, want %q", r.Name, r.Source, skills.CoreSource)
		}
		if r.Description == "" {
			t.Errorf("%s: empty description; it is the only thing the model sees in the inventory", r.Name)
		}
		if r.Body == "" {
			t.Errorf("%s: empty body", r.Name)
		}
		// The frontmatter must have been stripped: a body that still opens
		// with the delimiter would put YAML into the model's context.
		if len(r.Body) >= 3 && r.Body[:3] == "---" {
			t.Errorf("%s: body still carries its frontmatter block", r.Name)
		}
	}
}

func TestCoreSkillNamesAreStableSlugs(t *testing.T) {
	recs, err := loadCoreSkills()
	if err != nil {
		t.Fatalf("loadCoreSkills: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range recs {
		if seen[r.Name] {
			t.Errorf("duplicate core skill name %q", r.Name)
		}
		seen[r.Name] = true
		for _, bad := range []string{" ", ":", "/", "\t"} {
			if strings.Contains(r.Name, bad) {
				t.Errorf("core skill name %q contains %q; names are directory names and tool arguments", r.Name, bad)
			}
		}
	}
}
