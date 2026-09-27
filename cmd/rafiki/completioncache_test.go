// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestCacheRoundTrips(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewCollecting(t)

	cacheWrite("children", "unix:/x", []string{"alpha", "beta"})

	var got []string
	c.Require().True(cacheRead("children", "unix:/x", time.Minute, &got), "cacheRead returned false for a just-written entry")
	c.False(len(got) != 2 || got[0] != "alpha", "got %v, want [alpha beta]", got)
}

func TestCacheExpires(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheWrite("children", "unix:/x", []string{"alpha"})

	var got []string
	assert.NewCollecting(t).False(cacheRead("children", "unix:/x", 0, &got), "a zero TTL must never serve; got a hit")
}

// Two endpoints must not share an entry, or switching RAFIKI_URL offers the
// other daemon's children.
func TestCacheIsKeyedByEndpoint(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheWrite("children", "unix:/x", []string{"local"})

	var got []string
	assert.NewCollecting(t).False(cacheRead("children", "https://remote", time.Minute, &got), "endpoint https://remote read the entry for unix:/x: %v", got)
}

func TestCacheDropRemovesTheEntry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheWrite("children", "unix:/x", []string{"alpha"})
	cacheDrop("children", "unix:/x")

	var got []string
	assert.NewCollecting(t).False(cacheRead("children", "unix:/x", time.Minute, &got), "cacheRead served a dropped entry")
}

// Completion must never fail loudly. An unwritable cache dir, a corrupt file,
// and a missing one are all just misses.
func TestCacheDegradesQuietly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewCollecting(t)

	var got []string
	c.False(cacheRead("children", "unix:/nothing", time.Minute, &got), "a missing entry must be a miss, not a hit")

	cacheWrite("children", "unix:/x", []string{"alpha"})
	c.Require().NoError(corruptCacheForTest("children", "unix:/x"), "corrupt")
	c.False(cacheRead("children", "unix:/x", time.Minute, &got), "a corrupt entry must be a miss, not a hit")
}

// corruptCacheForTest truncates an entry to invalid JSON. Lives in the test
// file because that is the same package — it needs nothing outside it.
func corruptCacheForTest(kind, endpoint string) error {
	return os.WriteFile(cachePath(kind, endpoint), []byte("{not json"), 0o600)
}

// Concurrent completion processes write the same entry at the same time (a
// shell spawns one per TAB). The final file must always be ONE complete write
// — never a mix of two writers' bytes — so the temp file must be unique per
// write, not a shared "<path>.tmp" two processes can truncate and rename over
// each other. Payloads differ in LENGTH per writer so a mix cannot parse as a
// valid entry of the right shape.
func TestCacheWriteSurvivesConcurrentWriters(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewCollecting(t)

	const writers = 8
	const rounds = 400
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rows := make([]string, 64+w) // length varies per writer
			for i := 0; i < rounds; i++ {
				for j := range rows {
					rows[j] = fmt.Sprintf("writer-%d-round-%d-token-%03d", w, i, j)
				}
				cacheWrite("children", "unix:/x", rows)
			}
		}(w)
	}
	wg.Wait()

	b, err := os.ReadFile(cachePath("children", "unix:/x"))
	c.Require().NoError(err, "final entry unreadable after concurrent writes")
	var e cacheEntry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("final entry is not valid JSON — writers mixed bytes: %v\nraw: %.120s", err, b)
	}
	var got []string
	c.Require().NoError(json.Unmarshal(e.Payload, &got), "final payload is not a valid entry — writers mixed bytes")
	if len(got) < 64 || len(got) >= 64+writers {
		t.Fatalf("final payload has %d rows, want one writer's complete payload (64..%d)", len(got), 63+writers)
	}
	// No temp litter may survive alongside the entry.
	entries, err := os.ReadDir(filepath.Dir(cachePath("children", "unix:/x")))
	c.Require().NoError(err)
	for _, f := range entries {
		// os.CreateTemp names are "<base>.tmp-<random>" — the .tmp- marker is
		// what identifies a leaked temp, a HasSuffix(".tmp") check never would.
		c.NotStrContains(f.Name(), ".tmp-", "temp file")
	}
}
