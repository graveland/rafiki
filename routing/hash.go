// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// PrefixHash returns a sha256 of the request's static cache-prefix: the request
// with the volatile "messages" list removed and keys canonicalized (Go marshals
// map keys in sorted order). The same model + system prompt + tools therefore
// hashes identically across a conversation's turns.
//
// Diagnostic uses: a change mid-conversation flags dynamic content leaking into
// the Anthropic-cached prefix (the classic cache-buster); a reordered tools array
// changes the hash, catching non-deterministic tool ordering; and it correlates
// with the captured cache_read/cache_creation tokens to explain WHY a cache broke.
// Returns "" when the request can't be parsed (best-effort; column is nullable).
func PrefixHash(requestJSON []byte) string {
	var m map[string]any
	if err := json.Unmarshal(requestJSON, &m); err != nil {
		return ""
	}
	delete(m, "messages")
	canonical, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
