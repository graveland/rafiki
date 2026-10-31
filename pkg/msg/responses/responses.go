// Package responses is the codec between rafiki's native message model
// (pkg/msg) and the OpenAI Responses API, in both of its dialects: the plain
// api.openai.com endpoint and the ChatGPT-subscription backend.
//
// The two modes exist because the encrypted reasoning content a Responses
// response carries is only meaningful to the backend that minted it, so a
// failover between them has to drop the foreign blob exactly as a failover to
// Anthropic does. Everything else the two modes share lives here once, keyed on
// Mode.
package responses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// Mode names the Responses backend a codec talks to, and therefore the only
// mode of reasoning blob it may mint or resend.
type Mode string

const (
	// ModeAPI is the plain openai-responses kind: api.openai.com under an
	// operator's key, or a compatible endpoint.
	ModeAPI Mode = "api"
	// ModeChatGPT is the openai-responses-chatgpt subscription dialect, whose
	// blobs only the ChatGPT backend accepts.
	ModeChatGPT Mode = "chatgpt"
)

// valid reports whether m is a mode this package writes. An unknown mode is
// refused at parse time rather than kept as an opaque label: a blob under a mode
// nobody can resume is dead weight.
func (m Mode) valid() bool { return m == ModeAPI || m == ModeChatGPT }

// Dialect returns the native dialect this mode's content belongs to, or "" for a
// mode this package does not know.
func (m Mode) Dialect() msg.Dialect {
	switch m {
	case ModeAPI:
		return msg.DialectResponsesAPI
	case ModeChatGPT:
		return msg.DialectResponsesChatGPT
	}
	return ""
}

const (
	// includeEncryptedReasoning asks the endpoint for the encrypted reasoning
	// content that makes a reasoning item resendable on the next turn.
	includeEncryptedReasoning = "reasoning.encrypted_content"

	// reasonContentFilter is the incomplete reason for a response the provider's
	// content filter stopped, and reasonMaxOutputTokens for one that ran out of
	// output budget.
	reasonContentFilter   = "content_filter"
	reasonMaxOutputTokens = "max_output_tokens"
	contentFilterMessage  = "response blocked by the provider's content filter"
)

// ModelFact is what the serving account's /models entry says about one model.
//
// Every field is optional in the server's answer, and "absent" means "this
// deployment does not tell us", not "false" and not "empty". The rule is to omit
// a field the server's answer does not mention rather than send a value the
// endpoint may reject: these four are exactly the fields codex gates its own
// request on from its /models response.
type ModelFact struct {
	SupportsReasoningSummary bool     // supports_reasoning_summary_parameter
	SupportsVerbosity        bool     // support_verbosity
	DefaultVerbosity         string   // default_verbosity
	ReasoningLevels          []string // supported_reasoning_levels[].effort, server order
}

// ModelFacts looks a model up in the owner's /models cache; ok=false when
// unknown. Nil is legal.
type ModelFacts func(model string) (ModelFact, bool)

// Codec is the Responses codec for one mode.
type Codec struct {
	Mode Mode
	// Facts is what is known about the model being asked; nil means nothing is.
	Facts ModelFacts
	// DevMessage places the system prompt where current codex puts it — the
	// first input item, as a developer message — instead of in the top-level
	// instructions field.
	DevMessage bool
}

// effortOrder is fundi's escalating ladder, lowest first. It is both the clamp
// order and the vocabulary the endpoint's /models entries use.
var effortOrder = []string{"minimal", "low", "medium", "high", "xhigh"}

// check refuses a codec whose mode this package does not know. Without it a
// zero-valued Codec would encode like the API mode while dropping every reasoning
// item, because no signature parses as the empty mode — a silent failure, which is
// worse than a loud one.
func (c Codec) check() error {
	if !c.Mode.valid() {
		return fmt.Errorf("responses: unknown mode %q", c.Mode)
	}
	return nil
}

