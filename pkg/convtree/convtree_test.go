package convtree

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

var update = flag.Bool("update", false, "rewrite testdata/golden.json from the fixture")

const (
	captureDir = "../../testdata/claude-capture"
	goldenPath = "testdata/golden.json"
)

func TestCanonStringEqualsTextBlock(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(Equal(
		Msg{Role: "user", Content: []byte(`"hi"`)},
		Msg{Role: "user", Content: []byte(`[{"type":"text","text":"hi"}]`)},
	), `"hi" and its text block must canonicalise equal`)
}

func TestCanonStripsCacheControl(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(Equal(
		Msg{Role: "user", Content: []byte(`[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]`)},
		Msg{Role: "user", Content: []byte(`[{"type":"text","text":"a"}]`)},
	), "cache_control on a block is a transport hint, not content")
	c.True(Equal(
		Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","cache_control":{"type":"ephemeral"},"content":[{"type":"text","text":"A","cache_control":{"type":"ephemeral"}}]}]`)},
		Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"A"}]}]`)},
	), "cache_control is stripped at every depth, including inside a tool_result's content")
}

func TestCanonDropsNullKeys(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(Equal(
		Msg{Role: "assistant", Content: []byte(`[{"type":"text","text":"a","citations":null}]`)},
		Msg{Role: "assistant", Content: []byte(`[{"type":"text","text":"a"}]`)},
	), "an explicit null and an absent key mean the same thing")
}

func TestCanonKeyOrderIrrelevant(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(Equal(
		Msg{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"t1","name":"x","input":{"b":1,"a":2}}]`)},
		Msg{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"t1","name":"x","input":{"a":2,"b":1}}]`)},
	), "encoding/json sorts map keys, so key order cannot change the hash")
}

// TestCanonKeepsToolResultContent is the regression the SDK round trip had:
// decoding into the API's content union dropped tool_result.content,
// tool_result.is_error and image.source, so three different messages
// canonicalised to one shape.
func TestCanonKeepsToolResultContent(t *testing.T) {
	c := assert.NewAborting(t)
	c.False(Equal(
		Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"A"}]`)},
		Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"B"}]`)},
	), "tool_result.content is content")
	c.False(Equal(
		Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"A","is_error":false}]`)},
		Msg{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"t1","content":"A","is_error":true}]`)},
	), "tool_result.is_error is content")
	c.False(Equal(
		Msg{Role: "user", Content: []byte(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`)},
		Msg{Role: "user", Content: []byte(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBBB"}}]`)},
	), "image.source.data is content")
}

func TestCanonPreservesNumbers(t *testing.T) {
	c := assert.NewAborting(t)
	got, err := Canon("user", []byte(`[{"type":"text","text":"x","n":1.50}]`))
	c.NoError(err, "Canon")
	c.True(bytes.Contains(got, []byte(`1.50`)), "UseNumber must keep the literal 1.50, got %s", got)
	other, err := Canon("user", []byte(`[{"type":"text","text":"x","n":1.5}]`))
	c.NoError(err, "Canon")
	c.False(bytes.Equal(got, other), "1.50 and 1.5 are different literals")
}

// TestCanonEscapesHTMLAndLineSeparators pins the exact bytes Canon produces for
// the characters encoding/json escapes by default: <, > and & become \u003c,
// \u003e and \u0026, and U+2028/U+2029 become \u2028/\u2029. Those bytes are
// what NodeHash hashes, and the reference doc states them as the compatibility
// contract a non-Go reimplementation must reproduce — so a change to the
// marshalling (an Encoder with SetEscapeHTML(false), say) has to fail here
// rather than silently rehash every stored root.
//
// The assertion is on the canon alone: NodeHash mixes Canon's output verbatim,
// so there is nothing separate left to check in the hash.
func TestCanonEscapesHTMLAndLineSeparators(t *testing.T) {
	c := assert.NewAborting(t)

	// A bare JSON string, so the message is one text block; the two separators are
	// written as Go escapes so the source stays readable.
	got, err := Canon("user", []byte("\"<>&\u2028\u2029\""))
	c.NoError(err, "Canon")

	for _, tc := range []struct {
		escaped string
		raw     string
		why     string
	}{
		{`\u003c`, "<", "less-than"},
		{`\u003e`, ">", "greater-than"},
		{`\u0026`, "&", "ampersand"},
		{`\u2028`, "\u2028", "U+2028 line separator"},
		{`\u2029`, "\u2029", "U+2029 paragraph separator"},
	} {
		c.True(bytes.Contains(got, []byte(tc.escaped)),
			"Canon must write %s as the escape sequence %s, got %q", tc.why, tc.escaped, got)
		c.False(bytes.Contains(got, []byte(tc.raw)),
			"Canon must not carry %s raw: its bytes are what the hash covers, got %q", tc.why, got)
	}
}

