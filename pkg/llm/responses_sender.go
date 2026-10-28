// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/klauspost/compress/zstd"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/version"
)

// Endpoints. openai-responses has a canonical default (api.openai.com); the
// ChatGPT-subscription dialect has a FIXED one, which providers.Validate
// enforces by refusing a base_url on that kind.
const (
	openAIResponsesBaseURL = "https://api.openai.com/v1"
	chatGPTBaseURL         = "https://chatgpt.com/backend-api/codex"
)

// chatGPTInstructionsAsDeveloperMessage places the system prompt where current
// codex puts it: the first input item, as a developer message, with no top-level
// instructions field. Spike 2 got a 200 from the top-level form, which is what
// this sender uses by default, but codex no longer exercises that field, so a
// backend change is plausible; this constant makes it a one-line fix.
const chatGPTInstructionsAsDeveloperMessage = false

// responsesUserAgent is the User-Agent every Responses request carries. Every
// client in this space sends a real one: Go's default "Go-http-client/2.0" is
// exactly the kind of string a bot filter rejects, and a rejection here looks
// like an authentication failure.
func responsesUserAgent() string {
	return fmt.Sprintf("rafiki/%s (%s; %s)", version.String(), runtime.GOOS, runtime.GOARCH)
}

// TokenSource supplies the credential for one Responses request. Token is
// called per request; force=true asks for a refreshed credential after a 401.
//
// headers is where a credential contributes more than a bearer token: the
// ChatGPT backend also wants the account id that the login belongs to, and
// emitting it here keeps the daemon's credential store (which knows the
// account) the only thing that has to know.
type TokenSource interface {
	Token(ctx context.Context, force bool) (bearer string, headers http.Header, err error)
}

// StaticToken is the api_key_env TokenSource; an empty key sends no
// Authorization header.
//
// force is ignored: a static key cannot be refreshed, and answering with the
// same key is what makes the sender's "retry once after 401" a single retry
// rather than a loop.
type StaticToken string

func (t StaticToken) Token(context.Context, bool) (string, http.Header, error) {
	return string(t), nil, nil
}

// ModelFact is what the serving account's /models entry says about one model.
//
// Every field is optional in the server's answer, and "absent" means "this
// deployment does not tell us", not "false" and not "empty". The sender's rule
// is to omit a field the server's answer does not mention rather than send a
// value the endpoint may reject: these four are exactly the fields codex gates
// its own request on from its /models response.
type ModelFact struct {
	SupportsReasoningSummary bool     // supports_reasoning_summary_parameter
	SupportsVerbosity        bool     // support_verbosity
	DefaultVerbosity         string   // default_verbosity
	ReasoningLevels          []string // supported_reasoning_levels[].effort, server order
}

// ModelFacts looks a model up in the owner's /models cache; ok=false when
// unknown. Nil is legal.
type ModelFacts func(model string) (ModelFact, bool)

// responsesSender is the Sender for providers.KindOpenAIResponses and
// providers.KindOpenAIResponsesChatGPT: the OpenAI Responses API, translated
// to/from the Anthropic Messages shape at this boundary so nothing above
// pkg/llm learns a second wire format.
//
// The two modes differ in exactly four ways, and each is a field here rather
// than a second sender type: the base URL (fixed for ChatGPT), the credential
// source (an owner's login versus an env key), the request body's ChatGPT
// constraints (store false, stream always true, zstd, no temperature), and the
// per-owner cookie jar for the Cloudflare challenge the chatgpt.com edge issues.
//
// Provider.SessionHeader is deliberately not read: unlike the kinds that go
// through a RoundTripper, this sender rewrites the session id into the request
// BODY (prompt_cache_key), which is where the Responses API takes it, and sends
// the fixed session-id header only in ChatGPT mode.
type responsesSender struct {
	baseURL string // scheme+host+path prefix, trailing "/" trimmed; never empty
	chatGPT bool

	tokens TokenSource // never nil: NewChatGPTResponsesSender refuses nil, newResponsesSender wraps the key
	facts  ModelFacts  // nil means nothing is known about any model

	client *http.Client

	// streamIdleTimeout is how long the stream decoder waits between events
	// before declaring the connection dead. A field (not the package constant)
	// so tests can shorten it.
	streamIdleTimeout time.Duration

	// turnState is x-codex-turn-state, the sticky-routing token the ChatGPT
	// backend issues and expects replayed within a turn. Codex scopes it to one
	// turn; this sender cannot see turn boundaries (a turn is a sequence of
	// Sends, and the same sender serves them all), so it keeps the latest value
	// per session, which spike 2 showed is accepted either way.
	mu        sync.Mutex
	turnState map[string]string
}

