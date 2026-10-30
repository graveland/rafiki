// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"

	"github.com/multigres/testkit/assert"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite msgtest goldens")

// TestGoldenCorpus records what today's request builders and response decoders
// produce, so the native codecs that replace them can be measured against it.
// Nothing here changes production behaviour: every request body is built by the
// same function the real sender calls.
//
// A decoded golden is the wire's meaning expressed in native blocks. An
// Anthropic body's content is decoded by msganthropic.Decode — the residue-aware
// stored-message decoder — so a caller, a citation, or any key the native core
// does not model survives into the golden as Ext. The other fixtures are reduced
// to the same native blocks without the SDK's params round-trip, which drops a
// tool_use's caller and flattens nested unions (a web_search_result array comes
// back as the error variant).
func TestGoldenCorpus(t *testing.T) {
	t.Run("requests", func(t *testing.T) {
		for _, tc := range msgtest.Corpus() {
			tc := tc
			t.Run(tc.Name, func(t *testing.T) {
				ck := assert.NewAborting(t)
				params := corpusParams(t, tc)
				if len(tc.Extra) > 0 {
					params.SetExtraFields(tc.Extra)
				}

				anthropicBody, err := json.Marshal(withoutForeignThinking(params, false))
				ck.NoError(err, "marshal anthropic body")
				writeGolden(t, tc.Name+".anthropic.json", anthropicBody)

				chatBody, err := buildOpenAIChatRequest(params, false)
				ck.NoError(err, "build openai chat body")
				writeGolden(t, tc.Name+".openaichat.json", chatBody)

				chatStreamBody, err := buildOpenAIChatRequest(params, true)
				ck.NoError(err, "build openai chat stream body")
				writeGolden(t, tc.Name+".openaichat-stream.json", chatStreamBody)

				apiBody, err := buildResponsesRequestBody(withoutForeignResponses(params, ResponsesModeAPI), responsesBuildOptions{
					stream:    false,
					sessionID: "sess-1",
				})
				ck.NoError(err, "build responses api body")
				writeGolden(t, tc.Name+".responses-api.json", apiBody)

				chatGPTBody, err := buildResponsesRequestBody(withoutForeignResponses(params, ResponsesModeChatGPT), responsesBuildOptions{
					chatGPT:    true,
					stream:     true,
					sessionID:  "sess-1",
					devMessage: chatGPTInstructionsAsDeveloperMessage,
				})
				ck.NoError(err, "build responses chatgpt body")
				writeGolden(t, tc.Name+".responses-chatgpt.json", chatGPTBody)
			})
		}
	})

	t.Run("responses", func(t *testing.T) {
		decoded := make(map[string]decodedResponse, len(decodedFixtures))
		for _, fx := range decodedFixtures {
			fx := fx
			t.Run(fx.name, func(t *testing.T) {
				rec := decodeFixture(t, fx)
				if rec.wireBlocks > 0 && !hasContent(t, rec.Content) {
					t.Fatalf("%s: the wire carried %d block(s) but the golden records no content", fx.name, rec.wireBlocks)
				}
				decoded[fx.name] = rec
				writeGolden(t, fx.name+".decoded.json", rec.golden(t))
			})
		}
		for _, pair := range anthropicPairs {
			jsonSide, ok := decoded[pair[0]]
			if !ok {
				t.Fatalf("%s was not decoded", pair[0])
			}
			sseSide, ok := decoded[pair[1]]
			if !ok {
				t.Fatalf("%s was not decoded", pair[1])
			}
			compareCanonical(t, pair[0]+" vs "+pair[1], jsonSide.Content, sseSide.Content)
		}
	})
}

