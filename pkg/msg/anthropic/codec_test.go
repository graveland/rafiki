package anthropic_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// pngBase64 is a one-pixel PNG, the payload of every image fixture.
const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// fixture is what one testdata file must decode to: the block kinds it exists to
// pin, in order, or — for the deliberately untyped fixtures — the fact that
// every one of its blocks must come back Opaque.
type fixture struct {
	kinds  []string
	opaque bool
}

// fixtures names the expectation of every file in testdata. A file missing here
// fails TestRoundTripFaithful, so a new fixture has to state what it proves.
//
// The two opaque entries are the deliberate half of the split: a block the typed
// form cannot reproduce must be kept verbatim, and those are the fixtures that a
// canon-only round trip would go on passing even if every block had quietly
// collapsed to Opaque.
var fixtures = map[string]fixture{
	"text.json":                       {kinds: []string{"text"}},
	"text-cache-control.json":         {kinds: []string{"text"}},
	"multi-text.json":                 {kinds: []string{"text", "text"}},
	"string-content.json":             {kinds: []string{"text"}},
	"thinking-anthropic.json":         {kinds: []string{"thinking"}},
	"thinking-responses-api.json":     {kinds: []string{"thinking"}},
	"thinking-responses-chatgpt.json": {kinds: []string{"thinking"}},
	"thinking-responses-legacy.json":  {kinds: []string{"thinking"}},
	"tool-use.json":                   {kinds: []string{"text", "tool_use"}},
	"tool-result-string.json":         {kinds: []string{"tool_result"}},
	"tool-result-error.json":          {kinds: []string{"tool_result"}},
	"tool-result-not-error.json":      {kinds: []string{"tool_result"}},
	"tool-result-nested-image.json":   {kinds: []string{"tool_result"}},
	"image.json":                      {kinds: []string{"image"}},
	"claude-captured-mixed.json":      {kinds: []string{"text", "thinking", "tool_use"}},
	"fundi-row.json":                  {kinds: []string{"text", "tool_use", "tool_result"}},

	// Deliberately opaque fixtures.
	"opaque-redacted-thinking.json":   {opaque: true},
	"opaque-text-with-citations.json": {opaque: true},
}

// TestDecodeTypedBlocks pins the happy path for each typed kind: a block the
// codec can reproduce comes back typed, not Opaque.
func TestDecodeTypedBlocks(t *testing.T) {
	c := assert.NewAborting(t)

	png, err := base64.StdEncoding.DecodeString(pngBase64)
	c.NoError(err, "decode the fixture image")

	for _, tc := range []struct {
		name    string
		content string
		want    msg.Block
	}{
		{
			name:    "text",
			content: `[{"type":"text","text":"hi"}]`,
			want:    msg.Text{Text: "hi"},
		},
		{
			name:    "thinking",
			content: `[{"type":"thinking","thinking":"hmm","signature":"EqQ-anthropic-sig"}]`,
			want:    msg.Thinking{Text: "hmm", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "EqQ-anthropic-sig"}},
		},
		{
			name:    "tool_use",
			content: `[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a","n":1}}]`,
			want: msg.ToolUse{
				ID:    "toolu_1",
				Name:  "read_file",
				Input: json.RawMessage(`{"path":"a","n":1}`),
			},
		},
		{
			name:    "tool_result",
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"done"}]}]`,
			want:    msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: "done"}}},
		},
		{
			name:    "tool_result_error",
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"boom"}],"is_error":true}]`,
			want:    msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: "boom"}}, IsError: true},
		},
		{
			name:    "image",
			content: `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngBase64 + `"}}]`,
			want:    msg.Image{MediaType: "image/png", Data: png},
		},
	} {
		m, err := anthropic.Decode("user", []byte(tc.content))
		c.NoError(err, "%s: Decode", tc.name)
		c.Eq(1, len(m.Content), "%s: one block", tc.name)
		c.EqDeep(tc.want, m.Content[0], "%s must decode to its typed block", tc.name)
	}
}