// newResponsesSender builds the KindOpenAIResponses sender, the api-key
// dialect. An empty BaseURL means api.openai.com.
func newResponsesSender(p providers.Provider, key string, rt http.RoundTripper) (*responsesSender, error) {
	base := p.BaseURL
	if base == "" {
		base = openAIResponsesBaseURL
	}
	return &responsesSender{
		baseURL:           strings.TrimRight(base, "/"),
		tokens:            StaticToken(key),
		client:            &http.Client{Transport: headerCaptureTransport{base: rt}},
		streamIdleTimeout: defaultResponsesStreamIdleTimeout,
	}, nil
}

// NewChatGPTResponsesSender builds the KindOpenAIResponsesChatGPT sender bound to
// one owner's TokenSource.
//
// It returns the Sender interface because that is what a caller wiring a
// provider table holds, but the value also satisfies StreamingSender: ChatGPT
// mode is stream-only (the endpoint refuses store:true, and New assembles the
// streamed events into a message).
//
// Each sender owns its own cookie jar. The chatgpt.com edge answers a challenge
// with a Set-Cookie the follow-up request must replay; a jar shared between
// senders would let one owner's cookies ride another owner's requests, which is
// the same class of leak as sharing the token.
func NewChatGPTResponsesSender(p providers.Provider, ts TokenSource, facts ModelFacts, rt http.RoundTripper) (Sender, error) {
	if ts == nil {
		return nil, fmt.Errorf("llm: provider %q: kind %q needs a token source", p.Name, p.Kind)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("llm: provider %q: cookie jar: %w", p.Name, err)
	}
	return &responsesSender{
		baseURL:           chatGPTBaseURL,
		chatGPT:           true,
		tokens:            ts,
		facts:             facts,
		client:            &http.Client{Transport: headerCaptureTransport{base: rt}, Jar: jar},
		streamIdleTimeout: defaultResponsesStreamIdleTimeout,
	}, nil
}

// New issues one Responses call and translates the response into an
// anthropic.Message.
//
// In ChatGPT mode there is no non-streamed call to make (store must be false and
// the endpoint always streams), so New opens the stream and accumulates it: the
// caller gets the same fully-formed message either way.
func (s *responsesSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	if s.chatGPT {
		return s.newFromStream(ctx, params)
	}
	req, resp, err := s.post(ctx, params, false)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, responsesMaxLineBytes))
	resp.Body.Close()
	if err := s.errorFor(req, resp, body); err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, fmt.Errorf("llm: openai-responses: read response: %w", readErr)
	}
	var wire responsesResponseObject
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("llm: openai-responses: decode response (status %d): %w: %.200s", resp.StatusCode, err, body)
	}
	return parseResponsesResponse(&wire)
}

// newFromStream is ChatGPT mode's New: stream, accumulate, and hand back the
// message. The stream's own error is returned verbatim so the classifiers above
// this package see the same values the streaming path produces.
func (s *responsesSender) newFromStream(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	stream, err := s.NewStreaming(ctx, params)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	acc := anthropic.Message{}
	for stream.Next() {
		ev := stream.Current()
		FixEmptyToolInput(&ev)
		SanitizeInvalidAccumulatedInput(&acc, ev)
		if aerr := acc.Accumulate(ev); aerr != nil {
			return nil, fmt.Errorf("llm: openai-responses: accumulate stream event: %w", aerr)
		}
		FixAccumulatedEmptyToolInput(&acc, ev)
		backfillDeltaUsage(&acc, ev)
	}
	if serr := stream.Err(); serr != nil {
		return nil, serr
	}
	return &acc, nil
}

