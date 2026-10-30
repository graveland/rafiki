package msg

import "encoding/json"

// CacheTTL is how long a cached prompt prefix stays cached.
type CacheTTL string

const (
	// CacheOff asks for no caching.
	CacheOff CacheTTL = "off"
	// Cache1h keeps a cached prefix for an hour.
	Cache1h CacheTTL = "1h"
	// Cache5m keeps a cached prefix for five minutes.
	Cache5m CacheTTL = "5m"
)

// CachePolicy is a request's caching preference: how long each cacheable
// section stays cached and how many explicit breakpoints to place. It is a
// hint, not a promise.
type CachePolicy struct {
	SystemTTL   CacheTTL
	MessagesTTL CacheTTL
	Breakpoints int
}

// ToolDef is one tool a request offers the model: the name it is called by, the
// description the model reads, and the JSON Schema its input must satisfy.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Request is a provider-independent completion request. A codec translates it
// into its provider's wire format, so nothing here names a provider.
//
// Cache is a hint only the Anthropic codec acts on.
type Request struct {
	Model          string
	System         []Text
	Messages       []Message
	Tools          []ToolDef
	ForceTool      string // "" = auto
	MaxTokens      int64
	Temperature    *float64
	TopK           *int64
	ThinkingBudget int64 // 0 = off
	Cache          CachePolicy
}

// StopReason is why a model stopped generating.
type StopReason uint8

const (
	StopUnknown      StopReason = iota // String() == ""
	StopEndTurn                        // "end_turn"
	StopMaxTokens                      // "max_tokens"
	StopToolUse                        // "tool_use"
	StopStopSequence                   // "stop_sequence"
	StopRefusal                        // "refusal"
	StopPause                          // "pause_turn"
)

// String returns the wire spelling of s. StopUnknown and any value this package
// does not model render as "".
func (s StopReason) String() string {
	switch s {
	case StopEndTurn:
		return "end_turn"
	case StopMaxTokens:
		return "max_tokens"
	case StopToolUse:
		return "tool_use"
	case StopStopSequence:
		return "stop_sequence"
	case StopRefusal:
		return "refusal"
	case StopPause:
		return "pause_turn"
	}
	return ""
}

// ParseStopReason parses a wire stop reason, inverting String. "" and anything
// this package does not model are StopUnknown rather than an error: a stop
// reason is a report, and an unrecognised one is still a stop.
func ParseStopReason(s string) StopReason {
	switch s {
	case "end_turn":
		return StopEndTurn
	case "max_tokens":
		return StopMaxTokens
	case "tool_use":
		return StopToolUse
	case "stop_sequence":
		return StopStopSequence
	case "refusal":
		return StopRefusal
	case "pause_turn":
		return StopPause
	}
	return StopUnknown
}

// Usage is a request's or a response's token accounting.
//
// A nil field means the provider did not report that count, which is not the
// same as a reported zero: cost math and the provider guard both need the
// difference. Codecs decode by presence, so a count absent on the wire stays
// nil.
//
// Input is the UNCACHED input tokens. CacheRead and CacheWrite are the cached
// portions reported alongside it, not part of it.
type Usage struct {
	Input      *int64 // UNCACHED input tokens
	Output     *int64
	CacheRead  *int64
	CacheWrite *int64
	Reasoning  *int64
}

// Val returns the count p points at, or 0 when it is nil.
func Val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Ptr returns a pointer to a copy of v.
func Ptr(v int64) *int64 { return &v }

// Response is a provider-independent completion: what the model produced, why
// it stopped, and what it cost in tokens.
type Response struct {
	ID         string
	Model      string
	Content    []Block
	StopReason StopReason
	Usage      Usage
	Provider   string // OpenRouter's top-level "provider" (served-by); "" when absent
}

// EncodeOptions are the per-call knobs a codec's Encode honours.
type EncodeOptions struct {
	Stream    bool
	SessionID string         // the Responses codec puts it in prompt_cache_key; others ignore it
	Extra     map[string]any // top-level body fields merged after encoding (OpenRouter "provider", provider extras)
}