// corpusParams assembles the SDK params the client would build for tc: the
// conversation defaults, msganthropic.ToParam over the messages, and
// Conversation.assemble's cache breakpoints.
func corpusParams(t *testing.T, tc msgtest.Case) anthropic.MessageNewParams {
	t.Helper()
	ck := assert.NewAborting(t)
	req := tc.Req

	system := make([]anthropic.TextBlockParam, 0, len(req.System))
	for _, s := range req.System {
		system = append(system, anthropic.TextBlockParam{Text: s.Text})
	}

	tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
	for _, td := range req.Tools {
		var schema anthropic.ToolInputSchemaParam
		ck.NoError(json.Unmarshal(td.InputSchema, &schema), "unmarshal %s input schema", td.Name)
		tool := anthropic.ToolParam{Name: td.Name, InputSchema: schema}
		if td.Description != "" {
			tool.Description = anthropic.String(td.Description)
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &tool})
	}

	reqMsgs := make([]Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		param, err := msganthropic.ToParam(m)
		ck.NoError(err, "convert %s message", m.Role)
		reqMsgs = append(reqMsgs, param)
	}

	policy := CachePolicy{
		SystemTTL:   CacheTTL(req.Cache.SystemTTL),
		MessagesTTL: CacheTTL(req.Cache.MessagesTTL),
		Breakpoints: req.Cache.Breakpoints,
	}
	conv := &Conversation{cfg: convConfig{
		model:          req.Model,
		system:         system,
		temperature:    req.Temperature,
		topK:           req.TopK,
		maxTokens:      req.MaxTokens,
		thinkingBudget: req.ThinkingBudget,
		cache:          &policy,
	}}
	return conv.assemble(reqMsgs, sendConfig{
		maxTokens:  req.MaxTokens,
		tools:      tools,
		toolChoice: req.ForceTool,
	})
}

// withoutForeignResponses mirrors the strip a Responses sender applies before
// building: only its own dialect's reasoning survives.
func withoutForeignResponses(params anthropic.MessageNewParams, mode ResponsesSignatureMode) anthropic.MessageNewParams {
	params.Messages = StripForeignResponsesThinking(params.Messages, mode)
	return params
}

// fixtureDecoder names the path a response fixture is decoded through.
type fixtureDecoder string

const (
	decoderAnthropicJSON  fixtureDecoder = "anthropic-json"
	decoderAnthropicSSE   fixtureDecoder = "anthropic-sse"
	decoderOpenAIChatJSON fixtureDecoder = "openaichat-json"
	decoderOpenAIChatSSE  fixtureDecoder = "openaichat-sse"
	decoderResponsesSSE   fixtureDecoder = "responses-sse"
)

type decodedFixture struct {
	name    string
	decoder fixtureDecoder
	mode    ResponsesSignatureMode
}

// decodedFixtures is every recorded response in the corpus. The Responses
// recordings are the ones the README in pkg/llm/testdata/responses describes:
// turn* is the ChatGPT dialect, std_* the plain API.
var decodedFixtures = []decodedFixture{
	{name: "anthropic-tooluse.json", decoder: decoderAnthropicJSON},
	{name: "anthropic-tooluse.sse", decoder: decoderAnthropicSSE},
	{name: "anthropic-citations.json", decoder: decoderAnthropicJSON},
	{name: "anthropic-citations.sse", decoder: decoderAnthropicSSE},
	{name: "anthropic-caller.json", decoder: decoderAnthropicJSON},
	{name: "anthropic-caller.sse", decoder: decoderAnthropicSSE},
	{name: "anthropic-servertool.json", decoder: decoderAnthropicJSON},
	{name: "anthropic-servertool.sse", decoder: decoderAnthropicSSE},
	{name: "openaichat-tooluse.json", decoder: decoderOpenAIChatJSON},
	{name: "openaichat-tooluse.sse", decoder: decoderOpenAIChatSSE},
	{name: "openaichat-nousage-details.json", decoder: decoderOpenAIChatJSON},
	{name: "responses-turn1.response.sse", decoder: decoderResponsesSSE, mode: ResponsesModeChatGPT},
	{name: "responses-turn2.response.sse", decoder: decoderResponsesSSE, mode: ResponsesModeChatGPT},
	{name: "responses-std_turn1.response.sse", decoder: decoderResponsesSSE, mode: ResponsesModeAPI},
	{name: "responses-std_turn2.response.sse", decoder: decoderResponsesSSE, mode: ResponsesModeAPI},
}