// NewStreaming opens one Responses call with "stream": true and returns the
// response translated into the Anthropic stream event sequence.
func (s *responsesSender) NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	req, resp, err := s.post(ctx, params, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain before closing so the connection stays reusable. The body is
		// what a usage limit is recognized by, so it is read rather than
		// discarded.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if uerr := s.errorFor(req, resp, body); uerr != nil {
			return nil, uerr
		}
		// errorFor only returns nil for a 2xx; defensive.
		return nil, &anthropic.Error{StatusCode: resp.StatusCode, Request: req, Response: resp}
	}
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](newResponsesSSEDecoder(resp.Body, s.streamIdleTimeout), nil), nil
}

// post issues one request, retrying once after a 401 with a forced credential
// refresh. A second 401 is returned as-is: a credential the token source cannot
// fix is a configuration problem, and a third attempt would only spend more time
// reaching the same answer.
func (s *responsesSender) post(ctx context.Context, params anthropic.MessageNewParams, stream bool) (*http.Request, *http.Response, error) {
	sessionID, _ := sessionIDFromContext(ctx)
	body, err := buildResponsesRequestBody(params, responsesBuildOptions{
		chatGPT:    s.chatGPT,
		stream:     stream,
		sessionID:  sessionID,
		devMessage: s.chatGPT && chatGPTInstructionsAsDeveloperMessage,
		facts:      s.facts,
	})
	if err != nil {
		return nil, nil, err
	}
	payload, err := s.encodeBody(body)
	if err != nil {
		return nil, nil, err
	}

	var req *http.Request
	for attempt := 0; attempt < 2; attempt++ {
		bearer, extra, terr := s.tokens.Token(ctx, attempt > 0)
		if terr != nil {
			return nil, nil, fmt.Errorf("llm: openai-responses: resolve credential: %w", terr)
		}
		req, err = s.newRequest(ctx, payload, stream, bearer, extra, sessionID)
		if err != nil {
			return nil, nil, err
		}
		resp, derr := s.client.Do(req)
		if derr != nil {
			return nil, nil, fmt.Errorf("llm: openai-responses: post responses: %w", derr)
		}
		s.rememberTurnState(sessionID, resp.Header)
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return req, resp, nil
		}
		// First 401: drain and close so the connection stays reusable, ask the
		// token source for a forced refresh, and try once more.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
	return req, nil, fmt.Errorf("llm: openai-responses: post responses: unauthorized")
}

// encodeBody compresses the request body when the mode needs it. The ChatGPT
// endpoint accepts plain JSON (spike 3) and zstd is an optimization, but it is
// the shape codex sends and it keeps a large tool schema from being the slowest
// part of a turn.
//
// Accept-Encoding is never set by hand anywhere in this sender: doing so turns
// off net/http's transparent gzip decompression, which would hand raw compressed
// bytes to the response parser.
func (s *responsesSender) encodeBody(body []byte) ([]byte, error) {
	if !s.chatGPT {
		return body, nil
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("llm: openai-responses: zstd encoder: %w", err)
	}
	defer enc.Close()
	return enc.EncodeAll(body, nil), nil
}

// newRequest builds one request with every header the mode requires.
func (s *responsesSender) newRequest(ctx context.Context, payload []byte, stream bool, bearer string, extra http.Header, sessionID string) (*http.Request, error) {
	url := s.baseURL + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: openai-responses: build request: %w", err)
	}
	// TokenSource headers first: the ones below are this package's contract and
	// a credential must not be able to redefine them.
	for name, values := range extra {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	// Keyless convention: an empty bearer sends no Authorization header at all.
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("originator", "rafiki")
	req.Header.Set("User-Agent", responsesUserAgent())
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if s.chatGPT {
		req.Header.Set("Content-Encoding", "zstd")
		if sessionID != "" {
			req.Header.Set("session-id", sessionID)
			if ts := s.turnStateFor(sessionID); ts != "" {
				req.Header.Set("x-codex-turn-state", ts)
			}
		}
	}
	return req, nil
}

