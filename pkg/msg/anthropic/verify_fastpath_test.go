// SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"runtime"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// TestDecodeOfOwnEncodingSkipsCanonicalisation pins the byte-equality fast path
// in verify: decoding what EncodeContent wrote must not canonicalise a large
// tool input (that parses and copies it several times over), while the same
// block with its keys reordered — equal in content, not in bytes — must still
// decode to the typed block through the full comparison.
func TestDecodeOfOwnEncodingSkipsCanonicalisation(t *testing.T) {
	c := assert.NewCollecting(t)
	const size = 200_000
	input := `{"data":"` + strings.Repeat("x", size) + `"}`
	content, err := EncodeContent(msg.Message{Role: msg.Assistant,
		Content: []msg.Block{msg.ToolUse{ID: "t", Name: "w", Input: []byte(input)}}})
	c.Require().NoError(err, "EncodeContent")

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	m, err := Decode("assistant", content)
	runtime.ReadMemStats(&after)
	c.Require().NoError(err, "Decode")
	_, typed := m.Content[0].(msg.ToolUse)
	c.True(typed, "own encoding must decode to a typed ToolUse, got %T", m.Content[0])
	got := after.TotalAlloc - before.TotalAlloc
	c.True(got < 20*size, "Decode of a %d-byte tool input allocated %d bytes; the fast path is not skipping canonicalisation", size, got)

	reordered := `[{"name":"w","input":{"a":1},"id":"t","type":"tool_use"}]`
	m, err = Decode("assistant", []byte(reordered))
	c.Require().NoError(err, "Decode reordered")
	tu, typed := m.Content[0].(msg.ToolUse)
	c.Require().True(typed, "reordered keys must still verify to a typed ToolUse, got %T", m.Content[0])
	c.Eq("w", tu.Name, "name")
}