// Encode translates req into a Responses request body.
//
// The shape is the STANDARD Responses shape: top-level instructions and tools,
// function_call items, no codex dialect. That is not a stylistic preference: the
// endpoint's lite dialect is selected by a header and imposes requirements that a
// standard body contradicts.
func (c Codec) Encode(req *msg.Request, opts msg.EncodeOptions) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("responses: encode: nil request")
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	fact, haveFact := c.factFor(req.Model)
	chatGPT := c.Mode == ModeChatGPT

	body := request{
		Model:             req.Model,
		Input:             []any{},
		ParallelToolCalls: true,
		Stream:            opts.Stream || chatGPT,
		MaxOutputTokens:   req.MaxTokens,
	}
	if chatGPT {
		// The subscription backend refuses to store a response, and the caller
		// cannot ask for it through the native request.
		store := false
		body.Store = &store
	}

	// The native system is a list of blocks; Responses has both a top-level
	// instructions string and (as current codex uses) a developer input item.
	system := systemText(req.System)
	switch {
	case c.DevMessage:
		if system != "" {
			body.Input = append(body.Input, messageItem{
				Type:    "message",
				Role:    "developer",
				Content: []any{textPart{Type: "input_text", Text: system}},
			})
		}
	case system != "" || chatGPT:
		// ChatGPT mode always sends the key (empty when there is no system
		// prompt): the endpoint's own request shape has it, and an absent
		// instructions field reads as "use the default", not "no system prompt".
		body.Instructions = &system
	}

	for _, m := range req.Messages {
		items, err := c.messageItems(m)
		if err != nil {
			return nil, err
		}
		body.Input = append(body.Input, items...)
	}

	for _, t := range req.Tools {
		schema, err := toolSchema(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("responses: tool %q: %w", t.Name, err)
		}
		body.Tools = append(body.Tools, functionTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			// strict is a separate, schema-rewriting feature, and a caller that
			// wanted it has no way to ask through the native tool shape.
			Strict:     false,
			Parameters: schema,
		})
	}
	// tool_choice without tools is an API error, so it is only sent when at least
	// one tool survived translation. The native request can only name a tool.
	if len(body.Tools) > 0 && req.ForceTool != "" {
		body.ToolChoice = toolChoiceFunction{Type: "function", Name: req.ForceTool}
	}

	effort, reasoning := c.effort(req.ThinkingBudget, fact, haveFact)
	if reasoning {
		r := reasoningConfig{Effort: effort}
		// summary is available on openai-responses, and on the ChatGPT dialect
		// only for models whose /models entry offers it.
		if !chatGPT || fact.SupportsReasoningSummary {
			r.Summary = "auto"
		}
		body.Reasoning = &r
	}
	// Encrypted reasoning content is what makes a reasoning item resendable, so it
	// is requested whenever reasoning is, and always in ChatGPT mode (where the
	// endpoint emits reasoning items regardless of our effort choice).
	if reasoning || chatGPT {
		body.Include = []string{includeEncryptedReasoning}
	}
	// verbosity is a ChatGPT-dialect field: it is sent only for a model whose
	// /models entry declares support AND a default, since an empty verbosity is not
	// a value the endpoint can act on.
	if chatGPT && fact.SupportsVerbosity && fact.DefaultVerbosity != "" {
		body.Text = &textConfig{Verbosity: fact.DefaultVerbosity}
	}
	// temperature is not a supported parameter on the ChatGPT dialect (400
	// "Unsupported parameter: temperature"), so it is never sent there.
	if !chatGPT && req.Temperature != nil {
		t := *req.Temperature
		body.Temperature = &t
	}
	if opts.SessionID != "" {
		body.PromptCacheKey = opts.SessionID
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("responses: marshal request: %w", err)
	}
	return mergeExtra(encoded, opts.Extra)
}

// factFor looks req's model up in the codec's facts, if it has any.
func (c Codec) factFor(model string) (ModelFact, bool) {
	if c.Facts == nil {
		return ModelFact{}, false
	}
	return c.Facts(model)
}

// mergeExtra folds opts.Extra into the encoded body as top-level fields,
// replacing any key the codec wrote itself.
func mergeExtra(body []byte, extra map[string]any) ([]byte, error) {
	if len(extra) == 0 {
		return body, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, fmt.Errorf("responses: read encoded body for extras: %w", err)
	}
	for k, v := range extra {
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("responses: marshal extra field %q: %w", k, err)
		}
		merged[k] = encoded
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("responses: marshal body with extras: %w", err)
	}
	return out, nil
}

