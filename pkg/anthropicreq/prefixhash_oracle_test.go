// SPDX-License-Identifier: Apache-2.0

package anthropicreq

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

// oraclePrefixHash is the algorithm this package replaced, kept verbatim: the
// whole body unmarshalled into map[string]any, "messages" deleted, marshalled,
// sha256, hex.
//
// It is the only definition of what a stored prefix_hash MEANS — every turn row
// in every database was written by it — and it is compared turn to turn to flag
// a cache break, so a drifted hash would flag a false break on every live
// conversation.
func oraclePrefixHash(requestJSON []byte) string {
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

// rawMarshalHash is what the obvious "skip the any round trip" implementation
// produces: marshal the raw per-key values straight back out. The test below
// uses it to prove the fixtures can tell the two apart.
func rawMarshalHash(r *Request) string {
	m := make(map[string]json.RawMessage, len(r.fields))
	for k, v := range r.fields {
		if k == "messages" {
			continue
		}
		m[k] = v
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// TestPrefixHashMatchesOracle pins PrefixHash to the algorithm it replaced, over
// every fixture, and proves the fixtures DISCRIMINATE: at least one of them
// hashes differently when the `any` round trip is skipped, so a reimplementation
// that drops the canonicalisation cannot pass this test.
func TestPrefixHashMatchesOracle(t *testing.T) {
	c := assert.NewCollecting(t)
	fixtures := bodyFixtures(t)
	c.Require().Greater(0, len(fixtures), "no fixtures were collected")

	var discriminating []string
	var nonEmpty int
	for name, body := range fixtures {
		want := oraclePrefixHash(body)
		req, perr := Parse(body)
		got := req.PrefixHash()
		if perr != nil {
			c.Eq("", got, "%s: an unparseable body must hash to \"\"", name)
			c.Eq("", want, "%s: the oracle disagrees about an unparseable body", name)
			continue
		}
		c.Eq(want, got, "%s: prefix hash drift", name)
		if want == "" {
			continue
		}
		nonEmpty++
		if raw := rawMarshalHash(req); raw != want {
			discriminating = append(discriminating, name)
		}
	}
	c.Require().Greater(5, nonEmpty, "too few fixtures produced a hash to be meaningful")
	if len(discriminating) == 0 {
		t.Fatal("no fixture discriminates: every one hashes the same without the `any` round trip, " +
			"so this test cannot catch an implementation that skips it")
	}
	t.Logf("fixtures whose raw-marshal hash differs (the round trip is load-bearing): %v", discriminating)
}

// TestPrefixHashIsMessageInvariant pins the property the hash exists for: a
// growing message list is the same prefix, while a changed system prompt or a
// reordered tools array is not.
func TestPrefixHashIsMessageInvariant(t *testing.T) {
	c := assert.NewCollecting(t)
	base := `{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"a"},{"name":"b"}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	h := mustParse(t, base).PrefixHash()
	c.Require().NotEq("", h, "expected a hash")

	grown := `{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"a"},{"name":"b"}],` +
		`"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`
	c.Eq(h, mustParse(t, grown).PrefixHash(), "the prefix hash changed with only the message list")

	reordered := `{"tools":[{"name":"a"},{"name":"b"}],"messages":[],"system":"you are helpful","model":"claude-sonnet-5"}`
	c.Eq(h, mustParse(t, reordered).PrefixHash(), "top-level key order must not matter")

	toolsSwapped := `{"model":"claude-sonnet-5","system":"you are helpful","tools":[{"name":"b"},{"name":"a"}],"messages":[]}`
	c.NotEq(h, mustParse(t, toolsSwapped).PrefixHash(), "a reordered tools array is a different prefix")

	sysChanged := `{"model":"claude-sonnet-5","system":"you are DIFFERENT","tools":[{"name":"a"},{"name":"b"}],"messages":[]}`
	c.NotEq(h, mustParse(t, sysChanged).PrefixHash(), "a changed system prompt is a different prefix")

	unparsable, err := Parse([]byte("not json"))
	c.Require().Error(err, "a malformed body must report its decode failure")
	c.Eq("", unparsable.PrefixHash(), "an unparseable request hashes to the empty string")
}

// TestPrefixHashRoundTripIsStable proves the hash is a pure function of the
// bytes: the same body parsed twice, and a body re-marshalled through WithFields
// unchanged, hash the same.
func TestPrefixHashRoundTripIsStable(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(`{"model":"m","system":[{"type":"text","text":"s"}],"tools":[{"name":"t","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}`)
	first := mustParse(t, string(body)).PrefixHash()
	c.Eq(first, mustParse(t, string(body)).PrefixHash(), "two parses of one body must hash the same")

	req := mustParse(t, string(body))
	same, err := req.WithFields(req.Fields())
	c.Require().NoError(err, "WithFields")
	c.Eq(first, same.PrefixHash(), "an untouched rewrite must hash the same")
}

// mustParse is Parse for fixtures the test expects to decode.
func mustParse(t *testing.T, body string) *Request {
	t.Helper()
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse(%q): %v", body, err)
	}
	return req
}
