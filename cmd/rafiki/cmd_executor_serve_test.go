// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
)

func TestExecutorProfileProxyNoManifestDoesNotBootstrap(t *testing.T) {
	isolateProfiles(t)
	cmd := newRootCmd()

	if got := executorProfileProxy(cmd); got != "" {
		t.Fatalf("executorProfileProxy = %q, want empty with no manifest", got)
	}
	if _, err := profile.Load(); err == nil {
		t.Fatal("executorProfileProxy must not create a profiles.toml as a side effect")
	}
}

func TestExecutorProfileProxyResolvesTheSelectedProfile(t *testing.T) {
	isolateProfiles(t)
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", URL: "https://rafiki.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("work"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
	cmd := newRootCmd()

	got := executorProfileProxy(cmd)
	if want := "https://rafiki.example.net"; got != want {
		t.Fatalf("executorProfileProxy = %q, want %q (derived from the profile's url)", got, want)
	}
}

func TestExecutorProfileProxyUnknownProfileNameDegradesToEmpty(t *testing.T) {
	isolateProfiles(t)
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", URL: "https://rafiki.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cmd := newRootCmd()
	if err := cmd.PersistentFlags().Set("profile", "does-not-exist"); err != nil {
		t.Fatalf("Set profile flag: %v", err)
	}

	if got := executorProfileProxy(cmd); got != "" {
		t.Fatalf("executorProfileProxy = %q, want empty for an unknown profile name", got)
	}
}

func TestExecutorProfileProxySocketOnlyProfileIsEmpty(t *testing.T) {
	isolateProfiles(t)
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"local": {Name: "local", Socket: filepath.Join(t.TempDir(), "d.sock")},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("local"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
	cmd := newRootCmd()

	if got := executorProfileProxy(cmd); got != "" {
		t.Fatalf("executorProfileProxy = %q, want empty for a socket-only profile", got)
	}
}

func TestExecutorProfileProxyExplicitFlagWithNoManifestStillAttemptsResolution(t *testing.T) {
	isolateProfiles(t)
	cmd := newRootCmd()
	if err := cmd.PersistentFlags().Set("profile", "work"); err != nil {
		t.Fatalf("Set profile flag: %v", err)
	}

	if got := executorProfileProxy(cmd); got != "" {
		t.Fatalf("executorProfileProxy = %q, want empty (no manifest at all, even for an explicit -P)", got)
	}
	if _, err := profile.Load(); err == nil {
		t.Fatal("an explicit -P on a machine with no manifest must still never bootstrap one — profile.Resolve refuses to bootstrap a requested name on its own")
	}
}

// skillsSyncFromArgv drives the same path RunE uses — parse the flags, read
// them back, resolve them against the environment — for the combinations that
// matter: flag off and env unset (the default), flag off and env set, an
// explicit --skills-sync=false over a set env, the bare flag, and the
// --launch claude implication with and without an explicit refusal.
func skillsSyncFromArgv(t *testing.T, argv []string, env string) bool {
	t.Helper()
	t.Setenv("RAFIKI_EXECUTOR_SKILLS_SYNC", env)
	cmd := newExecutorServeCmd()
	if err := cmd.Flags().Parse(argv); err != nil {
		t.Fatal(err)
	}
	on, err := cmd.Flags().GetBool("skills-sync")
	if err != nil {
		t.Fatal(err)
	}
	launchKinds, err := cmd.Flags().GetStringArray("launch")
	if err != nil {
		t.Fatal(err)
	}
	return skillsSyncEnabled(cmd, on, launchKinds)
}

func TestSkillsSyncDefaultsOff(t *testing.T) {
	if skillsSyncFromArgv(t, nil, "") {
		t.Error("skills-sync defaulted on; it writes into the operator's home directory")
	}
}

func TestSkillsSyncEnvTurnsItOn(t *testing.T) {
	if !skillsSyncFromArgv(t, nil, "1") {
		t.Error("a non-empty RAFIKI_EXECUTOR_SKILLS_SYNC must enable skills sync — a service unit cannot edit argv")
	}
}

func TestSkillsSyncExplicitFalseBeatsTheEnvironment(t *testing.T) {
	if skillsSyncFromArgv(t, []string{"--skills-sync=false"}, "1") {
		t.Error("an explicit --skills-sync=false must stay able to switch the feature off on a machine whose environment enables it")
	}
}

func TestSkillsSyncFlagTurnsItOn(t *testing.T) {
	if !skillsSyncFromArgv(t, []string{"--skills-sync"}, "") {
		t.Error("--skills-sync must enable skills sync")
	}
}

func TestSkillsSyncImpliedByLaunchClaude(t *testing.T) {
	if !skillsSyncFromArgv(t, []string{"--launch", "claude"}, "") {
		t.Error("--launch claude must imply skills sync — a claude host without the " +
			"corpus launches children that silently see no rafiki skills")
	}
}

func TestSkillsSyncExplicitFalseBeatsLaunchClaude(t *testing.T) {
	if skillsSyncFromArgv(t, []string{"--launch", "claude", "--skills-sync=false"}, "") {
		t.Error("an explicit --skills-sync=false must beat the --launch claude implication")
	}
}

func TestSkillsSyncNotImpliedByOtherLaunchKinds(t *testing.T) {
	if skillsSyncFromArgv(t, []string{"--launch", "other"}, "") {
		t.Error("only --launch claude implies skills sync")
	}
}
