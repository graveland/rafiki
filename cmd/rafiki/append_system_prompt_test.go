// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

func TestLoadProfileAppendSystemPrompt_MissingFileIsEmptyNoError(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)

	got, err := loadProfileAppendSystemPrompt("nonexistent")
	c.Require().NoError(err, "loadProfileAppendSystemPrompt")
	c.Eq("", got, "got")
}

func TestLoadProfileAppendSystemPrompt_ReadsAndTrimsTrailingNewline(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)

	path := profile.AppendSystemPromptFile("work")
	c.Require().NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	c.Require().NoError(os.WriteFile(path, []byte("be terse.\n"), 0o600))

	got, err := loadProfileAppendSystemPrompt("work")
	c.Require().NoError(err, "loadProfileAppendSystemPrompt")
	c.Eq("be terse.", got, "got")
}

func TestMergeAppendSystemPrompt(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		flag     string
		wantText string
	}{
		{"both empty", "", "", ""},
		{"file only", "from the file", "", "from the file"},
		{"flag only", "", "from the flag", "from the flag"},
		{"both, file first", "from the file", "from the flag", "from the file\n\nfrom the flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeAppendSystemPrompt(tc.file, tc.flag)
			assert.NewCollecting(t).Eq(tc.wantText, got, "mergeAppendSystemPrompt(%q, %q) = %q, want", tc.file, tc.flag, got)
		})
	}
}

// TestBuildSpawnRequest_AppendSystemPrompt_MergesProfileFileAndFlag exercises
// the wiring in buildSpawnRequest end to end: the profile's
// append-system-prompt.md and the --append-system-prompt flag both land in
// req.AppendSystemPrompt, file first, for any --kind (this test uses the
// default kind, fundi, since the merge itself is kind-agnostic).
func TestBuildSpawnRequest_AppendSystemPrompt_MergesProfileFileAndFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	resetProfileCache()
	c.Require().NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: "/tmp/rafiki-test.sock"},
	}}), "Save")
	c.Require().NoError(profile.SavePointer("work"), "SavePointer")

	path := profile.AppendSystemPromptFile("work")
	c.Require().NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	c.Require().NoError(os.WriteFile(path, []byte("standing default\n"), 0o600))

	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/explicit/path"))
	c.Require().NoError(cmd.Flags().Set("append-system-prompt", "be terse"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "buildSpawnRequest")
	want := "standing default\n\nbe terse"
	c.Eq(want, req.AppendSystemPrompt, "AppendSystemPrompt")
}