// systemText joins the system blocks with a blank line. One string, not one per
// block: the Responses API has a single system field either way.
func systemText(blocks []msg.Text) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolSchema renders a tool's input schema. Only the three keys every input
// schema in this codebase declares are carried: the native tool has no place to
// put JSON-schema keywords the API does not model, so inventing them
// (additionalProperties: false, for instance) would silently narrow a caller's
// schema.
func toolSchema(raw json.RawMessage) (map[string]any, error) {
	schema := map[string]any{"type": "object"}
	if len(bytes.TrimSpace(raw)) == 0 {
		return schema, nil
	}
	var in struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	if in.Type != "" {
		schema["type"] = in.Type
	}
	if props := bytes.TrimSpace(in.Properties); len(props) > 0 && !bytes.Equal(props, []byte("null")) {
		schema["properties"] = in.Properties
	}
	if len(in.Required) > 0 {
		schema["required"] = in.Required
	}
	return schema, nil
}

// effort resolves one thinking budget to a reasoning effort, and says whether
// reasoning was requested at all.
//
// The budget thresholds are fundi's own levels (low 4096, medium 8192, high
// 16384, xhigh 32768). When the model's facts are known its supported levels win:
// the desired level if listed, else the highest listed level below it, else the
// lowest listed level. Without facts the only correction is xhigh to high,
// because xhigh is a codex-dialect level a plain api.openai.com model may not
// accept.
func (c Codec) effort(budget int64, fact ModelFact, haveFact bool) (string, bool) {
	if budget <= 0 {
		return "", false
	}
	desired := "xhigh"
	switch {
	case budget <= 4096:
		desired = "low"
	case budget <= 8192:
		desired = "medium"
	case budget <= 16384:
		desired = "high"
	}
	if haveFact && len(fact.ReasoningLevels) > 0 {
		return clampEffort(desired, fact.ReasoningLevels), true
	}
	if desired == "xhigh" {
		desired = "high"
	}
	return desired, true
}

// clampEffort keeps desired if levels supports it, else lowers to the highest
// supported level below it, else the lowest supported level. levels arrives in
// the server's order; the clamp walks effortOrder so an unfamiliar level name in
// the middle cannot reorder the ladder.
func clampEffort(desired string, levels []string) string {
	if slices.Contains(levels, desired) {
		return desired
	}
	desiredRank := slices.Index(effortOrder, desired)
	if desiredRank < 0 {
		desiredRank = len(effortOrder) - 1
	}
	best := ""
	bestRank := -1
	lowest := ""
	lowestRank := len(effortOrder)
	for _, l := range levels {
		rank := slices.Index(effortOrder, l)
		if rank < 0 {
			continue // a level this ladder does not know
		}
		if rank < desiredRank && rank > bestRank {
			best, bestRank = l, rank
		}
		if rank < lowestRank {
			lowest, lowestRank = l, rank
		}
	}
	if best != "" {
		return best
	}
	if lowest != "" {
		return lowest
	}
	return desired
}

