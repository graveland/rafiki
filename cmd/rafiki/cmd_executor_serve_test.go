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