// anthropicPairs are the fixtures whose final blocks must decode identically
// whether they arrived streamed or whole.
var anthropicPairs = [][2]string{
	{"anthropic-tooluse.json", "anthropic-tooluse.sse"},
	{"anthropic-citations.json", "anthropic-citations.sse"},
	{"anthropic-caller.json", "anthropic-caller.sse"},
	{"anthropic-servertool.json", "anthropic-servertool.sse"},
}

func decodeFixture(t *testing.T, fx decodedFixture) decodedResponse {
	t.Helper()
	body := msgtest.Golden(filepath.Join("responses", fx.name))
	switch fx.decoder {
	case decoderAnthropicJSON:
		return decodeAnthropicBody(t, body)
	case decoderAnthropicSSE:
		acc, starts := accumulateStream(t, anthropicStream(body))
		return decodedResponse{
			ID:         acc.ID,
			Model:      acc.Model,
			StopReason: string(acc.StopReason),
			Provider:   ProviderOf(acc),
			Usage:      usageOf(acc),
			Content:    nativeContent(t, wireContent(t, starts, acc)),
			wireBlocks: len(acc.Content),
		}
	case decoderOpenAIChatJSON:
		ck := assert.NewAborting(t)
		var wire openAIChatResponse
		ck.NoError(json.Unmarshal(body, &wire), "decode %s", fx.name)
		resp, err := parseOpenAIChatResponse(&wire)
		ck.NoError(err, "parse %s", fx.name)
		return openAIRecord(t, resp)
	case decoderOpenAIChatSSE:
		acc, _ := accumulateStream(t, openAIChatStream(body))
		return openAIRecord(t, acc)
	case decoderResponsesSSE:
		acc, _ := accumulateStream(t, responsesStream(body, fx.mode))
		return responsesRecord(t, acc)
	}
	t.Fatalf("unknown decoder %q for %s", fx.decoder, fx.name)
	return decodedResponse{}
}

// decodeAnthropicBody records a whole (non-streamed) Anthropic body: its content
// array goes through the stored-message decoder, never through SDK params.
func decodeAnthropicBody(t *testing.T, body []byte) decodedResponse {
	t.Helper()
	ck := assert.NewAborting(t)
	var fixture struct {
		ID         string                `json:"id"`
		Model      string                `json:"model"`
		StopReason string                `json:"stop_reason"`
		Provider   string                `json:"provider"`
		Usage      anthropicFixtureUsage `json:"usage"`
		Content    json.RawMessage       `json:"content"`
	}
	ck.NoError(json.Unmarshal(body, &fixture), "decode anthropic body")
	return decodedResponse{
		ID:         fixture.ID,
		Model:      fixture.Model,
		StopReason: fixture.StopReason,
		Provider:   fixture.Provider,
		Usage:      fixture.Usage.decoded(),
		Content:    nativeContent(t, fixture.Content),
		wireBlocks: contentLen(t, fixture.Content),
	}
}

type anthropicFixtureUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u anthropicFixtureUsage) decoded() decodedUsage {
	return decodedUsage(u)
}

// openAIRecord builds the native blocks from the parsed message's typed fields.
// A Chat Completions block is a constructed (or accumulated) union with no raw
// JSON of its own, so ContentBlockUnion.ToParam/AsAny would read an empty value
// and record nothing.
func openAIRecord(t *testing.T, resp *anthropic.Message) decodedResponse {
	t.Helper()
	ck := assert.NewAborting(t)
	blocks := make([]msg.Block, 0, len(resp.Content))
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			blocks = append(blocks, msg.Text{Text: b.Text})
		case "tool_use":
			blocks = append(blocks, msg.ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		default:
			t.Fatalf("openai fixture: unexpected %q content block", b.Type)
		}
	}
	content, err := msganthropic.EncodeContent(msg.Message{Role: msg.Assistant, Content: blocks})
	ck.NoError(err, "encode openai content")
	return decodedResponse{
		ID:         resp.ID,
		Model:      resp.Model,
		StopReason: string(resp.StopReason),
		Provider:   ProviderOf(resp),
		Usage:      usageOf(resp),
		Content:    content,
		wireBlocks: len(resp.Content),
	}
}