// messageItems translates one native message into Responses input items,
// preserving block order.
//
// tool_use becomes function_call with NO id key, ever. The endpoint's own item
// ids are fc_… and it rejects any other prefix (a resent id that came from
// Anthropic after a failover would be toolu_…), while it accepts a function_call
// without one. The id a native tool_use block HAS is the endpoint's call_id, which
// is what correlates the call with its output; the separate fc_… item id has
// nowhere to live in the native block and is discarded.
func (c Codec) messageItems(m msg.Message) ([]any, error) {
	var items []any
	var parts []any
	role := "user"
	partType := "input_text"
	switch m.Role {
	case msg.Assistant:
		role, partType = "assistant", "output_text"
	case msg.User:
	default:
		return nil, fmt.Errorf("responses: message role %q has no Responses equivalent", m.Role)
	}
	flush := func() {
		if len(parts) == 0 {
			return
		}
		items = append(items, messageItem{Type: "message", Role: role, Content: parts})
		parts = nil
	}

	for _, b := range m.Content {
		switch t := b.(type) {
		case msg.Text:
			if t.Text == "" {
				continue
			}
			parts = append(parts, textPart{Type: partType, Text: t.Text})
		case msg.Image:
			img, err := imagePart(t)
			if err != nil {
				return nil, err
			}
			parts = append(parts, img)
		case msg.Thinking:
			// A Responses reasoning blob is only meaningful to the backend that
			// produced it, and the parsed mode IS the dialect that minted it: this
			// is the same gate a sender applies before building, so a foreign
			// reasoning item (Anthropic's, or the other Responses mode's) is
			// dropped rather than replayed to a backend that cannot verify it.
			mode, id, blob, ok := ParseSignature(t.Sig.Value)
			if !ok || mode != c.Mode || blob == "" {
				continue
			}
			flush()
			summary := []summaryPart{}
			if t.Text != "" {
				summary = append(summary, summaryPart{Type: "summary_text", Text: t.Text})
			}
			items = append(items, reasoningItem{
				Type:             "reasoning",
				ID:               id,
				EncryptedContent: blob,
				Summary:          summary,
			})
		case msg.ToolUse:
			if role != "assistant" {
				return nil, fmt.Errorf("responses: tool_use in a %q message", m.Role)
			}
			args, err := toolArguments(t.Input)
			if err != nil {
				return nil, fmt.Errorf("responses: tool_use %q: %w", t.ID, err)
			}
			flush()
			items = append(items, functionCall{
				Type:      "function_call",
				CallID:    t.ID,
				Name:      t.Name,
				Arguments: args,
			})
		case msg.ToolResult:
			flush()
			items = append(items, functionCallOutputFrom(t))
		case msg.Opaque:
			logDroppedBlock(msg.OpaqueType(t), string(m.Role))
		default:
			logDroppedBlock("unknown", string(m.Role))
		}
	}
	flush()
	return items, nil
}

// functionCallOutputFrom renders one tool result. Text-only results carry a plain
// string; a result with an image carries the array form, which is the only way to
// send an image back.
func functionCallOutputFrom(tr msg.ToolResult) functionCallOutput {
	var text strings.Builder
	var parts []any
	for _, c := range tr.Content {
		switch t := c.(type) {
		case msg.Text:
			text.WriteString(t.Text)
			if t.Text != "" {
				parts = append(parts, textPart{Type: "input_text", Text: t.Text})
			}
		case msg.Image:
			img, err := imagePart(t)
			if err != nil {
				// An unrenderable image is dropped from the array form rather than
				// failing the turn: the text of the result still reaches the model,
				// which is the whole point of the tool result.
				logDroppedBlock("image", "tool_result")
				continue
			}
			parts = append(parts, img)
		default:
			logDroppedBlock(blockTypeName(c), "tool_result")
		}
	}
	out := functionCallOutput{Type: "function_call_output", CallID: tr.ToolUseID}
	if containsImage(parts) {
		out.Output = parts
	} else {
		out.Output = text.String()
	}
	return out
}

// containsImage reports whether any part of a tool result is an image, which is
// what selects the array form of "output".
func containsImage(parts []any) bool {
	for _, p := range parts {
		if _, ok := p.(inputImage); ok {
			return true
		}
	}
	return false
}

// imagePart renders a native image block as a data URL, which is the only image
// form the Responses input accepts for inline bytes.
func imagePart(img msg.Image) (inputImage, error) {
	if len(img.Data) == 0 {
		return inputImage{}, fmt.Errorf("responses: image block has no data")
	}
	media := img.MediaType
	if media == "" {
		media = "image/png"
	}
	return inputImage{
		Type:     "input_image",
		ImageURL: "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
	}, nil
}

// toolArguments renders a tool_use's input as the JSON string the endpoint takes.
// An absent input is the empty object, which is what a tool with no arguments
// declares.
func toolArguments(input json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, input); err != nil {
		return "", fmt.Errorf("tool input is not valid JSON (%.200q): %w", string(input), err)
	}
	return buf.String(), nil
}

