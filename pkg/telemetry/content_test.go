// SPDX-License-Identifier: Apache-2.0

package telemetry_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/telemetry"
)

func TestUserMessageUTF8Boundary(t *testing.T) {
	c := assert.NewCollecting(t)
	in := strings.Repeat("a", 2047) + "é"
	c.Eq(2049, len(in), "fixture length")

	out := telemetry.UserMessage(in)
	c.Eq(2047, len(out))
	c.True(utf8.ValidString(out), "output must be valid UTF-8")
	c.Eq(strings.Repeat("a", 2047), out)

	c.Eq("short", telemetry.UserMessage("short"))
}

func TestReplySnippetRunes(t *testing.T) {
	c := assert.NewCollecting(t)
	in := strings.Repeat("é", 250)

	out := telemetry.ReplySnippet(in)
	c.Eq(200, utf8.RuneCountInString(out))
	c.Eq(strings.Repeat("é", 200), out)

	c.Eq("short", telemetry.ReplySnippet("short"))
}

func TestToolArgsLeafRule(t *testing.T) {
	c := assert.NewCollecting(t)
	content := strings.Repeat("x", 300)
	in := `{"file_path":"/a/b","content":"` + content + `"}`

	out, truncated := telemetry.ToolArgs(in)
	c.False(truncated)

	var got map[string]any
	c.NoError(json.Unmarshal([]byte(out), &got), "output %q", out)
	c.Eq("/a/b", got["file_path"].(string), "short leaf must stay verbatim")

	c.NotStrContains(out, content, "the leaf text must not survive")
	c.NotStrContains(out, strings.Repeat("x", 64), "no long run of the leaf may survive")

	sum := sha256.Sum256([]byte(content))
	marker, ok := got["content"].(map[string]any)
	c.Require().True(ok, "oversized leaf must become a marker object, got %T", got["content"])
	c.Eq(hex.EncodeToString(sum[:])[:12], marker["sha256"].(string))
	c.Eq(float64(300), marker["bytes"].(float64))

	exactly256 := strings.Repeat("y", 256)
	out, _ = telemetry.ToolArgs(`{"v":"` + exactly256 + `"}`)
	c.Eq(`{"v":"`+exactly256+`"}`, out, "a 256-byte leaf stays verbatim")

	over257 := strings.Repeat("y", 257)
	out, _ = telemetry.ToolArgs(`{"v":"` + over257 + `"}`)
	c.NotStrContains(out, over257, "a 257-byte leaf is hashed")
	c.StrContains(out, `"bytes":257`)
}

func TestToolArgsNested(t *testing.T) {
	c := assert.NewCollecting(t)
	leaf := strings.Repeat("z", 300)
	in := `{"args":[{"cmd":"` + leaf + `"}]}`

	out, truncated := telemetry.ToolArgs(in)
	c.False(truncated)

	var got map[string]any
	c.NoError(json.Unmarshal([]byte(out), &got), "output %q", out)
	list, ok := got["args"].([]any)
	c.True(ok, "args must stay an array, got %T", got["args"])
	c.Len(list, 1)
	c.NotStrContains(out, leaf, "the nested leaf text must not survive")
	c.NotStrContains(out, strings.Repeat("z", 64), "no long run of the nested leaf may survive")

	obj, ok := list[0].(map[string]any)
	c.Require().True(ok, "nested element must stay an object, got %T", list[0])
	_, hashed := obj["cmd"].(map[string]any)
	c.True(hashed, "leaf inside array inside object must be hashed, got %T", obj["cmd"])
}

func TestToolArgsInvalidJSON(t *testing.T) {
	c := assert.NewCollecting(t)

	long := "not json " + strings.Repeat("q", 300)
	out, truncated := telemetry.ToolArgs(long)
	c.False(truncated)
	c.NotStrContains(out, long, "the raw invalid input must not survive")
	c.NotStrContains(out, strings.Repeat("q", 64), "no long run of the raw invalid input may survive")

	var got map[string]any
	c.Require().NoError(json.Unmarshal([]byte(out), &got), "output %q", out)
	sum := sha256.Sum256([]byte(long))
	c.Len(got, 2, "the marker carries only sha256 and bytes, got %v", got)
	c.Eq(hex.EncodeToString(sum[:])[:12], got["sha256"].(string))
	c.Eq(float64(len(long)), got["bytes"].(float64))

	out, _ = telemetry.ToolArgs("not json")
	c.Eq(`"not json"`, out, "short invalid input becomes a JSON string of itself")
}

