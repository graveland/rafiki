// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
	"time"
)

func TestCacheRoundTrips(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	cacheWrite("children", "unix:/x", []string{"alpha", "beta"})

	var got []string
	if !cacheRead("children", "unix:/x", time.Minute, &got) {
		t.Fatal("cacheRead returned false for a just-written entry")
	}
	if len(got) != 2 || got[0] != "alpha" {
		t.Errorf("got %v, want [alpha beta]", got)
	}
}

func TestCacheExpires(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheWrite("children", "unix:/x", []string{"alpha"})

	var got []string
	if cacheRead("children", "unix:/x", 0, &got) {
		t.Error("a zero TTL must never serve; got a hit")
	}
}

// Two endpoints must not share an entry, or switching RAFIKI_URL offers the
// other daemon's children.
func TestCacheIsKeyedByEndpoint(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheWrite("children", "unix:/x", []string{"local"})

	var got []string
	if cacheRead("children", "https://remote", time.Minute, &got) {
		t.Errorf("endpoint https://remote read the entry for unix:/x: %v", got)
	}
}

func TestCacheDropRemovesTheEntry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheWrite("children", "unix:/x", []string{"alpha"})
	cacheDrop("children", "unix:/x")

	var got []string
	if cacheRead("children", "unix:/x", time.Minute, &got) {
		t.Error("cacheRead served a dropped entry")
	}
}

// Completion must never fail loudly. An unwritable cache dir, a corrupt file,
// and a missing one are all just misses.
func TestCacheDegradesQuietly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var got []string
	if cacheRead("children", "unix:/nothing", time.Minute, &got) {
		t.Error("a missing entry must be a miss, not a hit")
	}

	cacheWrite("children", "unix:/x", []string{"alpha"})
	if err := corruptCacheForTest("children", "unix:/x"); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if cacheRead("children", "unix:/x", time.Minute, &got) {
		t.Error("a corrupt entry must be a miss, not a hit")
	}
}

// corruptCacheForTest truncates an entry to invalid JSON. Lives in the test
// file because that is the same package — it needs nothing outside it.
func corruptCacheForTest(kind, endpoint string) error {
	return os.WriteFile(cachePath(kind, endpoint), []byte("{not json"), 0o600)
}