// TestDecodeFallsBackToOpaque pins losslessness: a block carrying anything the
// codec cannot reproduce is kept byte-identically rather than trimmed.
func TestDecodeFallsBackToOpaque(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{
			name: "text with citations",
			raw:  `{"type":"text","text":"cited","citations":[{"type":"char_location","cited_text":"x","document_index":0,"document_title":"t","start_char_index":0,"end_char_index":1}]}`,
		},
		{
			name: "image with a url source",
			raw:  `{"type":"image","source":{"type":"url","url":"https://example.test/cat.png"}}`,
		},
		{
			name: "redacted_thinking",
			raw:  `{"type":"redacted_thinking","data":"EvEFCkgIBRABGAIiQOpaque"}`,
		},
		{
			name: "server_tool_use",
			raw:  `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"rafiki"}}`,
		},
		{
			name: "unknown type",
			raw:  `{"type":"future_block_type","payload":{"a":1}}`,
		},
		{
			name: "tool_use with a caller",
			raw:  `{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a"},"caller":{"type":"direct"}}`,
		},
	} {
		m, err := anthropic.Decode("user", []byte("["+tc.raw+"]"))
		c.NoError(err, "%s: Decode", tc.name)
		c.Eq(1, len(m.Content), "%s: one block", tc.name)

		opaque, ok := m.Content[0].(msg.Opaque)
		c.True(ok, "%s must fall back to Opaque, got %T", tc.name, m.Content[0])
		c.Eq(msg.DialectAnthropic, opaque.Provider, "%s: Provider", tc.name)
		c.Eq(tc.raw, string(opaque.Raw), "%s: Raw must be byte-identical", tc.name)
	}
}

// TestDecodeMalformedBlockFallsBackToOpaque pins totality: a block whose fields
// are the wrong JSON type cannot be typed losslessly, but it must still decode —
// as its own original bytes — because one odd block must not fail a whole
// conversation's walk.
func TestDecodeMalformedBlockFallsBackToOpaque(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "type is a number", raw: `{"type":5,"text":"hi"}`},
		{name: "text is a number", raw: `{"type":"text","text":5}`},
		{name: "thinking is an object", raw: `{"type":"thinking","thinking":{"nope":true},"signature":"sig"}`},
		{name: "signature is a number", raw: `{"type":"thinking","thinking":"hmm","signature":5}`},
		{name: "tool_use id is a number", raw: `{"type":"tool_use","id":5,"name":"read_file","input":{}}`},
		{name: "tool_result is_error is a string", raw: `{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok"}],"is_error":"yes"}`},
		{name: "tool_result content is a number", raw: `{"type":"tool_result","tool_use_id":"toolu_1","content":5}`},
		{name: "image data is a number", raw: `{"type":"image","source":{"type":"base64","media_type":"image/png","data":5}}`},
		{name: "block is a number", raw: `5`},
		{name: "block is a string", raw: `"just a string"`},
		{name: "block is an array", raw: `[1,2,3]`},
		{name: "block is null", raw: `null`},
	} {
		m, err := anthropic.Decode("user", []byte("["+tc.raw+"]"))
		c.NoError(err, "%s: a malformed block must decode, not error", tc.name)
		c.Eq(1, len(m.Content), "%s: one block", tc.name)

		opaque, ok := m.Content[0].(msg.Opaque)
		c.True(ok, "%s must fall back to Opaque, got %T", tc.name, m.Content[0])
		c.Eq(msg.DialectAnthropic, opaque.Provider, "%s: Provider", tc.name)
		c.Eq(tc.raw, string(opaque.Raw), "%s: Raw must be byte-identical", tc.name)
	}

	// What stays an error: content that is not a JSON array or string at all,
	// and content that is not JSON.
	_, err := anthropic.Decode("user", []byte(`{"type":"text","text":"hi"}`))
	c.Error(err, "a top-level object is not message content")
	_, err = anthropic.Decode("user", []byte(`[{"type":"text",`))
	c.Error(err, "truncated JSON is an error")
	_, err = anthropic.Decode("user", []byte(`7`))
	c.Error(err, "a top-level number is not message content")
}

