// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"go.graveland.dev/rafiki/pkg/paths"
)

// Completion caches. A TAB press must not pay a round trip for an answer that
// was true a moment ago, and the pattern is not new here — pkg/models already
// keeps an on-disk OpenRouter snapshot behind a 24h TTL. These aim the same
// shape at the DAEMON's answer.
//
// The two TTLs differ because the two are different kinds of fact. Models
// change on the order of days; the daemon's own catalog TTL is 24h, so an hour
// is conservative and `rafiki models` bypasses the cache to force a refresh.
// Child names change constantly — but the staleness that actually bites is
// self-inflicted (create a child, immediately TAB for it), and every local
// mutating verb drops the entry, so 15s only ever hides a child that ANOTHER
// actor created.
const (
	// Both are consumed by the completion wiring (plan tasks 7 and 8); the
	// cache itself ships first so the TTLs are decided with it.
	childCacheTTL = 15 * time.Second //nolint:unused // wired by completion handlers (task 7)
	modelCacheTTL = time.Hour        //nolint:unused // wired by completion handlers (task 8)
)

// cacheEntry wraps the payload with its write time. The TTL is applied by the
// READER so one file can serve callers with different freshness needs.
type cacheEntry struct {
	Written time.Time       `json:"written"`
	Payload json.RawMessage `json:"payload"`
}

// cachePath is <cache>/completion/<kind>-<hash of endpoint>.json.
//
// Hashing the endpoint keeps two daemons' answers apart: with RAFIKI_URL set
// and unset in two shells, a shared key would offer the remote daemon's
// children to the local one. It is also why the endpoint is not used directly
// — a URL is not a safe filename.
func cachePath(kind, endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	name := kind + "-" + base64.RawURLEncoding.EncodeToString(sum[:8]) + ".json"
	return filepath.Join(paths.CacheDir(), "completion", name)
}

// cacheRead unmarshals a fresh entry into out. Every failure — missing,
// unreadable, corrupt, stale — is a miss, because a completion handler must
// degrade to no candidates rather than to an error.
func cacheRead(kind, endpoint string, ttl time.Duration, out any) bool {
	b, err := os.ReadFile(cachePath(kind, endpoint))
	if err != nil {
		return false
	}
	var e cacheEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return false
	}
	if ttl <= 0 || time.Since(e.Written) > ttl {
		return false
	}
	return json.Unmarshal(e.Payload, out) == nil
}

// cacheWrite stores v. Best-effort and silent: a cache that cannot be written
// costs a round trip, which is exactly what the uncached path already pays.
func cacheWrite(kind, endpoint string, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	b, err := json.Marshal(cacheEntry{Written: time.Now(), Payload: payload})
	if err != nil {
		return
	}
	path := cachePath(kind, endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// Write-then-rename: a completion handler reading a half-written file
	// would see a corrupt entry, and these run concurrently by nature (a shell
	// spawns one per TAB).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// cacheDrop removes an entry. Called by every verb that mutates what the entry
// describes, which is what makes the 15s child TTL safe: the staleness a user
// actually notices is the one they caused themselves.
func cacheDrop(kind, endpoint string) {
	_ = os.Remove(cachePath(kind, endpoint))
}

// corruptCacheForTest truncates an entry to invalid JSON. Test-only hook, here
// rather than in the test file because cachePath is unexported.
func corruptCacheForTest(kind, endpoint string) error {
	return os.WriteFile(cachePath(kind, endpoint), []byte("{not json"), 0o600)
}
