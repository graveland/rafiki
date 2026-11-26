// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"go.graveland.dev/rafiki/pkg/anthropicreq"
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
//
// The rule itself lives in pkg/anthropicreq, which owns the one implementation
// the proxy reaches off its single parse of the body. This stays the byte-slice
// entry point for callers that only ever have bytes (the OpenAI face, tests).
func PrefixHash(requestJSON []byte) string {
	req, _ := anthropicreq.Parse(requestJSON)
	return req.PrefixHash()
}
