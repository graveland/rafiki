// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
)

// The cache is the only part of this path testable without a live daemon: the
// fetch itself needs a Connect server. What matters here is that a cached
// answer is served without one, and that a drop forces a refetch.
func TestChildCompletionServesFromCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	want := []completionChild{{ChildID: "c_1", Name: "alpha", Status: "idle"}}
	cacheWrite("children", completionEndpointKey(nil), want)

	got := completionChildrenCached(nil, childCacheTTL)
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("got %+v, want the cached row", got)
	}
}

func TestDropChildCompletionCacheForcesARefetch(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	cacheWrite("children", completionEndpointKey(nil), []completionChild{{Name: "alpha"}})
	dropChildCompletionCache(nil)

	if got := completionChildrenCached(nil, childCacheTTL); len(got) != 0 {
		t.Errorf("got %+v after a drop, want no cached rows", got)
	}
}

// An unreachable daemon must yield no candidates, never an error or an exit.
func TestChildCompletionOnAnUnreachableDaemonIsEmpty(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://127.0.0.1:1")
	t.Setenv("RAFIKI_TOKEN", "t")

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

// The bug this task closes: the old helpers read --socket and never consulted
// RAFIKI_URL, so a remote operator got no candidates at all, silently.
func TestCompletionEndpointPrefersRafikiURLOverTheSocket(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://daemon.example")
	if got := completionEndpointKey(nil); got != "https://daemon.example" {
		t.Errorf("key = %q, want the remote URL", got)
	}

	t.Setenv("RAFIKI_URL", "")
	if got := completionEndpointKey(nil); !strings.HasPrefix(got, "unix:") {
		t.Errorf("key = %q, want a unix: key with no RAFIKI_URL set", got)
	}
}

// A --socket override must move the cache KEY with it — the key names the
// endpoint the answer came from, and a command pointed at a scratch daemon
// must not read, write or drop the default daemon's entries. The key must also
// equal the identity the endpoint resolver answers, so reads, writes and drops
// can never drift from what is actually dialed.
func TestCompletionKeyFollowsTheSocketOverride(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "")
	t.Setenv("RAFIKI_TOKEN", "")

	cmd := &cobra.Command{}
	cmd.Flags().String("socket", "", "")
	if err := cmd.Flags().Set("socket", "/tmp/scratch-1/rafiki/controller.sock"); err != nil {
		t.Fatal(err)
	}

	want := "unix:/tmp/scratch-1/rafiki/connect.sock"
	if got := completionEndpointKey(cmd); got != want {
		t.Errorf("key = %q, want %q — the override must move the identity off the default daemon", got, want)
	}
	if got := completionEndpointKey(cmd); got == "unix:"+paths.ConnectSocketPath() {
		t.Errorf("key = %q, still the DEFAULT daemon's connect socket", got)
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
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "")

	if got := completionChildren(nil); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}