// TestDecodeSignatureDialect pins the reasoning-signature classification: a
// signature is only replayable to the backend that minted it, so the dialect has
// to be read off the signature itself.
func TestDecodeSignatureDialect(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name string
		sig  string
		want msg.Dialect
	}{
		{name: "anthropic", sig: "EqQBCkgIBRABGAIiQ-anthropic", want: msg.DialectAnthropic},
		{name: "responses api", sig: "openai-responses:api:rs_01:gAAAA-api", want: msg.DialectResponsesAPI},
		{name: "responses chatgpt", sig: "openai-responses:chatgpt:rs_02:gAAAA-chat", want: msg.DialectResponsesChatGPT},
		{name: "responses with no backend", sig: "openai-responses:rs_03:gAAAA-legacy", want: ""},
		{name: "anthropic sig containing the word responses", sig: "sig-openai-responses-elsewhere", want: msg.DialectAnthropic},
	} {
		content := `[{"type":"thinking","thinking":"r","signature":` + quoteJSON(c, tc.sig) + `}]`
		m, err := anthropic.Decode("assistant", []byte(content))
		c.NoError(err, "%s: Decode", tc.name)
		c.Eq(1, len(m.Content), "%s: one block", tc.name)

		thinking, ok := m.Content[0].(msg.Thinking)
		c.True(ok, "%s must decode to Thinking, got %T", tc.name, m.Content[0])
		c.Eq(tc.want, thinking.Sig.Dialect, "%s: Dialect", tc.name)
		c.Eq(tc.sig, thinking.Sig.Value, "%s: Value must be byte-identical", tc.name)
	}
}

// TestDecodeEquivalences pins the shorthand and transport forms that decode to
// typed blocks: a string content, a string tool result, an explicit
// is_error:false, and a cache_control hint.
func TestDecodeEquivalences(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name    string
		content string
		want    msg.Message
	}{
		{
			name:    "bare string content",
			content: `"a shorthand prompt"`,
			want:    msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "a shorthand prompt"}}},
		},
		{
			name:    "string tool_result content",
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":"the result"}]`,
			want:    msg.Message{Role: msg.User, Content: []msg.Block{msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: "the result"}}}}},
		},
		{
			name:    "explicit is_error false",
			content: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok"}],"is_error":false}]`,
			want:    msg.Message{Role: msg.User, Content: []msg.Block{msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: "ok"}}}}},
		},
		{
			name:    "text with cache_control",
			content: `[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}]`,
			want:    msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "cached"}}},
		},
		{
			name:    "null content",
			content: `null`,
			want:    msg.Message{Role: msg.User},
		},
		{
			name:    "empty content",
			content: `   `,
			want:    msg.Message{Role: msg.User},
		},
	} {
		m, err := anthropic.Decode("user", []byte(tc.content))
		c.NoError(err, "%s: Decode", tc.name)
		c.EqDeep(tc.want, m, "%s must decode without falling back to Opaque", tc.name)
	}

	_, err := anthropic.Decode("system", []byte(`"x"`))
	c.EqualError(err, `msg: unknown role "system"`, "an unknown role is an error, not a guess")
}

// TestRoundTripFaithful pins losslessness over realistic stored rows, and pins
// what each fixture is supposed to be: decoding and re-encoding a fixture must
// not change its canonical identity, AND it must yield the block kinds that
// fixture exists to prove. The second half is what stops a canon-only round trip
// from passing while every block has quietly collapsed to Opaque.
func TestRoundTripFaithful(t *testing.T) {
	c := assert.NewAborting(t)

	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	c.NoError(err, "glob testdata")
	c.True(len(paths) >= 12, "want at least 12 fixtures, found %d", len(paths))
	c.Eq(len(fixtures), len(paths), "every fixture must declare its expectation in fixtures")

	for _, path := range paths {
		name := filepath.Base(path)
		original, err := os.ReadFile(path)
		c.NoError(err, "read %s", name)

		want, declared := fixtures[name]
		c.True(declared, "%s has no declared expectation", name)

		m, err := anthropic.Decode("user", original)
		c.NoError(err, "%s: Decode", name)

		var kinds []string
		for i, block := range m.Content {
			_, isOpaque := block.(msg.Opaque)
			if want.opaque {
				c.True(isOpaque, "%s block %d belongs to a deliberately untyped fixture and must be Opaque, got %T", name, i, block)
				continue
			}
			c.False(isOpaque, "%s block %d must decode to a typed block, not Opaque", name, i)
			kinds = append(kinds, kindOf(block))
		}
		if !want.opaque {
			c.EqDeep(want.kinds, kinds, "%s: decoded block kinds", name)
		}

		encoded, err := anthropic.EncodeContent(m)
		c.NoError(err, "%s: EncodeContent", name)

		assertCanonEqual(c, original, encoded, name)
	}
}

