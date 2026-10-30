// Package anthropic is the codec between rafiki's native message model
// (pkg/msg) and the Anthropic message content stored for a conversation.
//
// Stored content is JSON: an array of blocks as the API accepts them, a bare
// JSON string for the shorthand prompt form, or null. Decoding is total and
// lossless: a block is turned into a native typed block only when re-encoding it
// reproduces the same canonical content, and otherwise it is kept verbatim as
// msg.Opaque. That is what keeps a field this package does not model —
// citations, an unusual image source, a caller added by the API — from being
// silently dropped on the way to storage, and what lets a single odd block (a
// wrongly-typed field, say) be read back without making its whole conversation
// unreadable.
package anthropic

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
)

const (
	// sigPrefixResponses is the older Responses signature shape, which names no
	// backend and so carries no replayable dialect.
	sigPrefixResponses        = "openai-responses:"
	sigPrefixResponsesAPI     = "openai-responses:api:"
	sigPrefixResponsesChatGPT = "openai-responses:chatgpt:"
)

// Decode turns stored message content into a native message. content is a JSON
// array of blocks, a bare JSON string (one text block), or null or empty (no
// content).
//
// Two things are errors: an unknown role, and content that is not valid JSON or
// is JSON of an unexpected top-level kind. Nothing inside a block is: a block
// whose fields cannot be read into the typed form degrades to Opaque carrying
// its original bytes, because one unreadable block must not fail a whole
// conversation's walk.
func Decode(role string, content []byte) (msg.Message, error) {
	r, err := msg.ParseRole(role)
	if err != nil {
		return msg.Message{}, err
	}
	blocks, err := decodeContent(content)
	if err != nil {
		return msg.Message{}, err
	}
	return msg.Message{Role: r, Content: blocks}, nil
}

// EncodeContent turns a native message into stored content JSON. The result is
// always a JSON array, and an Opaque block is emitted byte-identically.
func EncodeContent(m msg.Message) ([]byte, error) {
	out := make([]json.RawMessage, 0, len(m.Content))
	for _, b := range m.Content {
		enc, err := encodeBlock(b)
		if err != nil {
			return nil, err
		}
		out = append(out, enc)
	}
	return json.Marshal(out)
}

// ToParam builds the SDK's message type, for sending m through the API.
func ToParam(m msg.Message) (sdk.MessageParam, error) {
	role, err := toParamRole(m.Role)
	if err != nil {
		return sdk.MessageParam{}, err
	}
	content := make([]sdk.ContentBlockParamUnion, 0, len(m.Content))
	for _, b := range m.Content {
		p, err := toParamBlock(b)
		if err != nil {
			return sdk.MessageParam{}, err
		}
		content = append(content, p)
	}
	return sdk.MessageParam{Role: role, Content: content}, nil
}

// FromParam converts the SDK's message type back into a native message. It is
// Decode over the SDK's own JSON form, so it inherits the same guarantees.
func FromParam(p sdk.MessageParam) (msg.Message, error) {
	content, err := json.Marshal(p.Content)
	if err != nil {
		return msg.Message{}, fmt.Errorf("anthropic: marshal param content: %w", err)
	}
	return Decode(string(p.Role), content)
}

// decodeContent decodes stored content: an array of blocks, a bare string, or
// null/empty. A nil result means the message carries no content at all.
func decodeContent(content []byte) ([]msg.Block, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("anthropic: decode string content: %w", err)
		}
		return []msg.Block{msg.Text{Text: s}}, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(trimmed, &raws); err != nil {
		return nil, fmt.Errorf("anthropic: decode content: %w", err)
	}
	blocks := make([]msg.Block, 0, len(raws))
	for _, raw := range raws {
		b, err := decodeBlock(raw)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// decodeBlock decodes one block, falling back to Opaque whenever the native
// form cannot reproduce it.
func decodeBlock(raw json.RawMessage) (msg.Block, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return opaque(raw), nil
	}
	var (
		block msg.Block
		err   error
	)
	switch head.Type {
	case "text":
		block, err = decodeTextBlock(raw)
	case "thinking":
		block, err = decodeThinkingBlock(raw)
	case "tool_use":
		block, err = decodeToolUseBlock(raw)
	case "tool_result":
		block, err = decodeToolResultBlock(raw)
	case "image":
		block, err = decodeImageBlock(raw)
	default:
		return opaque(raw), nil
	}
	if err != nil {
		return opaque(raw), nil
	}
	return verify(raw, block)
}

