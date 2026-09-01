// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

// The cache is the only part of this path testable without a live daemon: the
// fetch itself needs a Connect server. What matters here is that a cached
// answer is served without one, and that a drop forces a refetch.
func TestChildCompletionServesFromCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	want := []completionChild{{ChildID: "c_1", Name: "alpha", Status: "idle"}}
	cacheWrite("children", completionEndpointKey(), want)

	got := completionChildrenCached(childCacheTTL)
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("got %+v, want the cached row", got)
	}
}

func TestDropChildCompletionCacheForcesARefetch(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	cacheWrite("children", completionEndpointKey(), []completionChild{{Name: "alpha"}})
	dropChildCompletionCache()

	if got := completionChildrenCached(childCacheTTL); len(got) != 0 {
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
	if got := completionEndpointKey(); got != "https://daemon.example" {
		t.Errorf("key = %q, want the remote URL", got)
	}

	t.Setenv("RAFIKI_URL", "")
	if got := completionEndpointKey(); !strings.HasPrefix(got, "unix:") {
		t.Errorf("key = %q, want a unix: key with no RAFIKI_URL set", got)
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
