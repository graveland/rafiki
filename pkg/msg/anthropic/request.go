package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/msg"
)

// Encode turns a native request into an Anthropic Messages request body.
//
// The body is the JSON form of the SDK's own params, because that is what the
// API accepts: the same key set, the same values, the same nesting. Everything
// opts adds — a stream flag, OpenRouter's "provider" routing object, any other
// top-level field — is merged afterwards, so the SDK never has to model a field
// only one provider speaks.
//
// Two things are applied to a COPY of the request, never to the caller's own
// value: the foreign-block rule, which drops reasoning and opaque blocks that
// belong to another provider's dialect, and the moving prompt-cache markers.
func Encode(req *msg.Request, opts msg.EncodeOptions) ([]byte, error) {
	if req == nil {
		return nil, errors.New("anthropic: encode: nil request")
	}
	params, err := encodeParams(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode request: %w", err)
	}
	if !opts.Stream && len(opts.Extra) == 0 {
		return body, nil
	}
	fields := make(map[string]any, len(opts.Extra)+1)
	for k, v := range opts.Extra {
		fields[k] = v
	}
	if opts.Stream {
		fields["stream"] = true
	}
	return mergeFields(body, fields)
}

// mergeFields replaces or adds top-level fields of an already-encoded body.
func mergeFields(body []byte, fields map[string]any) ([]byte, error) {
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, fmt.Errorf("anthropic: read encoded body: %w", err)
	}
	for k, v := range fields {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("anthropic: encode extra field %q: %w", k, err)
		}
		merged[k] = raw
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode body with extras: %w", err)
	}
	return out, nil
}

// encodeParams assembles the SDK's request params: the provider-independent
// request, the dialect's own filtering, and the cache policy's markers.
func encodeParams(req *msg.Request) (sdk.MessageNewParams, error) {
	messages, _ := dropForeign(req.Messages)
	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: req.MaxTokens,
		Messages:  make([]sdk.MessageParam, 0, len(messages)),
	}
	for _, m := range messages {
		p, err := ToParam(m)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		params.Messages = append(params.Messages, p)
	}
	if len(req.System) > 0 {
		system := make([]sdk.TextBlockParam, len(req.System))
		for i, s := range req.System {
			system[i] = sdk.TextBlockParam{Text: s.Text}
		}
		// Tools render before system in the API's prefix, so a marker on the
		// last system block caches the tool schemas together with it.
		if cc, on := cacheControlFor(req.Cache.SystemTTL); on {
			system[len(system)-1].CacheControl = cc
		}
		params.System = system
	}
	if len(req.Tools) > 0 {
		tools, err := encodeTools(req.Tools)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		params.Tools = tools
	}
	if req.ForceTool != "" {
		params.ToolChoice = sdk.ToolChoiceUnionParam{OfTool: &sdk.ToolChoiceToolParam{Name: req.ForceTool}}
	}
	if req.Temperature != nil {
		params.Temperature = sdk.Float(*req.Temperature)
	}
	if req.TopK != nil {
		params.TopK = sdk.Int(*req.TopK)
	}
	if req.ThinkingBudget > 0 {
		params.Thinking = sdk.ThinkingConfigParamOfEnabled(req.ThinkingBudget)
	}
	applyBreakpoints(params.Messages, breakpoints(messages, req.Cache), req.Cache.MessagesTTL)
	return params, nil
}

