// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
)

// seedRemoteProfile isolates profile state for the test and seeds a single
// remote profile pointed at url, with token as its credential (empty means
// no token file is written at all).
func seedRemoteProfile(t *testing.T, name, url, token string) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		name: {Name: name, URL: url},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if token != "" {
		if err := profile.WriteToken(name, token); err != nil {
			t.Fatalf("WriteToken: %v", err)
		}
	}
	if err := profile.SavePointer(name); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

// seedLocalProfile is seedRemoteProfile's socket-based counterpart.
func seedLocalProfile(t *testing.T, name, socket string) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		name: {Name: name, Socket: socket},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer(name); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

// The cache is the only part of this path testable without a live daemon: the
// fetch itself needs a Connect server. What matters here is that a cached
// answer is served without one, and that a drop forces a refetch.
func TestChildCompletionServesFromCache(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	want := []completionChild{{ChildID: "c_1", Name: "alpha", Status: "idle"}}
	cacheWrite("children", completionEndpointKey(nil), want)

	got := completionChildrenCached(nil, childCacheTTL)
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("got %+v, want the cached row", got)
	}
}

func TestDropChildCompletionCacheForcesARefetch(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	cacheWrite("children", completionEndpointKey(nil), []completionChild{{Name: "alpha"}})
	dropChildCompletionCache(nil)

	if got := completionChildrenCached(nil, childCacheTTL); len(got) != 0 {
		t.Errorf("got %+v after a drop, want no cached rows", got)
	}
}

// An unreachable daemon must yield no candidates, never an error or an exit.
func TestChildCompletionOnAnUnreachableDaemonIsEmpty(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://127.0.0.1:1", "t")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	done := make(chan []completionChild, 1)
	go func() { done <- completionChildren(nil) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completion blocked the shell for 5s; it must bound its own deadline")
	}
}

// The completion key follows the resolved PROFILE, not an env var: the old
// bug this task closes was a helper reading only --socket and never
// consulting RAFIKI_URL, so a remote operator got no candidates at all,
// silently. Now the key is the profile's URL when it names a remote, and a
// "unix:"-prefixed socket path when it names a local daemon.
func TestCompletionEndpointKeyFollowsTheProfile(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://daemon.example", "t")
	if got := completionEndpointKey(nil); got != "https://daemon.example" {
		t.Errorf("key = %q, want the remote URL", got)
	}

	seedLocalProfile(t, "work", "/tmp/work-scratch/controller.sock")
	if got := completionEndpointKey(nil); !strings.HasPrefix(got, "unix:") {
		t.Errorf("key = %q, want a unix: key for a local (socket) profile", got)
	}
}

// A -P profile override must move the cache KEY with it — the key names the
// endpoint the answer came from, and a command pointed at a scratch daemon's
// profile must not read, write or drop the default profile's entries. The key
// must also equal the identity the endpoint resolver answers, so reads,
// writes and drops can never drift from what is actually dialed.
func TestCompletionKeyFollowsTheProfileOverride(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"default": {Name: "default", Socket: "/tmp/default-scratch/controller.sock"},
		"scratch": {Name: "scratch", Socket: "/tmp/scratch-1/rafiki/controller.sock"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("default"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().StringP("profile", "P", "", "")
	if err := cmd.Flags().Set("profile", "scratch"); err != nil {
		t.Fatal(err)
	}

	want := "unix:/tmp/scratch-1/rafiki/connect.sock"
	if got := completionEndpointKey(cmd); got != want {
		t.Errorf("key = %q, want %q — the override must move the identity off the default profile", got, want)
	}
	if got := completionEndpointKey(cmd); got == "unix:/tmp/default-scratch/connect.sock" {
		t.Errorf("key = %q, still the DEFAULT profile's connect socket", got)
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.identity != completionEndpointKey(cmd) {
		t.Errorf("endpoint identity %q != cache key %q — reads, writes and drops must not drift from what is dialed", ep.identity, completionEndpointKey(cmd))
	}
}

// A remote endpoint with no token cannot succeed. It must be a miss, not the
// error newConnectEndpoint returns for interactive callers.
func TestChildCompletionWithoutATokenIsEmpty(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if got := completionChildren(nil); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}