// responsesRecord records a Responses stream. The accumulated message is
// rehydrated through JSON first, so every content block carries the raw JSON
// ContentBlockUnion.ToParam reads back; without that, a decoded block converts
// to a zero value.
func responsesRecord(t *testing.T, acc *anthropic.Message) decodedResponse {
	t.Helper()
	ck := assert.NewAborting(t)
	raw, err := json.Marshal(acc)
	ck.NoError(err, "marshal decoded message")
	var rehydrated anthropic.Message
	ck.NoError(json.Unmarshal(raw, &rehydrated), "rehydrate decoded message")
	native, err := msganthropic.FromParam(rehydrated.ToParam())
	ck.NoError(err, "convert rehydrated message")
	content, err := msganthropic.EncodeContent(native)
	ck.NoError(err, "encode rehydrated content")
	return decodedResponse{
		ID:         acc.ID,
		Model:      acc.Model,
		StopReason: string(acc.StopReason),
		Provider:   ProviderOf(acc),
		Usage:      usageOf(acc),
		Content:    content,
		wireBlocks: len(acc.Content),
	}
}

// nativeContent turns a wire content array into native blocks through the
// stored-message decoder.
func nativeContent(t *testing.T, content []byte) json.RawMessage {
	t.Helper()
	ck := assert.NewAborting(t)
	message, err := msganthropic.Decode("assistant", content)
	ck.NoError(err, "decode content")
	out, err := msganthropic.EncodeContent(message)
	ck.NoError(err, "encode content")
	return out
}

// accumulateStream is sendStreamingAttempt's accumulation loop without the
// handler and turn bookkeeping. It also returns each content block's start JSON:
// Message.Accumulate replaces a block's raw JSON with a re-marshalled union on
// content_block_stop, which is lossy for a nested union.
func accumulateStream(t *testing.T, stream *ssestream.Stream[anthropic.MessageStreamEventUnion]) (*anthropic.Message, []json.RawMessage) {
	t.Helper()
	ck := assert.NewAborting(t)
	defer func() { ck.NoError(stream.Close(), "close stream") }()
	acc := anthropic.Message{}
	var starts []json.RawMessage
	for stream.Next() {
		ev := stream.Current()
		FixEmptyToolInput(&ev)
		SanitizeInvalidAccumulatedInput(&acc, ev)
		ck.NoError(acc.Accumulate(ev), "accumulate %s", ev.Type)
		FixAccumulatedEmptyToolInput(&acc, ev)
		backfillDeltaUsage(&acc, ev)
		if ev.Type == "content_block_start" {
			starts = append(starts, json.RawMessage(ev.ContentBlock.RawJSON()))
		}
	}
	ck.NoError(stream.Err(), "stream error")
	return &acc, starts
}

// wireContent rebuilds each block's wire JSON: the start block's own envelope
// (which keeps a caller, provider_metadata, or any other key the core does not
// model) with the streamed fields filled from the accumulated block.
func wireContent(t *testing.T, starts []json.RawMessage, acc *anthropic.Message) json.RawMessage {
	t.Helper()
	ck := assert.NewAborting(t)
	blocks := make([]json.RawMessage, 0, len(starts))
	for i, start := range starts {
		var block map[string]json.RawMessage
		ck.NoError(json.Unmarshal(start, &block), "read start block %d", i)
		if i < len(acc.Content) {
			b := acc.Content[i]
			if b.Text != "" {
				block["text"] = rawString(t, b.Text)
			}
			if b.Thinking != "" {
				block["thinking"] = rawString(t, b.Thinking)
			}
			if b.Signature != "" {
				block["signature"] = rawString(t, b.Signature)
			}
			if len(b.Input) > 0 && !bytes.Equal(b.Input, []byte("{}")) {
				block["input"] = b.Input
			}
			if len(b.Citations) > 0 {
				citations := make([]json.RawMessage, 0, len(b.Citations))
				for _, c := range b.Citations {
					citations = append(citations, json.RawMessage(c.RawJSON()))
				}
				block["citations"] = rawValue(t, citations)
			}
		}
		out, err := json.Marshal(block)
		ck.NoError(err, "marshal block %d", i)
		blocks = append(blocks, out)
	}
	out, err := json.Marshal(blocks)
	ck.NoError(err, "marshal wire content")
	return out
}