func TestCanonInvalidJSON(t *testing.T) {
	c := assert.NewAborting(t)
	_, err := Canon("user", []byte(`{"type":`))
	c.Error(err, "truncated JSON must not canonicalise")
}

func TestRootSeedDistinguishesKindAndDerivedFrom(t *testing.T) {
	c := assert.NewAborting(t)
	h1 := mustNodeHash(t, RootSeed(RootKindRoot, nil), Msg{Role: "user", Content: []byte(`"one"`)})
	h2 := mustNodeHash(t, RootSeed(RootKindRoot, nil), Msg{Role: "user", Content: []byte(`"two"`)})

	c.NotEq(RootSeed(RootKindClear, &h1), RootSeed(RootKindClear, &h2),
		"a clear boundary is a constant, so the node it was cut from must distinguish two clears")
	c.NotEq(RootSeed(RootKindClear, &h1), RootSeed(RootKindCompaction, &h1),
		"the root kind is part of the seed")
	c.Eq(RootSeed(RootKindCompaction, &h1), RootSeed(RootKindCompaction, &h2),
		"a compaction summary is unique model output; derived-from must not seed it")
	c.Eq(RootSeed(RootKindCompaction, &h1), RootSeed(RootKindCompaction, nil),
		"a compaction root ignores derived-from entirely")
	c.Eq(RootSeed(RootKindRoot, &h1), RootSeed(RootKindRoot, nil),
		"a plain root ignores derived-from entirely")
}

// TestRootSeedBytesAreTheCompatibilityContract pins the literal hash input of
// RootSeed, byte for byte. Every stored root's path hash is computed from it, so
// reordering its components (the kind before or after the derived-from hash, a
// different separator, dropping the "root" prefix) would change every root hash
// in the database while passing every other test in this package — and passing
// the tree check too, because the check calls this same function. A stored root
// hash that no longer recomputes is an orphan: AppendChain cannot find the node a
// compaction or clear retry would land on.
//
// These bytes are the compatibility contract. Changing them requires bumping
// HashVersion, so that no hash written under the old rule can collide with one
// written under the new.
func TestRootSeedBytesAreTheCompatibilityContract(t *testing.T) {
	c := assert.NewAborting(t)
	// Fixed derived-from hashes, not NodeHash output, so this pins RootSeed's own
	// bytes and nothing else.
	var d1, d2 Hash
	for i := range d1 {
		d1[i] = 0x01
		d2[i] = 0x02
	}
	hashOf := func(hexBytes string) Hash {
		raw, err := hex.DecodeString(hexBytes)
		c.NoError(err, "decode golden seed")
		var h Hash
		copy(h[:], raw)
		return h
	}

	cases := []struct {
		name string
		got  Hash
		want string
	}{
		{"root without derived-from", RootSeed(RootKindRoot, nil),
			"9c4f9b0dd19d7add2737999f9226a374ec7483c8dd63021a288c25776f69fb80"},
		{"root with a derived-from", RootSeed(RootKindRoot, &d1),
			"9c4f9b0dd19d7add2737999f9226a374ec7483c8dd63021a288c25776f69fb80"},
		{"compaction_summary without derived-from", RootSeed(RootKindCompaction, nil),
			"0e1bd354aaa5ab15f8fe581368d5938059b9984966217e307e028958cc71f0ee"},
		{"compaction_summary with a derived-from", RootSeed(RootKindCompaction, &d1),
			"0e1bd354aaa5ab15f8fe581368d5938059b9984966217e307e028958cc71f0ee"},
		{"clear without derived-from", RootSeed(RootKindClear, nil),
			"3a6b6e8f744b2aad8f267fe4649dbbeec70942aaa7b3c875310f6e91bbfaf24d"},
		{"clear derived from d1", RootSeed(RootKindClear, &d1),
			"db9d68b2d25fb9d11d9225c3ae700e2d1ecb45c599dc9706081a7ecf1d5ed1ca"},
		{"clear derived from d2", RootSeed(RootKindClear, &d2),
			"74f4ac25125e6b138a53ab587703033464f6e60c4e86c5c75dfcca8d5ea3c0da"},
	}
	for _, tc := range cases {
		c.Eq(hashOf(tc.want), tc.got, "%s: RootSeed bytes changed; bump HashVersion if this is intended", tc.name)
	}

	c.Eq(RootSeed(RootKindRoot, &d1), RootSeed(RootKindRoot, nil),
		"a plain root ignores derived-from")
	c.Eq(RootSeed(RootKindCompaction, &d1), RootSeed(RootKindCompaction, nil),
		"a compaction root ignores derived-from")
	c.NotEq(RootSeed(RootKindClear, &d1), RootSeed(RootKindClear, &d2),
		"a clear is distinguished by the node it was cut from")
	c.NotEq(RootSeed(RootKindClear, nil), RootSeed(RootKindClear, &d1),
		"a clear cut from a node differs from one with no derived-from")
}

