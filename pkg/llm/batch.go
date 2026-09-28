// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// BatchSuffix marks an OpenRouter model id whose first call goes through the
// provider Batch API. A send PARKS iff the resolved model id ends in this
// suffix AND the request's messages contain no assistant-role message
// (firstCall); every later call of the same conversation has the suffix
// stripped and goes through the normal live sender.
const BatchSuffix = ":batch"

// IsBatchModel reports whether id carries the batch suffix.
func IsBatchModel(id string) bool { return strings.HasSuffix(id, BatchSuffix) }

// Batcher parks one request in a provider Batch API and blocks until its
// result is delivered, ctx is cancelled, or the batch fails. Implemented by
// pkg/batch; the method set is exactly this — pkg/llm must not import
// pkg/batch (interface only), and no batch DB or Batch-API HTTP lives here.
//
// Park MUST NOT return or wrap an *anthropic.Error, context.DeadlineExceeded,
// or a net/syscall error: a retryable error would make pkg/agentloop's
// isRetryable resubmit the batch up to 7 times. Terminal batch failures are a
// plain error.
//
// provider is the raw JSON of the NARROWED provider object the batch envelope
// carries at its TOP level — {"only": [...]} — or nil when nothing pins the
// call. The OpenRouter Batch wire accepts exactly provider.only (Task 0.1's
// probe: sort, data_collection, zdr and ignore are each a submit-time 400
// "Unrecognized key"), so this is never the live path's full provider prefs.
type Batcher interface {
	Park(ctx context.Context, customID, model string, params anthropic.MessageNewParams, provider json.RawMessage) (*anthropic.Message, error)
}

// BatchCustomID is "<conversationID>-<ordinal>" — the key a parked call is
// matched back by when batch results arrive OUT OF ORDER. The result must
// match Anthropic's `^[A-Za-z0-9_-]{1,64}$` custom_id charset (anything else
// is a 422 that fails the WHOLE batch); it only ever concatenates a UUID
// conversation id, a "-" and a decimal ordinal, and TestBatchCustomIDCharset
// pins the charset.
func BatchCustomID(conversationID string, ordinal int) string {
	return conversationID + "-" + strconv.Itoa(ordinal)
}

// WithBatcher installs the daemon's batcher. nil (the default) means a
// :batch first call fails with an error instead of parking.
func WithBatcher(b Batcher) ClientOption {
	return func(c *Client) { c.batcher = b }
}

// batchOnlyList computes the only-list a parked call's batch envelope pins
// serving with: the routing spec's Only when set, else the SAME pinOnly rule
// applyProviderPrefs implements — the alias's Only when non-empty, else the
// static pin's (routing.ProviderPrefsFor). model must be the id with the
// :batch suffix STRIPPED (a pin matches the model line, and ":batch" is not
// a line extension). nil means no opinion: the envelope carries no provider.
func batchOnlyList(spec routing.Spec, alias *providers.ModelAlias, model string) []string {
	if len(spec.Only) > 0 {
		return spec.Only
	}
	if alias != nil && len(alias.Only) > 0 {
		return alias.Only
	}
	pin, _ := routing.ProviderPrefsFor(model)
	return pin.Only
}

// batchProviderJSON renders the narrowed provider object the batch envelope
// carries: {"only": [...]}, or nil when the only-list is empty.
func batchProviderJSON(only []string) (json.RawMessage, error) {
	if len(only) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(struct {
		Only []string `json:"only"`
	}{Only: only})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// firstCall reports whether params contains no assistant-role message —
// i.e. this send is the conversation's first call, the one that parks.
func firstCall(params anthropic.MessageNewParams) bool {
	for _, m := range params.Messages {
		if m.Role == anthropic.MessageParamRoleAssistant {
			return false
		}
	}
	return true
}

// parkSend is SendParams's batch branch: the request parks with the batcher
// instead of going through a live sender. Runs only after prepareSend has
// established that params.Model ends in :batch (the park invariant) — the
// caller checked that, never re-derive it here.
//
// It skips the model gate, the breaker and the fallback chain entirely (a
// parked call names no provider to gate or fail over between), write-aheads
// the turn exactly as the live path does, and resolves the turn with the
// batch result. c.guard.Observe is deliberately NOT called: a batch response
// names no provider, so there is nothing to observe. Capture still records
// primary as the turn's upstream — the provider the send WOULD have gone
// live through — so conversation_turn.upstream stays a provider name on the
// batch path too (insights joins it, never a model id).
// batchOnly is the only-list batchOnlyList computed for this park (prepareSend
// resolves it while it still holds the alias); nil = no pin, no provider key.
func (c *Client) parkSend(ctx context.Context, span trace.Span, meta SendMeta, params anthropic.MessageNewParams, primary string, batchOnly []string) (*anthropic.Message, error) {
	if c.batcher == nil {
		return nil, errors.New("llm: " + string(params.Model) + ": no batcher configured")
	}
	if meta.ConversationID == "" {
		return nil, errors.New("llm: a :batch send needs a conversation id")
	}

	if meta.OnBatchWait != nil {
		meta.OnBatchWait(true)
		defer meta.OnBatchWait(false)
	}

	ref := c.beginTurn(ctx, meta, params)
	// ref.convID equals meta.ConversationID for a captured conversation; when
	// capture is off (store-less), beginTurn leaves it empty and the caller's
	// id is the only handle the custom_id can carry.
	convID := ref.convID
	if convID == "" {
		convID = meta.ConversationID
	}
	ctx = WithSessionID(ctx, ref.convID)
	ctx, hdrs := withRawTraceHeaders(ctx)

	provider, err := batchProviderJSON(batchOnly)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: marshal batch provider: %w", params.Model, err)
	}
	start := time.Now()
	resp, err := c.batcher.Park(ctx, BatchCustomID(convID, meta.Ordinal), string(params.Model), params, provider)
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		span.RecordError(err)
		c.failTurn(ctx, ref.capturing, ref.turnID, ref.createdAt, err)
		c.recordRawTrace(ctx, meta, ref.turnID, params, nil, 0, primary, latency, err, hdrs)
		return nil, err
	}

	span.SetAttributes(
		attribute.Int64("rafiki.tokens.input", resp.Usage.InputTokens),
		attribute.Int64("rafiki.tokens.output", resp.Usage.OutputTokens),
		attribute.Int64("rafiki.tokens.cache_read", resp.Usage.CacheReadInputTokens),
		attribute.Int64("rafiki.tokens.cache_creation", resp.Usage.CacheCreationInputTokens),
	)
	if ref.capturing {
		c.completeTurn(ctx, ref.turnID, ref.createdAt, resp, primary, latency)
	}
	c.recordRawTrace(ctx, meta, ref.turnID, params, resp, 200, primary, latency, nil, hdrs)
	return resp, nil
}