// encodeTools builds the API's tool declarations. A tool with no schema still
// gets the API's default object schema rather than a decode error.
func encodeTools(defs []msg.ToolDef) ([]sdk.ToolUnionParam, error) {
	tools := make([]sdk.ToolUnionParam, 0, len(defs))
	for _, td := range defs {
		var schema sdk.ToolInputSchemaParam
		if len(bytes.TrimSpace(td.InputSchema)) > 0 {
			if err := json.Unmarshal(td.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("anthropic: tool %q input schema: %w", td.Name, err)
			}
		}
		tool := sdk.ToolUnionParamOfTool(schema, td.Name)
		if td.Description != "" {
			tool.OfTool.Description = sdk.String(td.Description)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// cacheControlFor maps a cache TTL to its wire form. on=false for CacheOff and
// for the empty TTL, so an unset policy never emits a marker.
func cacheControlFor(ttl msg.CacheTTL) (sdk.CacheControlEphemeralParam, bool) {
	switch ttl {
	case msg.Cache5m:
		return sdk.NewCacheControlEphemeralParam(), true // an omitted TTL is the 5m default
	case msg.Cache1h:
		cc := sdk.NewCacheControlEphemeralParam()
		cc.TTL = sdk.CacheControlEphemeralTTLTTL1h
		return cc, true
	}
	return sdk.CacheControlEphemeralParam{}, false
}

// applyBreakpoints writes the message markers breakpoints placed. The positions
// index the filtered messages, which is exactly how params.Messages was built,
// so a marker always lands on a block that survived the foreign-block rule.
func applyBreakpoints(messages []sdk.MessageParam, positions []Position, ttl msg.CacheTTL) {
	cc, on := cacheControlFor(ttl)
	if !on {
		return
	}
	for _, p := range positions {
		if p.Message < 0 || p.Message >= len(messages) {
			continue
		}
		block := p.Block
		if block < 0 || block >= len(messages[p.Message].Content) {
			continue
		}
		messages[p.Message].Content[block] = blockWithCacheControl(messages[p.Message].Content[block], cc)
	}
}

// blockWithCacheControl returns a copy of b with cc applied. Every union member
// that carries cache_control is covered, mirroring the SDK's own
// ContentBlockParamUnion.GetCacheControl; a block without one is returned
// unchanged.
func blockWithCacheControl(b sdk.ContentBlockParamUnion, cc sdk.CacheControlEphemeralParam) sdk.ContentBlockParamUnion {
	switch {
	case b.OfText != nil:
		v := *b.OfText
		v.CacheControl = cc
		b.OfText = &v
	case b.OfImage != nil:
		v := *b.OfImage
		v.CacheControl = cc
		b.OfImage = &v
	case b.OfDocument != nil:
		v := *b.OfDocument
		v.CacheControl = cc
		b.OfDocument = &v
	case b.OfSearchResult != nil:
		v := *b.OfSearchResult
		v.CacheControl = cc
		b.OfSearchResult = &v
	case b.OfToolUse != nil:
		v := *b.OfToolUse
		v.CacheControl = cc
		b.OfToolUse = &v
	case b.OfToolResult != nil:
		v := *b.OfToolResult
		v.CacheControl = cc
		b.OfToolResult = &v
	case b.OfServerToolUse != nil:
		v := *b.OfServerToolUse
		v.CacheControl = cc
		b.OfServerToolUse = &v
	case b.OfWebSearchToolResult != nil:
		v := *b.OfWebSearchToolResult
		v.CacheControl = cc
		b.OfWebSearchToolResult = &v
	case b.OfWebFetchToolResult != nil:
		v := *b.OfWebFetchToolResult
		v.CacheControl = cc
		b.OfWebFetchToolResult = &v
	case b.OfCodeExecutionToolResult != nil:
		v := *b.OfCodeExecutionToolResult
		v.CacheControl = cc
		b.OfCodeExecutionToolResult = &v
	case b.OfBashCodeExecutionToolResult != nil:
		v := *b.OfBashCodeExecutionToolResult
		v.CacheControl = cc
		b.OfBashCodeExecutionToolResult = &v
	case b.OfTextEditorCodeExecutionToolResult != nil:
		v := *b.OfTextEditorCodeExecutionToolResult
		v.CacheControl = cc
		b.OfTextEditorCodeExecutionToolResult = &v
	case b.OfToolSearchToolResult != nil:
		v := *b.OfToolSearchToolResult
		v.CacheControl = cc
		b.OfToolSearchToolResult = &v
	case b.OfContainerUpload != nil:
		v := *b.OfContainerUpload
		v.CacheControl = cc
		b.OfContainerUpload = &v
	}
	return b
}

// dropForeign returns a copy of msgs holding only what this dialect may send,
// plus, per message, each original block's index in the copy (-1 when it was
// dropped).
//
// A thinking block whose signature another dialect minted carries reasoning
// this API can neither verify nor resume, and an opaque block another provider
// wrote has no meaning here at all; both are dropped whole. Residue a foreign
// dialect left in a block's Ext is dropped as well, while this dialect's own
// residue rides along. Nothing in msgs is written to: blocks are values, and
// the copy is what ToParam sees.
func dropForeign(msgs []msg.Message) ([]msg.Message, [][]int) {
	out := make([]msg.Message, len(msgs))
	remap := make([][]int, len(msgs))
	for i, m := range msgs {
		content := make([]msg.Block, 0, len(m.Content))
		index := make([]int, len(m.Content))
		for j, b := range m.Content {
			index[j] = -1
			if foreignBlock(b) {
				continue
			}
			index[j] = len(content)
			content = append(content, withoutForeignExt(b))
		}
		out[i] = msg.Message{Role: m.Role, Content: content}
		remap[i] = index
	}
	return out, remap
}

// foreignBlock reports whether b belongs to another provider's dialect.
func foreignBlock(b msg.Block) bool {
	switch t := b.(type) {
	case msg.Thinking:
		return t.Sig.Dialect != msg.DialectAnthropic
	case msg.Opaque:
		return t.Provider != msg.DialectAnthropic
	}
	return false
}

// withoutForeignExt returns a copy of b with another dialect's residue
// removed. A tool_result's own nested blocks are treated the same way.
func withoutForeignExt(b msg.Block) msg.Block {
	switch t := b.(type) {
	case msg.Text:
		if foreignExt(t.Ext) {
			t.Ext = msg.Ext{}
		}
		return t
	case msg.Thinking:
		if foreignExt(t.Ext) {
			t.Ext = msg.Ext{}
		}
		return t
	case msg.ToolUse:
		if foreignExt(t.Ext) {
			t.Ext = msg.Ext{}
		}
		return t
	case msg.ToolResult:
		if foreignExt(t.Ext) {
			t.Ext = msg.Ext{}
		}
		if t.Content != nil {
			content := make([]msg.Block, 0, len(t.Content))
			for _, c := range t.Content {
				if foreignBlock(c) {
					continue
				}
				content = append(content, withoutForeignExt(c))
			}
			t.Content = content
		}
		return t
	case msg.Image:
		if foreignExt(t.Ext) {
			t.Ext = msg.Ext{}
		}
		return t
	}
	return b
}

// foreignExt reports whether ext holds residue another dialect wrote.
func foreignExt(ext msg.Ext) bool {
	return ext.Dialect != "" && ext.Dialect != msg.DialectAnthropic
}