func TestChainIsParentSensitive(t *testing.T) {
	c := assert.NewAborting(t)
	p1 := RootSeed(RootKindRoot, nil)
	p2 := RootSeed(RootKindClear, nil)
	a := Msg{Role: "user", Content: []byte(`"a"`)}
	b := Msg{Role: "assistant", Content: []byte(`"b"`)}

	ha := mustNodeHash(t, p1, a)
	hb := mustNodeHash(t, p2, a)
	c.NotEq(ha, hb, "the same message under two parent slots must hash differently")

	want := []Hash{ha, mustNodeHash(t, ha, b)}

	got, err := Chain(p1, []Msg{a, b})
	c.NoError(err, "Chain")
	c.EqDeep(want, got, "Chain must chain each hash onto the previous one")
}

// TestGoldenFixtureChains pins the path hashes of a real captured Claude Code
// session. The capture is the awkward shapes of request in one conversation:
// tool calls and results, a failed tool, a killed-and-resumed turn that
// replays a rewritten assistant message, /compact and /clear. Regenerate with
// -update, which rewrites testdata/golden.json.
func TestGoldenFixtureChains(t *testing.T) {
	c := assert.NewAborting(t)
	golden := map[string][]string{}
	for _, name := range requestFiles(t) {
		msgs := readMessages(t, filepath.Join(captureDir, name))
		hashes, err := Chain(RootSeed(RootKindRoot, nil), msgs)
		c.NoError(err, "Chain %s", name)
		hexes := make([]string, len(hashes))
		for i, h := range hashes {
			hexes[i] = hex.EncodeToString(h[:])
		}
		golden[name] = hexes
	}

	if *update {
		buf, err := json.MarshalIndent(golden, "", "  ")
		c.NoError(err, "marshal golden")
		c.NoError(os.MkdirAll(filepath.Dir(goldenPath), 0o755), "mkdir testdata")
		c.NoError(os.WriteFile(goldenPath, append(buf, '\n'), 0o644), "write golden")
		return
	}

	raw, err := os.ReadFile(goldenPath)
	c.NoError(err, "read %s (regenerate with -update)", goldenPath)
	want := map[string][]string{}
	c.NoError(json.Unmarshal(raw, &want), "unmarshal golden")
	c.EqDeep(want, golden, "captured session path hashes changed")
}

// TestParityStoredVersusReplayed proves the hash survives the round trip a
// stored message makes: decoded into generic JSON and re-marshalled, which
// changes whitespace and key order but nothing the hash covers.
func TestParityStoredVersusReplayed(t *testing.T) {
	c := assert.NewAborting(t)
	files := requestFiles(t)
	last := files[len(files)-1]
	msgs := readMessages(t, filepath.Join(captureDir, last))
	c.NotEmpty(msgs, "%s has no messages", last)

	slot := RootSeed(RootKindRoot, nil)
	rewritten := 0
	for i, m := range msgs {
		dec := json.NewDecoder(bytes.NewReader(m.Content))
		dec.UseNumber()
		var v any
		c.NoError(dec.Decode(&v), "%s message %d: decode content", last, i)
		replayed, err := json.Marshal(v)
		c.NoError(err, "%s message %d: marshal content", last, i)
		if !bytes.Equal(replayed, m.Content) {
			rewritten++
		}
		want, err := NodeHash(slot, m)
		c.NoError(err, "%s message %d: NodeHash", last, i)
		got, err := NodeHash(slot, Msg{Role: m.Role, Content: replayed})
		c.NoError(err, "%s message %d: NodeHash replayed", last, i)
		c.Eq(want, got, "%s message %d: re-marshalling changed the hash", last, i)
		slot = want
	}
	c.NotZero(rewritten, "%s: no message was re-marshalled, so this proves nothing", last)
}

func mustNodeHash(t *testing.T, parent Hash, m Msg) Hash {
	t.Helper()
	c := assert.NewAborting(t)
	h, err := NodeHash(parent, m)
	c.NoError(err, "NodeHash")
	return h
}

func requestFiles(t *testing.T) []string {
	t.Helper()
	c := assert.NewAborting(t)
	entries, err := os.ReadDir(captureDir)
	c.NoError(err, "ReadDir %s", captureDir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "-request.http") {
			names = append(names, e.Name())
		}
	}
	c.NotEmpty(names, "no captured requests under %s", captureDir)
	sort.Strings(names)
	return names
}

func readMessages(t *testing.T, path string) []Msg {
	t.Helper()
	c := assert.NewAborting(t)
	raw, err := os.ReadFile(path)
	c.NoError(err, "read %s", path)
	body := httpBody(raw)
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	c.NoError(json.Unmarshal(body, &req), "unmarshal %s", path)
	msgs := make([]Msg, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = Msg{Role: m.Role, Content: m.Content}
	}
	return msgs
}

func httpBody(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[i+4:]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[i+2:]
	}
	return raw
}