// errorFor classifies a non-2xx response. A subscription usage limit becomes its
// own terminal type; everything else is an *anthropic.Error carrying the status,
// exactly what openAISender.New returns, so pkg/agentloop's retry classification
// works unchanged.
func (s *responsesSender) errorFor(req *http.Request, resp *http.Response, body []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if u := responsesUsageLimitError(resp.StatusCode, resp.Header, body); u != nil {
		return u
	}
	return &anthropic.Error{StatusCode: resp.StatusCode, Request: req, Response: resp}
}

// rememberTurnState records x-codex-turn-state for the session it arrived on.
func (s *responsesSender) rememberTurnState(sessionID string, h http.Header) {
	if sessionID == "" || h == nil {
		return
	}
	v := h.Get("x-codex-turn-state")
	if v == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnState == nil {
		s.turnState = make(map[string]string)
	}
	s.turnState[sessionID] = v
}

// turnStateFor reads the remembered turn state for a session.
func (s *responsesSender) turnStateFor(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnState[sessionID]
}

// ---- request shape ----

// responsesBuildOptions is everything the request's SHAPE depends on, kept
// together because each field is a decision the two modes make differently.
type responsesBuildOptions struct {
	chatGPT    bool   // the ChatGPT-subscription dialect
	stream     bool   // NewStreaming (always true in ChatGPT mode)
	sessionID  string // prompt_cache_key, and in ChatGPT mode the session-id header
	devMessage bool   // system prompt as a developer input item
	facts      ModelFacts
}

// buildResponsesRequest is the three-argument form of the request builder, for
// the shape a ChatGPT-mode call always has: streaming.
func buildResponsesRequest(params anthropic.MessageNewParams, chatgpt bool, sessionID string) ([]byte, error) {
	return buildResponsesRequestBody(params, responsesBuildOptions{
		chatGPT:    chatgpt,
		stream:     chatgpt,
		sessionID:  sessionID,
		devMessage: chatgpt && chatGPTInstructionsAsDeveloperMessage,
	})
}