// toolCallInput parses a function_call's argument string back into the native
// form. An absent argument string is the empty object.
func toolCallInput(arguments string) (json.RawMessage, error) {
	if arguments == "" {
		return json.RawMessage("{}"), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(arguments)); err != nil {
		return nil, fmt.Errorf("tool arguments are not valid JSON (%.200q): %w", arguments, err)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// blockTypeName names a block for a dropped-block log line.
func blockTypeName(b msg.Block) string {
	switch b.(type) {
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

// logDroppedBlock reports a content block this translation cannot represent.
// Images and documents carry user content, so losing them is worth a warning;
// thinking blocks are routine and carry no user content.
func logDroppedBlock(kind, role string) {
	loud := kind == "image" || kind == "document" || kind == "search_result"
	message := "responses: dropping content block with no Responses equivalent"
	if loud {
		slog.Warn(message, "role", role, "type", kind)
		return
	}
	slog.Debug(message, "role", role, "type", kind)
}

// summaryText joins a reasoning item's summary parts into one string.
func summaryText(parts []summaryPart) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// DecodeResponse translates a complete (stream:false) Responses body into a
// native response. It is the non-streaming twin of the stream decoder and shares
// its item rules: function calls become tool_use blocks keyed by call_id, and
// reasoning items become thinking blocks carrying the Responses signature (empty
// blob when the item carried no encrypted content: the signature is what marks
// the block as this dialect's, so an Anthropic failover strips it instead of
// rejecting it).
func (c Codec) DecodeResponse(body []byte) (*msg.Response, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	var wire responseObject
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("responses: decode response: %w", err)
	}
	if wire.Error != nil {
		return nil, c.errorFromBody(body)
	}
	resp := &msg.Response{ID: wire.ID, Model: wire.Model, Usage: usageOf(wire.Usage)}
	sawTool := false
	for _, item := range wire.Output {
		switch item.Type {
		case "message":
			var sb strings.Builder
			for _, p := range item.Content {
				if p.Type == "output_text" || p.Type == "text" {
					sb.WriteString(p.Text)
				}
			}
			if sb.Len() > 0 {
				resp.Content = append(resp.Content, msg.Text{Text: sb.String()})
			}
		case "function_call":
			input, err := toolCallInput(item.Arguments)
			if err != nil {
				return nil, fmt.Errorf("responses: function call %q: %w", item.CallID, err)
			}
			resp.Content = append(resp.Content, msg.ToolUse{ID: item.CallID, Name: item.Name, Input: input})
			sawTool = true
		case "reasoning":
			resp.Content = append(resp.Content, msg.Thinking{
				Text: summaryText(item.Summary),
				Sig: msg.Signature{
					Dialect: c.Mode.Dialect(),
					Value:   Signature(c.Mode, item.ID, item.EncryptedContent),
				},
			})
		}
	}
	reason := ""
	if wire.IncompleteDetails != nil {
		reason = wire.IncompleteDetails.Reason
	}
	// The endpoint reports why it stopped as incomplete_details.reason, or not
	// at all; keep its own spelling beside the modelled enum for a record.
	resp.StopReasonRaw = reason
	switch {
	case reason == reasonContentFilter:
		return nil, &msg.APIError{Kind: msg.ErrOther, Dialect: c.Mode.Dialect(), Message: contentFilterMessage}
	case reason == reasonMaxOutputTokens:
		resp.StopReason = msg.StopMaxTokens
	case sawTool:
		resp.StopReason = msg.StopToolUse
	default:
		resp.StopReason = msg.StopEndTurn
	}
	return resp, nil
}

// errorFromBody turns a response object's error into the error this codec
// returns. A usage limit gets its own type; everything else is an APIError
// carrying the body's classification.
func (c Codec) errorFromBody(body []byte) error {
	if u := usageLimitFromEnvelope(decodeErrorEnvelope(body)); u != nil {
		return u
	}
	_, _, message, _ := decodeErrorEnvelope(body).info()
	return &msg.APIError{
		Status:  0,
		Body:    body,
		Kind:    Classify(0, nil, body),
		Dialect: c.Mode.Dialect(),
		Message: message,
	}
}

// usageOf maps the endpoint's counters onto native usage, by presence: a count
// the response did not report stays nil, because a reported zero and an absent
// count are not the same evidence.
//
// input_tokens excludes the cached prefix (the endpoint reports the two
// separately, and a caller that added them would double-count the cache), and
// output_tokens already includes reasoning tokens, which is also how Anthropic
// counts them.
func usageOf(u *usageWire) msg.Usage {
	if u == nil {
		return msg.Usage{}
	}
	var out msg.Usage
	if u.InputTokens != nil {
		if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
			cached := *u.InputTokensDetails.CachedTokens
			out.CacheRead = msg.Ptr(cached)
			out.Input = msg.Ptr(max(*u.InputTokens-cached, 0))
		} else {
			out.Input = msg.Ptr(*u.InputTokens)
		}
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CacheWriteTokens != nil {
		out.CacheWrite = msg.Ptr(*u.InputTokensDetails.CacheWriteTokens)
	}
	if u.OutputTokens != nil {
		out.Output = msg.Ptr(*u.OutputTokens)
	}
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens != nil {
		out.Reasoning = msg.Ptr(*u.OutputTokensDetails.ReasoningTokens)
	}
	return out
}

// ---- wire shapes of a Responses request ----

// request is one POST /responses body. Absent fields are absent for a reason and
// each is documented above at its assignment site.
type request struct {
	Model             string           `json:"model"`
	Instructions      *string          `json:"instructions,omitempty"`
	Input             []any            `json:"input"`
	Tools             []functionTool   `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls bool             `json:"parallel_tool_calls"`
	Reasoning         *reasoningConfig `json:"reasoning,omitempty"`
	Text              *textConfig      `json:"text,omitempty"`
	MaxOutputTokens   int64            `json:"max_output_tokens"`
	Temperature       *float64         `json:"temperature,omitempty"`
	Stream            bool             `json:"stream"`
	Store             *bool            `json:"store,omitempty"`
	Include           []string         `json:"include,omitempty"`
	PromptCacheKey    string           `json:"prompt_cache_key,omitempty"`
}

type reasoningConfig struct {
	Effort string `json:"effort"`
	// Summary is "auto" only where the model's facts allow it. context is never
	// sent: codex sends it only in the lite dialect, and the standard shape omits
	// it.
	Summary string `json:"summary,omitempty"`
}

type textConfig struct {
	Verbosity string `json:"verbosity"`
}

type functionTool struct {
	Type        string         `json:"type"` // always "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"`
}

// toolChoiceFunction is the named-tool spelling: no nested function object,
// unlike Chat Completions.
type toolChoiceFunction struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// messageItem is an input item carrying conversation content: role user or
// assistant for the transcript, developer for the system prompt's alternative
// placement.
type messageItem struct {
	Type    string `json:"type"` // "message"
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// textPart is one content part of a message item. The type differs by role:
// input_text for a user or developer message, output_text for an assistant's own
// words.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputImage struct {
	Type     string `json:"type"` // "input_image"
	ImageURL string `json:"image_url"`
}

// reasoningItem resends one reasoning item from the transcript. id and
// encrypted_content are the two halves of this package's thinking signature;
// summary echoes the thinking text so a human reading the transcript sees what
// the item's summary was.
type reasoningItem struct {
	Type             string        `json:"type"` // "reasoning"
	ID               string        `json:"id"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []summaryPart `json:"summary"`
}

// functionCall is a resent tool call. It deliberately has no id field: see
// messageItems.
type functionCall struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// functionCallOutput is one tool result. Output is a string for a text result and
// an array of input parts when the result carries an image.
type functionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}

// ---- wire shapes of a Responses response ----

// responseObject is the body of a non-streamed call, and the response a stream's
// created/completed/incomplete/failed event carries.
type responseObject struct {
	ID                string           `json:"id"`
	Model             string           `json:"model"`
	Status            string           `json:"status"`
	Output            []outputItem     `json:"output"`
	Usage             *usageWire       `json:"usage"`
	IncompleteDetails *incompleteCause `json:"incomplete_details"`
	Error             *apiError        `json:"error"`
}

// incompleteCause is why a response stopped short.
type incompleteCause struct {
	Reason string `json:"reason"`
}

// outputItem is one entry of a response's output array, and the item a stream
// event carries.
type outputItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        string        `json:"arguments"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []summaryPart `json:"summary"`
	Content          []contentPart `json:"content"`
	Role             string        `json:"role"`
	Status           string        `json:"status"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// usageWire is the endpoint's token accounting. Every count is a pointer because
// a count the response omits is not a reported zero.
type usageWire struct {
	InputTokens         *int64              `json:"input_tokens"`
	InputTokensDetails  *inputTokenDetails  `json:"input_tokens_details"`
	OutputTokens        *int64              `json:"output_tokens"`
	OutputTokensDetails *outputTokenDetails `json:"output_tokens_details"`
}

type inputTokenDetails struct {
	CachedTokens     *int64 `json:"cached_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

type outputTokenDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}