func decodeTextBlock(raw json.RawMessage) (msg.Block, error) {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("anthropic: decode text block: %w", err)
	}
	return msg.Text{Text: p.Text}, nil
}

func decodeThinkingBlock(raw json.RawMessage) (msg.Block, error) {
	var p struct {
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("anthropic: decode thinking block: %w", err)
	}
	return msg.Thinking{
		Text: p.Thinking,
		Sig:  msg.Signature{Dialect: signatureDialect(p.Signature), Value: p.Signature},
	}, nil
}

func decodeToolUseBlock(raw json.RawMessage) (msg.Block, error) {
	var p struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("anthropic: decode tool_use block: %w", err)
	}
	return msg.ToolUse{ID: p.ID, Name: p.Name, Input: copyRaw(p.Input)}, nil
}

func decodeToolResultBlock(raw json.RawMessage) (msg.Block, error) {
	var p struct {
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("anthropic: decode tool_result block: %w", err)
	}
	content, err := decodeContent(p.Content)
	if err != nil {
		return nil, err
	}
	return msg.ToolResult{ToolUseID: p.ToolUseID, Content: content, IsError: p.IsError}, nil
}

func decodeImageBlock(raw json.RawMessage) (msg.Block, error) {
	var p struct {
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("anthropic: decode image block: %w", err)
	}
	if p.Source.Type != "base64" {
		return opaque(raw), nil
	}
	data, err := base64.StdEncoding.DecodeString(p.Source.Data)
	if err != nil {
		return opaque(raw), nil
	}
	return verify(raw, msg.Image{MediaType: p.Source.MediaType, Data: data})
}

// verify re-encodes block and returns it only if that reproduces raw's
// canonical content; otherwise the block is kept verbatim as Opaque.
func verify(raw json.RawMessage, block msg.Block) (msg.Block, error) {
	enc, err := encodeBlock(block)
	if err != nil {
		return nil, err
	}
	same, err := canonEqual(raw, enc)
	if err != nil {
		return nil, err
	}
	if !same {
		return opaque(raw), nil
	}
	return block, nil
}

// canonEqual reports whether two JSON values canonicalise to the same message
// content.
func canonEqual(a, b []byte) (bool, error) {
	ca, err := canonOf(a)
	if err != nil {
		return false, err
	}
	cb, err := canonOf(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

// canonOf returns the canonical form of a single JSON value treated as message
// content, which for a block means wrapping it in an array.
func canonOf(v []byte) ([]byte, error) {
	wrapped := make([]byte, 0, len(v)+2)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, v...)
	wrapped = append(wrapped, ']')
	normalized, err := Normalize(wrapped)
	if err != nil {
		return nil, err
	}
	canon, err := convtree.Canon(msg.User.String(), normalized)
	if err != nil {
		return nil, fmt.Errorf("anthropic: canon block: %w", err)
	}
	return canon, nil
}

func opaque(raw json.RawMessage) msg.Block {
	return msg.Opaque{Provider: msg.DialectAnthropic, Raw: copyRaw(raw)}
}

func copyRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}

// signatureDialect classifies a stored thinking signature by the family that
// minted it. A signature carrying an unrecognised openai-responses shape names
// no replayable backend, so it gets an empty dialect rather than a guess.
func signatureDialect(sig string) msg.Dialect {
	switch {
	case isPrefixed(sig, sigPrefixResponsesAPI):
		return msg.DialectResponsesAPI
	case isPrefixed(sig, sigPrefixResponsesChatGPT):
		return msg.DialectResponsesChatGPT
	case isPrefixed(sig, sigPrefixResponses):
		return ""
	}
	return msg.DialectAnthropic
}

