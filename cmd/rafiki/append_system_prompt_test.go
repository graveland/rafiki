// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
)

func TestLoadProfileAppendSystemPrompt_MissingFileIsEmptyNoError(t *testing.T) {
	isolateProfiles(t)

	got, err := loadProfileAppendSystemPrompt("nonexistent")
	if err != nil {
		t.Fatalf("loadProfileAppendSystemPrompt: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestLoadProfileAppendSystemPrompt_ReadsAndTrimsTrailingNewline(t *testing.T) {
	isolateProfiles(t)

	path := profile.AppendSystemPromptFile("work")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("be terse.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadProfileAppendSystemPrompt("work")
	if err != nil {
		t.Fatalf("loadProfileAppendSystemPrompt: %v", err)
	}
	if got != "be terse." {
		t.Errorf("got %q, want %q", got, "be terse.")
	}
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
			if got := mergeAppendSystemPrompt(tc.file, tc.flag); got != tc.wantText {
				t.Errorf("mergeAppendSystemPrompt(%q, %q) = %q, want %q", tc.file, tc.flag, got, tc.wantText)
			}
		})
	}
}

// TestBuildSpawnRequest_AppendSystemPrompt_MergesProfileFileAndFlag exercises
// the wiring in buildSpawnRequest end to end: the profile's
// append-system-prompt.md and the --append-system-prompt flag both land in
// req.AppendSystemPrompt, file first, for any --kind (this test uses the
// default kind, fundi, since the merge itself is kind-agnostic).
func TestBuildSpawnRequest_AppendSystemPrompt_MergesProfileFileAndFlag(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: "/tmp/rafiki-test.sock"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("work"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	path := profile.AppendSystemPromptFile("work")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("standing default\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/explicit/path"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("append-system-prompt", "be terse"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("buildSpawnRequest: %v", err)
	}
	want := "standing default\n\nbe terse"
	if req.AppendSystemPrompt != want {
		t.Errorf("AppendSystemPrompt = %q, want %q", req.AppendSystemPrompt, want)
	}
}
