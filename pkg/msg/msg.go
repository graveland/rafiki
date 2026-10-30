// Package msg defines rafiki's native conversation message model: a role and a
// list of content blocks, independent of any provider's wire types.
//
// It is the model everything downstream of storage speaks. A provider codec
// (pkg/msg/anthropic) translates between it and that provider's own message
// types, in both directions, and keeps anything it cannot represent faithfully
// as an Opaque block rather than dropping it.
package msg

import (
	"encoding/json"
	"fmt"
)

// Role is the author of a Message. The zero value is deliberately invalid:
// every message that reaches storage has an author, and a zero Role is a bug
// in the caller rather than a default worth guessing at.
type Role uint8

// System is a message the harness injected into the conversation rather than one
// either party wrote — a Claude Code notice, a heartbeat nudge, an environment
// update. Stored history contains them, so a model that cannot read one makes a
// whole conversation unreadable.
const (
	User      Role = 1
	Assistant Role = 2
	System    Role = 3
)

// String returns the wire spelling of r, or "invalid" for any unset value.
func (r Role) String() string {
	switch r {
	case User:
		return "user"
	case Assistant:
		return "assistant"
	case System:
		return "system"
	}
	return "invalid"
}

// ParseRole parses the wire spelling of a role.
func ParseRole(s string) (Role, error) {
	switch s {
	case "user":
		return User, nil
	case "assistant":
		return Assistant, nil
	case "system":
		return System, nil
	}
	return 0, fmt.Errorf("msg: unknown role %q", s)
}

// Message is one conversation message: its author and its content blocks, in
// order. Content is a block list so that a message can mix kinds, which every
// provider allows.
type Message struct {
	Role    Role
	Content []Block
}

// Block is one content block. The interface is closed by design: block() has a
// value receiver on every type in this package and nowhere else, so the set of
// known kinds is exactly BlockKinds().
type Block interface{ block() }

// Text is a run of plain text.
type Text struct{ Text string }

// Thinking is a model reasoning block. Sig carries where its signature came
// from, because a signature minted by one provider's dialect is only
// meaningful to that dialect and must not be replayed to another.
type Thinking struct {
	Text string
	Sig  Signature
}

// ToolUse is a model's request to run a tool. Input is the argument object as
// stored; it is opaque JSON because its shape belongs to the tool, not here.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is the outcome of a tool use, delivered back to the model. Content
// is a block list rather than a string because a result may carry images and
// other structured parts.
type ToolResult struct {
	ToolUseID string
	Content   []Block
	IsError   bool
}

// Image is an inline image. Data is the decoded bytes; MediaType is the MIME
// type it should be labelled with.
type Image struct {
	MediaType string
	Data      []byte
}

// Opaque is a block kept verbatim because it has no faithful native form: a
// provider block this codec does not model, or one whose fields would be lost
// on a round trip. Provider names the dialect Raw belongs to.
type Opaque struct {
	Provider Dialect
	Raw      json.RawMessage
}

// Dialect names a wire dialect. The Responses API's two backends are separate
// dialects because the encrypted reasoning a block carries is only meaningful
// to the backend that minted it.
type Dialect string

const (
	DialectAnthropic        Dialect = "anthropic"
	DialectOpenAIChat       Dialect = "openai-chat"
	DialectResponsesAPI     Dialect = "openai-responses:api"
	DialectResponsesChatGPT Dialect = "openai-responses:chatgpt"
)

// Signature is a thinking block's provenance: the dialect that minted it and
// the provider's signature string, carried verbatim.
type Signature struct {
	Dialect Dialect
	Value   string
}

func (Text) block()       {}
func (Thinking) block()   {}
func (ToolUse) block()    {}
func (ToolResult) block() {}
func (Image) block()      {}
func (Opaque) block()     {}

// BlockKinds returns one zero value of every block type in this package, in
// documentation order. It exists so that a new block type has one place to be
// registered, and a test can prove the registration is complete.
func BlockKinds() []Block {
	return []Block{
		Text{},
		Thinking{},
		ToolUse{},
		ToolResult{},
		Image{},
		Opaque{},
	}
}

// ToolUseIDs returns the tool_use and tool_result ids of m, in block order,
// skipping empty ids. It is the linkage orphaned-tool_use recovery keys on, and
// it must agree with what storage records for the same content.
//
// An Opaque block yields its id too, when its raw JSON declares one: a block kept
// verbatim for a field this model does not carry (a caller, say) is still a tool
// call, and dropping its id would orphan the result that answers it.
func ToolUseIDs(m Message) []string {
	var ids []string
	for _, b := range m.Content {
		switch t := b.(type) {
		case ToolUse:
			if t.ID != "" {
				ids = append(ids, t.ID)
			}
		case ToolResult:
			if t.ToolUseID != "" {
				ids = append(ids, t.ToolUseID)
			}
		case Opaque:
			ids = append(ids, opaqueIDs(t.Raw)...)
		}
	}
	return ids
}

// opaqueIDs returns the id an Opaque block declares, or nothing. Raw that is not
// a JSON object, that is not a tool block, or whose id is empty declares none.
func opaqueIDs(raw json.RawMessage) []string {
	var head struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil
	}
	switch head.Type {
	case "tool_use":
		if head.ID != "" {
			return []string{head.ID}
		}
	case "tool_result":
		if head.ToolUseID != "" {
			return []string{head.ToolUseID}
		}
	}
	return nil
}