func isPrefixed(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// encodeBlock encodes one block the way it is stored.
func encodeBlock(block msg.Block) (json.RawMessage, error) {
	switch t := block.(type) {
	case msg.Text:
		return marshalBlock(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", t.Text})
	case msg.Thinking:
		return marshalBlock(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{"thinking", t.Text, t.Sig.Value})
	case msg.ToolUse:
		input := t.Input
		if len(bytes.TrimSpace(input)) == 0 {
			input = json.RawMessage("{}")
		}
		return marshalBlock(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{"tool_use", t.ID, t.Name, input})
	case msg.ToolResult:
		content := make([]json.RawMessage, 0, len(t.Content))
		for _, c := range t.Content {
			enc, err := encodeBlock(c)
			if err != nil {
				return nil, err
			}
			content = append(content, enc)
		}
		var contentPtr *[]json.RawMessage
		if t.Content != nil {
			contentPtr = &content
		}
		return marshalBlock(struct {
			Type      string             `json:"type"`
			ToolUseID string             `json:"tool_use_id"`
			Content   *[]json.RawMessage `json:"content,omitempty"`
			IsError   bool               `json:"is_error,omitempty"`
		}{"tool_result", t.ToolUseID, contentPtr, t.IsError})
	case msg.Image:
		return marshalBlock(struct {
			Type   string `json:"type"`
			Source struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source"`
		}{
			Type: "image",
			Source: struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			}{
				Type:      "base64",
				MediaType: t.MediaType,
				Data:      base64.StdEncoding.EncodeToString(t.Data),
			},
		})
	case msg.Opaque:
		if len(bytes.TrimSpace(t.Raw)) == 0 {
			return nil, fmt.Errorf("anthropic: opaque block carries no JSON")
		}
		return copyRaw(t.Raw), nil
	}
	return nil, fmt.Errorf("anthropic: unknown block type %T", block)
}

func marshalBlock(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode block: %w", err)
	}
	return raw, nil
}

func toParamRole(r msg.Role) (sdk.MessageParamRole, error) {
	switch r {
	case msg.User:
		return sdk.MessageParamRoleUser, nil
	case msg.Assistant:
		return sdk.MessageParamRoleAssistant, nil
	}
	return "", fmt.Errorf("anthropic: unknown role %d", r)
}

// toParamBlock builds the SDK's typed block. Only an Opaque block is handed to
// the SDK's own JSON decoder, since it is the only one whose shape this package
// does not know.
func toParamBlock(block msg.Block) (sdk.ContentBlockParamUnion, error) {
	switch t := block.(type) {
	case msg.Text:
		return sdk.NewTextBlock(t.Text), nil
	case msg.Thinking:
		return sdk.NewThinkingBlock(t.Sig.Value, t.Text), nil
	case msg.ToolUse:
		input := t.Input
		if len(bytes.TrimSpace(input)) == 0 {
			input = json.RawMessage("{}")
		}
		return sdk.NewToolUseBlock(t.ID, input, t.Name), nil
	case msg.ToolResult:
		content, err := toParamToolResultContent(t.Content)
		if err != nil {
			return sdk.ContentBlockParamUnion{}, err
		}
		param := sdk.ToolResultBlockParam{ToolUseID: t.ToolUseID, Content: content}
		if t.IsError {
			param.IsError = sdk.Bool(true)
		}
		return sdk.ContentBlockParamUnion{OfToolResult: &param}, nil
	case msg.Image:
		return sdk.NewImageBlockBase64(t.MediaType, base64.StdEncoding.EncodeToString(t.Data)), nil
	case msg.Opaque:
		var param sdk.ContentBlockParamUnion
		if err := json.Unmarshal(t.Raw, &param); err != nil {
			return sdk.ContentBlockParamUnion{}, fmt.Errorf("anthropic: unmarshal opaque block: %w", err)
		}
		return param, nil
	}
	return sdk.ContentBlockParamUnion{}, fmt.Errorf("anthropic: unknown block type %T", block)
}

// toParamToolResultContent builds the narrower block union a tool_result
// carries. Anything outside it goes through a block's own param form and the
// SDK's JSON decoder, the same route an Opaque block takes.
func toParamToolResultContent(blocks []msg.Block) ([]sdk.ToolResultBlockParamContentUnion, error) {
	out := make([]sdk.ToolResultBlockParamContentUnion, 0, len(blocks))
	for _, b := range blocks {
		switch t := b.(type) {
		case msg.Text:
			out = append(out, sdk.ToolResultBlockParamContentUnion{
				OfText: &sdk.TextBlockParam{Text: t.Text},
			})
			continue
		case msg.Image:
			out = append(out, sdk.ToolResultBlockParamContentUnion{
				OfImage: &sdk.ImageBlockParam{Source: sdk.ImageBlockParamSourceUnion{
					OfBase64: &sdk.Base64ImageSourceParam{
						Data:      base64.StdEncoding.EncodeToString(t.Data),
						MediaType: sdk.Base64ImageSourceMediaType(t.MediaType),
					},
				}},
			})
			continue
		}
		param, err := toParamBlock(b)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(param)
		if err != nil {
			return nil, fmt.Errorf("anthropic: marshal nested block: %w", err)
		}
		var nested sdk.ToolResultBlockParamContentUnion
		if err := json.Unmarshal(raw, &nested); err != nil {
			return nil, fmt.Errorf("anthropic: unmarshal nested block: %w", err)
		}
		out = append(out, nested)
	}
	return out, nil
}
