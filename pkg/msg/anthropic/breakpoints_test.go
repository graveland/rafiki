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

// TestBreakpointsMatchesLegacyPlacement proves the ported placement rule still
// lands where the SDK-era builder put its markers: the positions must be
// exactly the blocks the recorded golden marks.
func TestBreakpointsMatchesLegacyPlacement(t *testing.T) {
	for _, name := range []string{"long-history", "tools-multiturn"} {
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			tc := corpusCase(t, name)
			want := goldenMarkers(t, name)
			c.NotEmpty(want, "%s should carry cache markers", name)
			c.EqDeep(want, anthropic.Breakpoints(tc.Req))
		})
	}
}

// TestBreakpointsOffIsNil pins that a request asking for no caching places no
// markers, whether the TTL says so explicitly or was never set at all.
func TestBreakpointsOffIsNil(t *testing.T) {
	for _, ttl := range []msg.CacheTTL{msg.CacheOff, ""} {
		c := assert.NewAborting(t)
		req := &msg.Request{
			Messages: []msg.Message{{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hi"}}}},
			Cache:    msg.CachePolicy{SystemTTL: msg.Cache5m, MessagesTTL: ttl, Breakpoints: 2},
		}
		c.Empty(anthropic.Breakpoints(req), "a %q message TTL must place no breakpoints", ttl)
	}
}

// TestBreakpointsClampsCount pins the API's three-breakpoint ceiling, the floor
// of one, and the stride between markers: a policy asking for zero still marks
// the last block, one asking for ten still marks only three, and each marker
// sits exactly lookbackStride blocks behind the previous one.
func TestBreakpointsClampsCount(t *testing.T) {
	req := longRequest()
	c := assert.NewAborting(t)

	req.Cache.Breakpoints = 0
	c.EqDeep([]anthropic.Position{{Message: 39, Block: 0}}, anthropic.Breakpoints(req),
		"zero breakpoints still marks the last block")

	req.Cache.Breakpoints = 10
	c.EqDeep([]anthropic.Position{{Message: 9, Block: 0}, {Message: 24, Block: 0}, {Message: 39, Block: 0}},
		anthropic.Breakpoints(req),
		"the API allows at most three message breakpoints, one every lookbackStride blocks")
}

// TestBreakpointsSkipsThinking pins that a thinking block never carries a
// marker, whether it is typed or kept opaque as redacted_thinking: the API
// refuses cache_control on one, so the marker goes to the cacheable block
// before it.
func TestBreakpointsSkipsThinking(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block msg.Block
	}{
		{"typed thinking", msg.Thinking{
			Text: "hmm",
			Sig:  msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"},
		}},
		{"redacted thinking", msg.Opaque{
			Provider: msg.DialectAnthropic,
			Raw:      json.RawMessage(`{"type":"redacted_thinking","data":"redacted"}`),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			req := &msg.Request{
				Messages: []msg.Message{
					{Role: msg.Assistant, Content: []msg.Block{
						msg.Text{Text: "answer"},
						tc.block,
					}},
				},
				Cache: msg.CachePolicy{MessagesTTL: msg.Cache5m, Breakpoints: 1},
			}
			c.EqDeep([]anthropic.Position{{Message: 0, Block: 0}}, anthropic.Breakpoints(req),
				"a %s block must never carry a marker", tc.name)
		})
	}
}

// TestBreakpointsSkipDroppedBlocks pins that placement walks the same filtered
// view Encode sends. A foreign Opaque block is removed by the foreign-block
// rule, so a marker on it would be removed with it and the turn would ship with
// no marker at all; the marker belongs on the last block that survives.
func TestBreakpointsSkipDroppedBlocks(t *testing.T) {
	foreign := msg.Opaque{
		Provider: msg.DialectResponsesAPI,
		Raw:      json.RawMessage(`{"type":"reasoning_x","encrypted_content":"opaque"}`),
	}

	t.Run("marker moves to the surviving block", func(t *testing.T) {
		c := assert.NewAborting(t)
		req := &msg.Request{
			Model:     "test-model",
			MaxTokens: 64,
			Messages: []msg.Message{
				{Role: msg.User, Content: []msg.Block{msg.Text{Text: "q"}}},
				{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "a"}, foreign}},
			},
			Cache: msg.CachePolicy{MessagesTTL: msg.Cache5m, Breakpoints: 1},
		}
		c.EqDeep([]anthropic.Position{{Message: 1, Block: 0}}, anthropic.Breakpoints(req),
			"the marker must land on the block that survives the foreign-block rule")

		body, err := anthropic.Encode(req, msg.EncodeOptions{})
		c.NoError(err, "encode")
		content := messageContent(t, body, 1)
		c.Len(content, 1, "the foreign opaque block must be dropped")
		c.HasKey(content[0], "cache_control", "the surviving block must carry the marker")
	})

	t.Run("no marker when nothing survives", func(t *testing.T) {
		c := assert.NewAborting(t)
		req := &msg.Request{
			Model:     "test-model",
			MaxTokens: 64,
			Messages: []msg.Message{
				{Role: msg.User, Content: []msg.Block{msg.Text{Text: "q"}}},
				{Role: msg.Assistant, Content: []msg.Block{foreign}},
			},
			Cache: msg.CachePolicy{MessagesTTL: msg.Cache5m, Breakpoints: 1},
		}
		c.EqDeep([]anthropic.Position{{Message: 0, Block: 0}}, anthropic.Breakpoints(req),
			"only the surviving text block is cacheable")

		body, err := anthropic.Encode(req, msg.EncodeOptions{})
		c.NoError(err, "encode")
		c.Empty(messageContent(t, body, 1), "the message holds nothing but the dropped block")
	})
}

// longRequest is a request with enough messages for the lookback stride to
// place more than one marker.
func longRequest() *msg.Request {
	req := &msg.Request{
		Model:     "test-model",
		MaxTokens: 64,
		Cache:     msg.CachePolicy{SystemTTL: msg.Cache5m, MessagesTTL: msg.Cache5m, Breakpoints: 3},
	}
	for i := 0; i < 40; i++ {
		role := msg.User
		if i%2 == 1 {
			role = msg.Assistant
		}
		req.Messages = append(req.Messages, msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: "m"}}})
	}
	return req
}

// corpusCase returns the named corpus case.
func corpusCase(t *testing.T, name string) msgtest.Case {
	t.Helper()
	for _, tc := range msgtest.Corpus() {
		if tc.Name == name {
			return tc
		}
	}
	t.Fatalf("the corpus has no case named %q", name)
	return msgtest.Case{}
}

// goldenMarkers returns the positions of the blocks an encoded golden marks.
func goldenMarkers(t *testing.T, name string) []anthropic.Position {
	t.Helper()
	c := assert.NewAborting(t)
	var wire struct {
		Messages []struct {
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	c.NoError(json.Unmarshal(msgtest.Golden(filepath.Join("golden", name+".anthropic.json")), &wire), "read golden %s", name)
	var out []anthropic.Position
	for i, m := range wire.Messages {
		for j, b := range m.Content {
			if _, ok := b["cache_control"]; ok {
				out = append(out, anthropic.Position{Message: i, Block: j})
			}
		}
	}
	return out
}
