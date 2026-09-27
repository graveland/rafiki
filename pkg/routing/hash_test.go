// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestPrefixHash(t *testing.T) {
	c := assert.NewCollecting(t)
	base := []byte(`{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"a"},{"name":"b"}],"messages":[{"role":"user","content":"hi"}]}`)
	h := PrefixHash(base)
	c.Require().NotEq("", h, "expected a hash")

	// Message-invariant: a different/growing message list must not change the prefix hash.
	moreMsgs := []byte(`{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"a"},{"name":"b"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`)
	c.Eq(h, PrefixHash(moreMsgs), "prefix hash changed when only the message list changed")

	// Top-level key order is irrelevant (map marshal canonicalizes).
	reordered := []byte(`{"tools":[{"name":"a"},{"name":"b"}],"messages":[],"system":"you are helpful","model":"claude-sonnet-5"}`)
	c.Eq(h, PrefixHash(reordered), "prefix hash changed on top-level key reorder (should be canonical)")

	// Tool ARRAY order IS significant — a reordered tools array is a different prefix
	// (this is the property that catches non-deterministic tool ordering).
	toolsSwapped := []byte(`{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"b"},{"name":"a"}],"messages":[]}`)
	c.NotEq(h, PrefixHash(toolsSwapped), "prefix hash did NOT change when the tools array was reordered")

	// A changed system prompt is a different prefix.
	sysChanged := []byte(`{"model":"claude-sonnet-5","system":"you are DIFFERENT","tools":[{"name":"a"},{"name":"b"}],"messages":[]}`)
	c.NotEq(h, PrefixHash(sysChanged), "prefix hash did NOT change when the system prompt changed")

	// Unparseable request → empty (best-effort; column is nullable).
	c.Eq("", PrefixHash([]byte("not json")), "expected empty hash for unparseable request")
}