// TestEncodeContentHandBuilt pins the outbound direction on messages built from
// msg values rather than decoded from stored bytes, which is how later work
// constructs nodes.
func TestEncodeContentHandBuilt(t *testing.T) {
	c := assert.NewAborting(t)

	t.Run("empty tool_use input encodes as an object", func(t *testing.T) {
		cc := c.Sub(t)
		m := msg.Message{Role: msg.Assistant, Content: []msg.Block{
			msg.ToolUse{ID: "toolu_1", Name: "noargs"},
		}}

		encoded, err := anthropic.EncodeContent(m)
		cc.NoError(err, "EncodeContent")
		cc.Eq(`[{"type":"tool_use","id":"toolu_1","name":"noargs","input":{}}]`, string(encoded),
			"an empty input must still be a JSON object, not null and not absent")

		back, err := anthropic.Decode("assistant", encoded)
		cc.NoError(err, "Decode")
		cc.EqDeep(
			[]msg.Block{msg.ToolUse{ID: "toolu_1", Name: "noargs", Input: json.RawMessage(`{}`)}},
			back.Content, "the block must come back typed with its empty input",
		)
	})

	for _, tc := range []struct {
		name     string
		content  []msg.Block
		wantJSON string
		wantBack []msg.Block
	}{
		{
			name:     "nil tool_result content",
			content:  nil,
			wantJSON: `[{"type":"tool_result","tool_use_id":"toolu_1"}]`,
			wantBack: []msg.Block{msg.ToolResult{ToolUseID: "toolu_1"}},
		},
		{
			name:     "empty tool_result content",
			content:  []msg.Block{},
			wantJSON: `[{"type":"tool_result","tool_use_id":"toolu_1","content":[]}]`,
			wantBack: []msg.Block{msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{}}},
		},
	} {
		m := msg.Message{Role: msg.User, Content: []msg.Block{
			msg.ToolResult{ToolUseID: "toolu_1", Content: tc.content},
		}}

		encoded, err := anthropic.EncodeContent(m)
		c.NoError(err, "%s: EncodeContent", tc.name)
		c.Eq(tc.wantJSON, string(encoded), "%s: encoded JSON", tc.name)

		back, err := anthropic.Decode("user", encoded)
		c.NoError(err, "%s: Decode", tc.name)
		c.EqDeep(tc.wantBack, back.Content, "%s must decode back to its typed block", tc.name)
	}

	t.Run("a hand-built image round-trips byte for byte", func(t *testing.T) {
		cc := c.Sub(t)
		image := msg.Image{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 0xff}}
		m := msg.Message{Role: msg.User, Content: []msg.Block{image}}

		encoded, err := anthropic.EncodeContent(m)
		cc.NoError(err, "EncodeContent")
		cc.Eq(
			`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+
				base64.StdEncoding.EncodeToString(image.Data)+`"}}]`,
			string(encoded), "the image must be base64 of its bytes",
		)

		back, err := anthropic.Decode("user", encoded)
		cc.NoError(err, "Decode")
		cc.EqDeep([]msg.Block{image}, back.Content, "image bytes must survive the round trip")
	})
}

// TestToParamMatchesSDKDecode pins the outbound conversion against the SDK's own
// decoding of the same bytes: ToParam must produce what the API client would.
func TestToParamMatchesSDKDecode(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		fixture string
		role    string
	}{
		{fixture: "text.json", role: "user"},
		{fixture: "text-cache-control.json", role: "user"},
		{fixture: "multi-text.json", role: "user"},
		{fixture: "thinking-anthropic.json", role: "assistant"},
		{fixture: "thinking-responses-api.json", role: "assistant"},
		{fixture: "thinking-responses-chatgpt.json", role: "assistant"},
		{fixture: "thinking-responses-legacy.json", role: "assistant"},
		{fixture: "tool-use.json", role: "assistant"},
		{fixture: "tool-result-string.json", role: "user"},
		{fixture: "tool-result-error.json", role: "user"},
		{fixture: "tool-result-not-error.json", role: "user"},
		{fixture: "tool-result-nested-image.json", role: "user"},
		{fixture: "image.json", role: "user"},
		{fixture: "claude-captured-mixed.json", role: "assistant"},
		{fixture: "fundi-row.json", role: "assistant"},
	} {
		content, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
		c.NoError(err, "%s: read", tc.fixture)

		m, err := anthropic.Decode(tc.role, content)
		c.NoError(err, "%s: Decode", tc.fixture)

		param, err := anthropic.ToParam(m)
		c.NoError(err, "%s: ToParam", tc.fixture)
		got, err := json.Marshal(param.Content)
		c.NoError(err, "%s: marshal ToParam", tc.fixture)

		want := sdkDecode(c, tc.role, content)
		wantJSON, err := json.Marshal(want.Content)
		c.NoError(err, "%s: marshal the SDK's own decode", tc.fixture)

		assertCanonEqual(c, wantJSON, got, tc.fixture)
	}
}