// buildResponsesRequestBody translates MessageNewParams into a Responses request
// body.
//
// The shape is the STANDARD Responses shape: top-level instructions and tools,
// function_call items, no codex dialect. That is not a stylistic preference: the
// endpoint's lite dialect is selected by the
// x-openai-internal-codex-responses-lite header and imposes requirements
// (reasoning.context all_turns, parallel_tool_calls false, function tools
// replaced by an additional_tools item) that a standard body contradicts. Spike 2
// confirmed the endpoint accepts the standard shape.
func buildResponsesRequestBody(params anthropic.MessageNewParams, opts responsesBuildOptions) ([]byte, error) {
	var fact ModelFact
	var haveFact bool
	if opts.facts != nil {
		fact, haveFact = opts.facts(string(params.Model))
	}

	req := responsesRequest{
		Model:             string(params.Model),
		Input:             []any{},
		ParallelToolCalls: true,
		Stream:            opts.stream || opts.chatGPT,
		MaxOutputTokens:   params.MaxTokens,
	}
	if opts.chatGPT {
		store := false
		req.Store = &store
	}

	// Anthropic's system is a top-level field; Responses has both a top-level
	// instructions string and (as current codex uses) a developer input item.
	system := responsesSystemText(params.System)
	switch {
	case opts.devMessage:
		if system != "" {
			req.Input = append(req.Input, responsesMessage{
				Type:    "message",
				Role:    "developer",
				Content: []any{responsesTextPart{Type: "input_text", Text: system}},
			})
		}
	case system != "" || opts.chatGPT:
		// ChatGPT mode always sends the key (empty when there is no system
		// prompt): the endpoint's own request shape has it, and an absent
		// instructions field reads as "use the default", not "no system
		// prompt".
		req.Instructions = &system
	}

	// Foreign thinking has no meaning for this endpoint and a Responses
	// reasoning id means nothing to Anthropic; StripForeignThinking is the one
	// place that decides which is which.
	for _, mp := range StripForeignThinking(params.Messages, true) {
		items, err := responsesMessageItems(&mp)
		if err != nil {
			return nil, err
		}
		req.Input = append(req.Input, items...)
	}

	for _, t := range params.Tools {
		if t.OfTool == nil {
			// Server-side tools (web_search, bash_20250124, ...) have no
			// function definition to send.
			slog.Debug("llm: openai-responses: dropping tool with no function equivalent")
			continue
		}
		tp := t.OfTool
		def := responsesFunctionTool{
			Type: "function",
			Name: tp.Name,
			// strict:false is what the fixture shape carries: strict mode is a
			// separate, schema-rewriting feature, and a caller that wanted it
			// has no way to ask through the Anthropic tool shape.
			Strict:     false,
			Parameters: responsesToolSchema(tp.InputSchema),
		}
		if tp.Description.Valid() {
			def.Description = tp.Description.Value
		}
		req.Tools = append(req.Tools, def)
	}
	// tool_choice without tools is an API error, so it is only sent when at
	// least one function tool survived translation.
	if len(req.Tools) > 0 {
		req.ToolChoice = translateResponsesToolChoice(params.ToolChoice)
	}

	effort, reasoning := responsesEffort(params.Thinking, fact, haveFact)
	if reasoning {
		r := responsesReasoning{Effort: effort}
		// summary is available on openai-responses, and on the ChatGPT
		// dialect only for models whose /models entry offers it.
		if !opts.chatGPT || fact.SupportsReasoningSummary {
			r.Summary = "auto"
		}
		req.Reasoning = &r
	}
	// Encrypted reasoning content is what makes a reasoning item resendable, so
	// it is requested whenever reasoning is, and always in ChatGPT mode (where
	// the endpoint emits reasoning items regardless of our effort choice).
	if reasoning || opts.chatGPT {
		req.Include = []string{"reasoning.encrypted_content"}
	}
	// verbosity is a ChatGPT-dialect field: it is sent only for a model whose
	// /models entry declares support AND a default, since an empty verbosity is
	// not a value the endpoint can act on.
	if opts.chatGPT && fact.SupportsVerbosity && fact.DefaultVerbosity != "" {
		req.Text = &responsesText{Verbosity: fact.DefaultVerbosity}
	}
	// temperature is not a supported parameter on the ChatGPT dialect (400
	// "Unsupported parameter: temperature"), so it is never sent there.
	if !opts.chatGPT && params.Temperature.Valid() {
		t := params.Temperature.Value
		req.Temperature = &t
	}
	if opts.sessionID != "" {
		req.PromptCacheKey = opts.sessionID
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: openai-responses: marshal request: %w", err)
	}
	return body, nil
}

// responsesSystemText joins the system blocks with a blank line. One string, not
// one per block: the Responses API has a single system field either way.
func responsesSystemText(blocks []anthropic.TextBlockParam) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// responsesToolSchema renders a tool's input schema. Only the three keys every
// input schema in this codebase declares are carried: an Anthropic tool has no
// place to put JSON-schema keywords the API does not model, so inventing them
// (additionalProperties: false, for instance) would silently narrow a caller's
// schema.
func responsesToolSchema(s anthropic.ToolInputSchemaParam) map[string]any {
	schema := map[string]any{"type": "object"}
	if v := string(s.Type); v != "" {
		schema["type"] = v
	}
	if s.Properties != nil {
		schema["properties"] = s.Properties
	}
	if len(s.Required) > 0 {
		schema["required"] = s.Required
	}
	return schema
}