func TestToolArgsTotalCap(t *testing.T) {
	c := assert.NewCollecting(t)
	// 84 "€" is 252 bytes: under the 256-byte leaf cap, so every leaf stays
	// verbatim and the total cap is what cuts.
	leaf := strings.Repeat("€", 84)
	parts := make([]string, 20)
	for i := range parts {
		parts[i] = `"` + leaf + `"`
	}
	in := "[" + strings.Join(parts, ",") + "]"
	c.Greater(4096, len(in), "fixture must exceed the total cap")
	// The cut at 4096 has to land inside a 3-byte rune, or the boundary
	// assertion below is vacuous. Leaf k starts at byte 2+255k, so byte 4096
	// sits at offset 14 of the 17th leaf, i.e. a continuation byte.
	c.False(utf8.RuneStart(in[4096]), "fixture must put a multi-byte rune across the cut")

	out, truncated := telemetry.ToolArgs(in)
	c.True(truncated, "twenty 252-byte leaves must hit the total cap")
	c.LessOrEqual(4096, len(out))
	c.Greater(4092, len(out)) // within one 3-byte rune of the cap
	c.True(utf8.ValidString(out), "the cut must land on a rune boundary")
	c.True(strings.HasPrefix(in, out), "the cut must be a prefix of the encoded arguments")
}

// straddlingError builds a >64 KiB error string whose head cut (byte 32768)
// and tail cut (byte len-32768) each land inside a 2-byte rune, so the UTF-8
// boundary handling is actually exercised.
func straddlingError() string {
	const total = 100 * 1024
	var b strings.Builder
	b.Grow(total)
	b.WriteString("FIRST\n")                          // 0..5
	b.WriteString(strings.Repeat("a", 32761))         // 6..32766
	b.WriteString("é")                                // 32767..32768: byte 32768 is a continuation
	b.WriteString(strings.Repeat("a", 36862))         // 32769..69630
	b.WriteString("é")                                // 69631..69632: the tail cut starts here
	b.WriteString(strings.Repeat("a", total-5-69633)) // 69633..total-6
	b.WriteString("\nLAST")                           // the last 5 bytes
	return b.String()
}

func TestErrorTextHeadTail(t *testing.T) {
	c := assert.NewCollecting(t)
	in := straddlingError()
	c.Eq(100*1024, len(in), "fixture length")
	c.Greater(65536, len(in), "fixture must exceed the cap")
	// Both cuts must straddle a rune, or the boundary assertions are vacuous.
	c.False(utf8.RuneStart(in[32768]), "head cut must land mid-rune")
	c.False(utf8.RuneStart(in[len(in)-32768]), "tail cut must land mid-rune")

	out := telemetry.ErrorText(in)
	c.StrContains(out, "FIRST")
	c.StrContains(out, "LAST")
	c.StrContains(out, "bytes elided")
	c.LessOrEqual(65536+64, len(out))
	c.True(utf8.ValidString(out))

	// Split the result back into the two kept regions and check each one on
	// its own: the cuts are what must be rune-safe, not merely the join.
	headEnd := strings.Index(out, "\n…[")
	c.Greater(0, headEnd, "marker must be present")
	tailStart := strings.Index(out[headEnd:], "…\n") + headEnd + len("…\n")
	head, tail := out[:headEnd], out[tailStart:]
	c.True(utf8.ValidString(head), "head must be valid UTF-8")
	c.True(utf8.ValidString(tail), "tail must be valid UTF-8")
	c.True(strings.HasPrefix(in, head), "head must be a prefix of the input")
	c.True(strings.HasSuffix(in, tail), "tail must be a suffix of the input")
	c.Eq(32767, len(head), "head walked back to the rune start before the cut")
	c.Eq(32767, len(tail), "tail walked forward to the rune start after the cut")

	exact := strings.Repeat("b", 65536)
	c.Eq(exact, telemetry.ErrorText(exact), "a 64 KiB error is returned unchanged")
}

func TestErrorTextInvalidUTF8(t *testing.T) {
	t.Run("under the cap", func(t *testing.T) {
		c := assert.NewCollecting(t)
		out := telemetry.ErrorText("boom: \xff done")
		c.True(utf8.ValidString(out), "output must be valid UTF-8, got %q", out)
		c.StrContains(out, "boom:")
		c.StrContains(out, "done")
		c.StrContains(out, "\uFFFD")
		c.NotStrContains(out, "\xff")
	})

	t.Run("over the cap, invalid bytes in head and tail", func(t *testing.T) {
		c := assert.NewCollecting(t)
		in := "FIRST\n\xff" + strings.Repeat("a", 100*1024) + "\xff\nLAST"
		out := telemetry.ErrorText(in)
		c.True(utf8.ValidString(out), "output must be valid UTF-8")
		c.StrContains(out, "FIRST")
		c.StrContains(out, "LAST")
		c.StrContains(out, "bytes elided")
		c.StrContains(out, "\uFFFD")
		c.NotStrContains(out, "\xff")
	})
}