// TestOpaqueToParamVerbatim pins that a block the codec kept verbatim reaches
// the API unchanged.
func TestOpaqueToParamVerbatim(t *testing.T) {
	c := assert.NewAborting(t)

	const raw = `{"type":"redacted_thinking","data":"EvEFCkgIBRABGAIiQOpaque"}`

	m, err := anthropic.Decode("assistant", []byte("["+raw+"]"))
	c.NoError(err, "Decode")
	c.Eq(1, len(m.Content), "one block")
	_, ok := m.Content[0].(msg.Opaque)
	c.True(ok, "redacted_thinking must decode to Opaque, got %T", m.Content[0])

	param, err := anthropic.ToParam(m)
	c.NoError(err, "ToParam")
	got, err := json.Marshal(param.Content)
	c.NoError(err, "marshal ToParam")

	assertCanonEqual(c, []byte("["+raw+"]"), got, "an Opaque block must survive ToParam")
}

// TestFromParamRoundTrip pins the inbound conversion: the SDK's own form decodes
// to the same native message as the stored bytes do.
func TestFromParamRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)

	content, err := os.ReadFile(filepath.Join("testdata", "claude-captured-mixed.json"))
	c.NoError(err, "read the fixture")

	want, err := anthropic.Decode("assistant", content)
	c.NoError(err, "Decode")

	fromParam, err := anthropic.FromParam(sdkDecode(c, "assistant", content))
	c.NoError(err, "FromParam")
	c.EqDeep(want, fromParam, "FromParam must agree with Decode")
}

// kindOf names a block's kind, for the fixture expectations.
func kindOf(block msg.Block) string {
	switch block.(type) {
	case msg.Text:
		return "text"
	case msg.Thinking:
		return "thinking"
	case msg.ToolUse:
		return "tool_use"
	case msg.ToolResult:
		return "tool_result"
	case msg.Image:
		return "image"
	case msg.Opaque:
		return "opaque"
	}
	return "unknown"
}

// sdkDecode is the SDK's own decoding of stored content: the harness the
// capture path uses, one message at a time.
func sdkDecode(c *assert.C, role string, content []byte) sdk.MessageParam {
	c.Helper()
	wire := make([]byte, 0, len(content)+len(role)+20)
	wire = append(wire, `{"role":`...)
	wire = append(wire, quoteJSON(c, role)...)
	wire = append(wire, `,"content":`...)
	wire = append(wire, content...)
	wire = append(wire, '}')

	var param sdk.MessageParam
	c.NoError(json.Unmarshal(wire, &param), "the SDK must decode %s", content)
	return param
}

func quoteJSON(c *assert.C, s string) string {
	c.Helper()
	out, err := json.Marshal(s)
	c.NoError(err, "marshal %q", s)
	return string(out)
}

func assertCanonEqual(c *assert.C, want, got []byte, what string) {
	c.Helper()
	c.True(bytes.Equal(canonOf(c, want), canonOf(c, got)),
		"%s: want canon of %s, got %s", what, want, got)
}

func canonOf(c *assert.C, content []byte) []byte {
	c.Helper()
	normalized, err := anthropic.Normalize(content)
	c.NoError(err, "Normalize(%s)", content)
	canon, err := convtree.Canon(msg.User.String(), normalized)
	c.NoError(err, "Canon(%s)", content)
	return canon
}