// translateResponsesToolChoice maps Anthropic's tool_choice onto the Responses
// spelling: auto, required, none, or {"type":"function","name":…}. A zero union
// maps to nil, which omits the field. Same mapping as translateOpenAIToolChoice
// except the named form, which Responses spells without the nested function
// object.
func translateResponsesToolChoice(tc anthropic.ToolChoiceUnionParam) any {
	switch {
	case tc.OfAuto != nil:
		return "auto"
	case tc.OfAny != nil:
		return "required"
	case tc.OfTool != nil:
		return responsesToolChoiceFunction{Type: "function", Name: tc.OfTool.Name}
	case tc.OfNone != nil:
		return "none"
	default:
		return nil
	}
}

// responsesEffortOrder is fundi's escalating ladder, lowest first. It is both
// the clamp order and the vocabulary the endpoint's /models entries use.
var responsesEffortOrder = []string{"minimal", "low", "medium", "high", "xhigh"}

// responsesEffort resolves one thinking budget to a reasoning effort, and says
// whether reasoning was requested at all.
//
// The budget thresholds are fundi's own levels (low 4096, medium 8192, high
// 16384, xhigh 32768). When the model's facts are known its supported levels
// win: the desired level if listed, else the highest listed level below it, else
// the lowest listed level. Without facts the only correction is xhigh to high,
// because xhigh is a codex-dialect level a plain api.openai.com model may not
// accept.
func responsesEffort(t anthropic.ThinkingConfigParamUnion, fact ModelFact, haveFact bool) (string, bool) {
	if t.OfEnabled == nil {
		// Disabled, absent, or adaptive (the model picks its own effort): no
		// reasoning block to send.
		return "", false
	}
	b := t.OfEnabled.BudgetTokens
	desired := "xhigh"
	switch {
	case b <= 4096:
		desired = "low"
	case b <= 8192:
		desired = "medium"
	case b <= 16384:
		desired = "high"
	}
	if haveFact && len(fact.ReasoningLevels) > 0 {
		return clampResponsesEffort(desired, fact.ReasoningLevels), true
	}
	if desired == "xhigh" {
		desired = "high"
	}
	return desired, true
}

