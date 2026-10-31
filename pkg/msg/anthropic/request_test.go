package anthropic_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
)

// TestGoldenEncodeAnthropic is the codec's contract with the recorded corpus:
// for every fixed request, what Encode builds must mean exactly what today's
// SDK-era builder built. Comparison is canonical, never byte-wise, because key
// order is not part of the promise.
func TestGoldenEncodeAnthropic(t *testing.T) {
	for _, tc := range msgtest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			c := assert.NewAborting(t)
			body, err := anthropic.Encode(tc.Req, msg.EncodeOptions{Extra: tc.Extra})
			c.NoError(err, "encode %s", tc.Name)
			compareCanonical(t, msgtest.Golden(filepath.Join("golden", tc.Name+".anthropic.json")), body)
		})
	}
}

// TestEncodeDeterministic pins that the same request encodes to the same bytes
// twice: the body is built from ordered structs and merged through a map, so
// nothing about it may depend on iteration order.
func TestEncodeDeterministic(t *testing.T) {
	c := assert.NewAborting(t)
	for _, tc := range msgtest.Corpus() {
		first, err := anthropic.Encode(tc.Req, msg.EncodeOptions{Extra: tc.Extra, Stream: true})
		c.NoError(err, "encode %s", tc.Name)
		second, err := anthropic.Encode(tc.Req, msg.EncodeOptions{Extra: tc.Extra, Stream: true})
		c.NoError(err, "re-encode %s", tc.Name)
		c.Eq(string(first), string(second), "%s does not encode deterministically", tc.Name)
	}
}

// TestEncodeDropsForeignThinking pins the cross-provider rule: only this
// dialect's reasoning may be resent. The foreign blocks disappear whole, text
// and all, because a signature this API cannot verify fails the request.
func TestEncodeDropsForeignThinking(t *testing.T) {
	c := assert.NewAborting(t)
	body := encodeCase(t, "thinking-foreign")

	content := messageContent(t, body, 0)
	c.Len(content, 2, "the foreign thinking blocks must be gone")
	c.Eq("thinking", content[0]["type"], "the anthropic thinking block must survive")
	c.Eq("sig-a", content[0]["signature"], "only the anthropic signature may be resent")
	c.Eq("done", content[1]["text"], "the trailing text must survive")
}

// TestEncodeClearsForeignExt pins that residue another dialect wrote never
// reaches this API: it would be a field the API rejects or ignores.
func TestEncodeClearsForeignExt(t *testing.T) {
	c := assert.NewAborting(t)
	req := &msg.Request{
		Model:     "test-model",
		MaxTokens: 64,
		Messages: []msg.Message{{Role: msg.Assistant, Content: []msg.Block{
			msg.Text{Text: "hello", Ext: msg.Ext{
				Dialect: msg.DialectResponsesAPI,
				Raw:     `{"foreign_key":"value"}`,
			}},
		}}},
	}
	body, err := anthropic.Encode(req, msg.EncodeOptions{})
	c.NoError(err, "encode")
	block := messageContent(t, body, 0)[0]
	c.NotHasKey(block, "foreign_key", "another dialect's residue must not be sent")
	c.Eq("hello", block["text"])
}

// TestEncodeKeepsOwnExt pins the other half: this dialect's own residue is what
// makes a tool_use's caller or a text block's citations survive a round trip,
// so it must reach the wire.
func TestEncodeKeepsOwnExt(t *testing.T) {
	c := assert.NewAborting(t)
	req := &msg.Request{
		Model:     "test-model",
		MaxTokens: 64,
		Messages: []msg.Message{{Role: msg.Assistant, Content: []msg.Block{
			msg.ToolUse{
				ID:    "toolu_1",
				Name:  "read_file",
				Input: json.RawMessage(`{"path":"a"}`),
				Ext:   msg.Ext{Dialect: msg.DialectAnthropic, Raw: `{"caller":{"type":"direct"}}`},
			},
		}}},
	}
	body, err := anthropic.Encode(req, msg.EncodeOptions{})
	c.NoError(err, "encode")
	block := messageContent(t, body, 0)[0]
	c.HasKey(block, "caller", "this dialect's residue must be sent")
	c.EqDeep(any(map[string]any{"type": "direct"}), block["caller"])
}

// TestEncodeDoesNotMutateRequest pins that the codec's filtering and marker
// placement happen on copies. The conversation's history is the caller's, and a
// request that changed it in place would corrupt the stored rows the next send
// is built from.
func TestEncodeDoesNotMutateRequest(t *testing.T) {
	c := assert.NewAborting(t)
	for _, tc := range msgtest.Corpus() {
		before := snapshot(t, tc.Req)
		_, err := anthropic.Encode(tc.Req, msg.EncodeOptions{Extra: tc.Extra, Stream: true})
		c.NoError(err, "encode %s", tc.Name)
		c.Eq(before, snapshot(t, tc.Req), "Encode mutated the %s request", tc.Name)
	}
}

// encodeCase encodes one corpus case and returns the body.
func encodeCase(t *testing.T, name string) []byte {
	t.Helper()
	c := assert.NewAborting(t)
	for _, tc := range msgtest.Corpus() {
		if tc.Name != name {
			continue
		}
		body, err := anthropic.Encode(tc.Req, msg.EncodeOptions{Extra: tc.Extra})
		c.NoError(err, "encode %s", name)
		return body
	}
	t.Fatalf("the corpus has no case named %q", name)
	return nil
}

// messageContent returns the decoded content blocks of one message of an
// encoded body.
func messageContent(t *testing.T, body []byte, index int) []map[string]any {
	t.Helper()
	c := assert.NewAborting(t)
	var wire struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	c.NoError(json.Unmarshal(body, &wire), "read encoded body")
	if index >= len(wire.Messages) {
		t.Fatalf("the body carries %d message(s), wanted index %d", len(wire.Messages), index)
	}
	return wire.Messages[index].Content
}

// snapshot renders req as JSON, which is a deep copy of everything Encode
// could reach.
func snapshot(t *testing.T, req *msg.Request) string {
	t.Helper()
	c := assert.NewAborting(t)
	raw, err := json.Marshal(req)
	c.NoError(err, "snapshot request")
	return string(raw)
}

// compareCanonical asserts that two JSON documents are the same value, key
// order and whitespace aside.
func compareCanonical(t *testing.T, want, got []byte) {
	t.Helper()
	c := assert.NewAborting(t)
	var w, g any
	c.NoError(json.Unmarshal(want, &w), "the golden is not JSON")
	c.NoError(json.Unmarshal(got, &g), "the built body is not JSON")
	c.EqDeep(w, g, "the built body does not match its golden")
}
