// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestApproxBytesCountsEveryBlockKindPayload(t *testing.T) {
	big := strings.Repeat("x", 1000)
	for name, b := range map[string]Block{
		"text":        Text{Text: big},
		"thinking":    Thinking{Text: big},
		"tool_use":    ToolUse{Input: []byte(big)},
		"tool_result": ToolResult{Content: []Block{Text{Text: big}}},
		"image":       Image{Data: []byte(big)},
		"opaque":      Opaque{Raw: []byte(big)},
		"text_ext":    Text{Ext: Ext{Raw: big}},
	} {
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			got := Message{Content: []Block{b}}.ApproxBytes()
			c.True(got >= 1000, "payload not counted: %d", got)
			c.True(got < 1000+512, "header overhead out of range: %d", got)
		})
	}
}

func TestApproxBytesPointerBlocksMatchValueBlocks(t *testing.T) {
	c := assert.NewAborting(t)
	v := Message{Content: []Block{Text{Text: "hello"}}}
	p := Message{Content: []Block{&Text{Text: "hello"}}}
	c.Eq(v.ApproxBytes(), p.ApproxBytes())
}

func TestApproxBytesNilPointerBlockIsZero(t *testing.T) {
	c := assert.NewAborting(t)
	var tp *Text
	c.Eq(Message{}.ApproxBytes(), Message{Content: []Block{tp}}.ApproxBytes())
}
