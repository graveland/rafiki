// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
type Batcher interface {
	Park(ctx context.Context, customID, model string, params anthropic.MessageNewParams) (*anthropic.Message, error)
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
// names no provider, so there is nothing to observe.
func (c *Client) parkSend(ctx context.Context, span trace.Span, meta SendMeta, params anthropic.MessageNewParams) (*anthropic.Message, error) {
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

	start := time.Now()
	resp, err := c.batcher.Park(ctx, BatchCustomID(convID, meta.Ordinal), string(params.Model), params)
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		span.RecordError(err)
		c.failTurn(ctx, ref.capturing, ref.turnID, ref.createdAt, err)
		c.recordRawTrace(ctx, meta, ref.turnID, params, nil, 0, string(params.Model), latency, err, hdrs)
		return nil, err
	}

	span.SetAttributes(
		attribute.Int64("rafiki.tokens.input", resp.Usage.InputTokens),
		attribute.Int64("rafiki.tokens.output", resp.Usage.OutputTokens),
		attribute.Int64("rafiki.tokens.cache_read", resp.Usage.CacheReadInputTokens),
		attribute.Int64("rafiki.tokens.cache_creation", resp.Usage.CacheCreationInputTokens),
	)
	if ref.capturing {
		c.completeTurn(ctx, ref.turnID, ref.createdAt, resp, string(params.Model), latency)
	}
	c.recordRawTrace(ctx, meta, ref.turnID, params, resp, 200, string(params.Model), latency, nil, hdrs)
	return resp, nil
}