// clampResponsesEffort keeps desired if levels supports it, else lowers to the
// highest supported level below it, else the lowest supported level. levels
// arrives in the server's order; the clamp walks responsesEffortOrder so an
// unfamiliar level name in the middle cannot reorder the ladder.
func clampResponsesEffort(desired string, levels []string) string {
	if slices.Contains(levels, desired) {
		return desired
	}
	desiredRank := slices.Index(responsesEffortOrder, desired)
	if desiredRank < 0 {
		desiredRank = len(responsesEffortOrder) - 1
	}
	best := ""
	bestRank := -1
	lowest := ""
	lowestRank := len(responsesEffortOrder)
	for _, l := range levels {
		rank := slices.Index(responsesEffortOrder, l)
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

// responsesMessageItems translates one Anthropic message into Responses input
// items, preserving block order.
//
// tool_use becomes function_call with NO id key, ever. The endpoint's own item
// ids are fc_… and it rejects any other prefix (a resent id that came from
// Anthropic after a failover would be toolu_…), while it accepts a function_call
// without one. The id an Anthropic tool_use block HAS is the endpoint's call_id,
// which is what correlates the call with its output; the separate fc_… item id
// has nowhere to live in an Anthropic tool_use block and is discarded.
func responsesMessageItems(mp *anthropic.MessageParam) ([]any, error) {
	var items []any
	var textParts []any
	role := "user"
	partType := "input_text"
	if mp.Role == "assistant" {
		role = "assistant"
		partType = "output_text"
	} else if mp.Role != "user" {
		return nil, fmt.Errorf("llm: openai-responses: message role %q has no Responses equivalent", mp.Role)
	}
	flush := func() {
		if len(textParts) == 0 {
			return
		}
		items = append(items, responsesMessage{Type: "message", Role: role, Content: textParts})
		textParts = nil
	}

	for i := range mp.Content {
		b := &mp.Content[i]
		switch {
		case b.OfText != nil:
			if b.OfText.Text == "" {
				continue
			}
			textParts = append(textParts, responsesTextPart{Type: partType, Text: b.OfText.Text})
		case b.OfImage != nil:
			img, err := responsesImagePart(b.OfImage)
			if err != nil {
				return nil, err
			}
			textParts = append(textParts, img)
		case b.OfThinking != nil:
			id, blob, ok := ParseResponsesSignature(b.OfThinking.Signature)
			if !ok || blob == "" {
				// Either not our reasoning (dropped by StripForeignThinking
				// already, so this is a block that names no item) or a
				// reasoning item whose encrypted content is empty: an empty blob
				// carries nothing, so resending it only costs tokens.
				continue
			}
			flush()
			summary := []responsesSummaryPart{}
			if txt := b.OfThinking.Thinking; txt != "" {
				summary = append(summary, responsesSummaryPart{Type: "summary_text", Text: txt})
			}
			items = append(items, responsesReasoningItem{
				Type:             "reasoning",
				ID:               id,
				EncryptedContent: blob,
				Summary:          summary,
			})
		case b.OfToolUse != nil:
			if role != "assistant" {
				return nil, fmt.Errorf("llm: openai-responses: tool_use in a %q message", mp.Role)
			}
			args, err := openAIToolArguments(b.OfToolUse.Input)
			if err != nil {
				return nil, fmt.Errorf("llm: openai-responses: tool_use %q: %w", b.OfToolUse.ID, err)
			}
			flush()
			items = append(items, responsesFunctionCall{
				Type:      "function_call",
				CallID:    b.OfToolUse.ID,
				Name:      b.OfToolUse.Name,
				Arguments: args,
			})
		case b.OfToolResult != nil:
			flush()
			items = append(items, responsesFunctionCallOutputFrom(b.OfToolResult))
		default:
			logDroppedResponsesBlock(b, string(mp.Role))
		}
	}
	flush()
	return items, nil
}

// responsesFunctionCallOutputFrom renders one tool result. Text-only results
// carry a plain string; a result with an image carries the array form, which the
// endpoint accepts (spike 5) and which is the only way to send an image back.
func responsesFunctionCallOutputFrom(tr *anthropic.ToolResultBlockParam) responsesFunctionCallOutput {
	var text strings.Builder
	var parts []any
	for _, c := range tr.Content {
		switch {
		case c.OfText != nil:
			text.WriteString(c.OfText.Text)
			if c.OfText.Text != "" {
				parts = append(parts, responsesTextPart{Type: "input_text", Text: c.OfText.Text})
			}
		case c.OfImage != nil:
			img, err := responsesImagePart(c.OfImage)
			if err != nil {
				// An unrenderable image is dropped from the array form rather
				// than failing the turn: the text of the result still reaches
				// the model, which is the whole point of the tool result.
				logDroppedResponsesBlock(&anthropic.ContentBlockParamUnion{OfImage: c.OfImage}, "tool_result")
				continue
			}
			parts = append(parts, img)
		default:
			logDroppedResponsesBlock(&anthropic.ContentBlockParamUnion{OfText: c.OfText}, "tool_result")
		}
	}
	out := responsesFunctionCallOutput{Type: "function_call_output", CallID: tr.ToolUseID}
	if parts != nil && containsImagePart(parts) {
		out.Output = parts
	} else {
		out.Output = text.String()
	}
	return out
}

// containsImagePart reports whether any part of a tool result is an image, which
// is what selects the array form of "output".
func containsImagePart(parts []any) bool {
	for _, p := range parts {
		if _, ok := p.(responsesInputImage); ok {
			return true
		}
	}
	return false
}

// responsesImagePart renders an Anthropic image block as a data URL, which is the
// only image form the Responses input accepts for a base64 source. A URL source
// passes through unchanged.
func responsesImagePart(b *anthropic.ImageBlockParam) (responsesInputImage, error) {
	if u := b.Source.OfURL; u != nil && u.URL != "" {
		return responsesInputImage{Type: "input_image", ImageURL: u.URL}, nil
	}
	if s := b.Source.OfBase64; s != nil && s.Data != "" {
		media := string(s.MediaType)
		if media == "" {
			media = "image/png"
		}
		return responsesInputImage{
			Type:     "input_image",
			ImageURL: "data:" + media + ";base64," + s.Data,
		}, nil
	}
	return responsesInputImage{}, fmt.Errorf("llm: openai-responses: image block has no usable source")
}

// logDroppedResponsesBlock reports a content block this translation cannot
// represent. Images and documents carry user content, so losing them is worth a
// warning; thinking blocks are routine and carry no user content.
func logDroppedResponsesBlock(b *anthropic.ContentBlockParamUnion, role string) {
	name := "unknown"
	if t := b.GetType(); t != nil && *t != "" {
		name = *t
	}
	loud := name == "image" || name == "document" || name == "search_result"
	msg := "llm: openai-responses: dropping content block with no Responses equivalent"
	if loud {
		slog.Warn(msg, "role", role, "type", name)
		return
	}
	slog.Debug(msg, "role", role, "type", name)
}

// ---- wire shapes of a Responses request ----

// responsesRequest is one POST /responses body. Absent fields are absent for a
// reason and each is documented above at its assignment site.
type responsesRequest struct {
	Model             string                  `json:"model"`
	Instructions      *string                 `json:"instructions,omitempty"`
	Input             []any                   `json:"input"`
	Tools             []responsesFunctionTool `json:"tools,omitempty"`
	ToolChoice        any                     `json:"tool_choice,omitempty"`
	ParallelToolCalls bool                    `json:"parallel_tool_calls"`
	Reasoning         *responsesReasoning     `json:"reasoning,omitempty"`
	Text              *responsesText          `json:"text,omitempty"`
	MaxOutputTokens   int64                   `json:"max_output_tokens"`
	Temperature       *float64                `json:"temperature,omitempty"`
	Stream            bool                    `json:"stream"`
	Store             *bool                   `json:"store,omitempty"`
	Include           []string                `json:"include,omitempty"`
	PromptCacheKey    string                  `json:"prompt_cache_key,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
	// Summary is "auto" only where the model's facts allow it. context is never
	// sent: codex sends it only in the lite dialect, and the standard shape
	// omits it.
	Summary string `json:"summary,omitempty"`
}

type responsesText struct {
	Verbosity string `json:"verbosity"`
}

type responsesFunctionTool struct {
	Type        string         `json:"type"` // always "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"`
}

// responsesToolChoiceFunction is the named-tool spelling: no nested function
// object, unlike Chat Completions.
type responsesToolChoiceFunction struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// responsesMessage is an input item carrying conversation content: role user or
// assistant for the transcript, developer for the system prompt's alternative
// placement.
type responsesMessage struct {
	Type    string `json:"type"` // "message"
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// responsesTextPart is one content part of a message item. The type differs by
// role: input_text for a user or developer message, output_text for an
// assistant's own words.
type responsesTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesInputImage struct {
	Type     string `json:"type"` // "input_image"
	ImageURL string `json:"image_url"`
}

// responsesReasoningItem resends one reasoning item from the transcript. id and
// encrypted_content are the two halves of this package's thinking signature;
// summary echoes the thinking text so a human reading the transcript sees what
// the item's summary was.
type responsesReasoningItem struct {
	Type             string                 `json:"type"` // "reasoning"
	ID               string                 `json:"id"`
	EncryptedContent string                 `json:"encrypted_content"`
	Summary          []responsesSummaryPart `json:"summary"`
}

// responsesFunctionCall is a resent tool call. It deliberately has no id field:
// see responsesMessageItems.
type responsesFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responsesFunctionCallOutput is one tool result. Output is a string for a text
// result and an array of input parts when the result carries an image.
type responsesFunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}