func rawString(t *testing.T, s string) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return out
}

func rawValue(t *testing.T, v any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	return out
}

func anthropicStream(body []byte) *ssestream.Stream[anthropic.MessageStreamEventUnion] {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](ssestream.NewDecoder(resp), nil)
}

func openAIChatStream(body []byte) *ssestream.Stream[anthropic.MessageStreamEventUnion] {
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](
		newOpenAISSEDecoder(io.NopCloser(bytes.NewReader(body))), nil)
}

func responsesStream(body []byte, mode ResponsesSignatureMode) *ssestream.Stream[anthropic.MessageStreamEventUnion] {
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](
		newResponsesSSEDecoder(io.NopCloser(bytes.NewReader(body)), defaultResponsesStreamIdleTimeout, responsesMaxLineBytes, mode), nil)
}

func usageOf(m *anthropic.Message) decodedUsage {
	return decodedUsage{
		Input:      m.Usage.InputTokens,
		Output:     m.Usage.OutputTokens,
		CacheRead:  m.Usage.CacheReadInputTokens,
		CacheWrite: m.Usage.CacheCreationInputTokens,
	}
}

// decodedResponse is the canonical shape of a decoded golden.
type decodedResponse struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Provider   string          `json:"provider"`
	Usage      decodedUsage    `json:"usage"`
	Content    json.RawMessage `json:"content"`

	// wireBlocks is how many content blocks the wire carried, so a recording
	// that silently loses them can be caught. It is not part of the golden.
	wireBlocks int
}

type decodedUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

func (r decodedResponse) golden(t *testing.T) []byte {
	t.Helper()
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal decoded golden: %v", err)
	}
	return out
}

// contentLen counts the blocks in a content array.
func contentLen(t *testing.T, content json.RawMessage) int {
	t.Helper()
	var blocks []json.RawMessage
	ck := assert.NewAborting(t)
	ck.NoError(json.Unmarshal(content, &blocks), "read content blocks")
	return len(blocks)
}

// hasContent reports whether a recorded content array carries anything the wire
// did. A lossy recording leaves blocks whose every value is empty — the shape
// ContentBlockUnion.ToParam produces when it reads back a block with no raw
// JSON — so a value beyond the block's own type is what proves the recording
// survived.
func hasContent(t *testing.T, content json.RawMessage) bool {
	t.Helper()
	var blocks []map[string]any
	ck := assert.NewAborting(t)
	ck.NoError(json.Unmarshal(content, &blocks), "read content blocks")
	for _, block := range blocks {
		for key, value := range block {
			if key == "type" {
				continue
			}
			if s, ok := value.(string); ok {
				if s != "" {
					return true
				}
				continue
			}
			if value != nil {
				return true
			}
		}
	}
	return false
}

func writeGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	path := msgtest.GoldenPath(filepath.Join("golden", name))
	if *updateGolden {
		pretty, err := prettyJSON(body)
		if err != nil {
			t.Fatalf("pretty-print golden %s: %v", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, pretty, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update-golden to record it)", name, err)
	}
	compareCanonical(t, name, want, body)
}

func prettyJSON(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// compareCanonical compares two JSON documents by value, never by bytes: key
// order and whitespace are not part of what a codec promises.
func compareCanonical(t *testing.T, name string, want, got []byte) {
	t.Helper()
	ck := assert.NewAborting(t)
	var w, g any
	ck.NoError(json.Unmarshal(want, &w), "%s: golden is not JSON", name)
	ck.NoError(json.Unmarshal(got, &g), "%s: built body is not JSON", name)
	ck.EqDeep(w, g, "%s does not match its golden (only an intended change justifies -update-golden)", name)
}
