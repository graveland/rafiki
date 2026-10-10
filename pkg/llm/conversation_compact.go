// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/store"
)

// compact summarises history and moves the conversation's working-set horizon
// to the summary, keeping a verbatim tail beside it. It returns ok=false when
// nothing was written — too few rows, a summary call that failed, a truncated
// summary, or no extractable summary — and the caller then continues on the
// unchanged history.
//
// Nothing is ever deleted or renumbered: the summary and the tail copies are
// appended at fresh ordinals and only the horizon moves. A failed attempt
// leaves no partial state; an End event is still emitted so observers stay
// balanced with the Start event (with zero tokens on failure).
func (conv *Conversation) compact(ctx context.Context, span trace.Span, history []store.Message, scfg sendConfig, trigger, instructions string) (bool, error) {
	p := *conv.cfg.compaction
	if conv.onCompact != nil {
		conv.onCompact(CompactionEvent{Phase: CompactionStart, Trigger: trigger})
	}
	pre, post, succeeded := 0, 0, false
	defer func() {
		if conv.onCompact != nil {
			conv.onCompact(CompactionEvent{Phase: CompactionEnd, Trigger: trigger, PreTokens: pre, PostTokens: post, Succeeded: succeeded})
		}
	}()

	// Build the summary request from the SAME message list a real turn would
	// send, then append the prompt to the trailing user message. Clone before
	// appending: the trailing row's Content shares its backing array with the
	// stored history.
	prompt := compactionPrompt
	if instructions != "" {
		prompt += "\n\nAdditional instructions for the summary:\n" + instructions
	}
	reqMsgs := mergeForRequest(history)
	last := &reqMsgs[len(reqMsgs)-1]
	if last.Role == anthropic.MessageParamRoleUser {
		last.Content = append(slices.Clone(last.Content), anthropic.NewTextBlock(prompt))
	} else {
		reqMsgs = append(reqMsgs, anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)))
	}

	// The summary call keeps the previous turn's tools, system prompt and
	// thinking config byte-identical so the cached prefix hits. Only the output
	// cap, source, tool choice and the streaming/batch hooks change.
	sc := scfg
	sc.streamHandler = nil
	sc.batchWait = nil
	sc.source = "compaction"
	sc.toolChoice = ""
	base := p.SummaryMaxTokens
	if m := conv.cfg.maxTokens; m > 0 && m < base {
		base = m
	}
	sc.maxTokens = base + conv.cfg.thinkingBudget

	meta := conv.sendMeta(nextOrdinal(history), sc)
	params := conv.assemble(reqMsgs, sc)
	resp, _, err := conv.sendAttempt(ctx, meta, params, nil)
	if err != nil {
		return false, err
	}

	// A summary cut off by max_tokens is never stored: an unclosed <summary>
	// tag would otherwise be accepted downstream as a complete handover.
	if resp.StopReason == anthropic.StopReasonMaxTokens {
		conv.client.logger.Warn("compaction summary truncated", "conversation", conv.ID, "trigger", trigger)
		return false, nil
	}

	// Text blocks only: the summary call may emit tool_use blocks despite the
	// prompt, and those carry no summary.
	var text string
	for _, b := range resp.Content {
		if tb, ok := b.AsAny().(anthropic.TextBlock); ok {
			text += tb.Text
		}
	}
	summary, ok := extractSummary(text)
	if !ok {
		conv.client.logger.Warn("compaction produced no summary", "conversation", conv.ID, "trigger", trigger)
		return false, nil
	}

	replaced := int(resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens)
	start := cutTail(history, p.tailBudget())
	tail := history[start:]
	summaryRow := store.Message{
		Param:       summaryMessage(summary),
		Kind:        ptr(store.KindCompactionSummary),
		InputTokens: &replaced,
	}

	if conv.client.messages == nil {
		n := nextOrdinal(conv.mem)
		summaryRow.Ordinal = n
		conv.memHorizon = len(conv.mem)
		conv.mem = append(conv.mem, summaryRow)
		for i, t := range tail {
			conv.mem = append(conv.mem, store.Message{
				Ordinal:    n + 1 + i,
				Param:      t.Param,
				ToolUseIDs: t.ToolUseIDs,
				StopReason: t.StopReason,
				Kind:       ptr(store.KindCompactionTail),
			})
		}
	} else if _, err := conv.client.messages.AppendCompaction(ctx, conv.ID, summaryRow.Param, replaced, tail); err != nil {
		return false, err
	}

	span.AddEvent("compaction", trace.WithAttributes(
		attribute.String("rafiki.compaction.trigger", trigger),
		attribute.Int("rafiki.compaction.replaced_tokens", replaced),
		attribute.Int("rafiki.compaction.tail_rows", len(tail)),
	))
	conv.usedTokens = 0
	// A successful compaction clears any proactive-retry suppression: the
	// working set is fresh and small, so a stale suppression threshold from an
	// earlier failure must not block the next legitimate compaction.
	conv.compactSuppressBelow = 0
	pre = replaced
	post = estimateTokens(summaryRow) + estimateHistoryTokens(tail)
	succeeded = true
	return true, nil
}

// ptr returns a pointer to a copy of v, for the optional store.Message fields.
func ptr[T any](v T) *T { return &v }

// ErrCompactionDisabled is returned by Compact on a Conversation built without
// WithCompaction.
var ErrCompactionDisabled = errors.New("llm: compaction is not configured for this conversation")

// Compact summarises the working set now, whatever the thresholds say, with
// trigger "manual". opts must carry the same send options a real turn uses
// (tools, system prompt, thinking) so the summary call hits the cached prefix.
// instructions, when non-empty, is appended to the summary prompt.
//
// ok=false with a nil error means nothing was written: fewer than
// minCompactableRows working rows, or a summary that failed or was truncated
// (the Warn is logged by compact). It does not touch the proactive-retry
// suppression a failed threshold attempt sets.
func (conv *Conversation) Compact(ctx context.Context, instructions string, opts ...SendOption) (bool, error) {
	if conv.cfg.compaction == nil {
		return false, ErrCompactionDisabled
	}
	scfg := sendConfig{maxTokens: conv.cfg.maxTokens}
	for _, opt := range opts {
		opt(&scfg)
	}
	ctx, span := conv.client.tracer.Start(ctx, "llm.conversation.compact",
		trace.WithAttributes(attribute.String("rafiki.conversation", conv.ID)))
	defer span.End()

	history, err := conv.loadHistory(ctx)
	if err != nil {
		return false, fmt.Errorf("llm: %w", err)
	}
	if len(history) < minCompactableRows {
		return false, nil
	}
	return conv.compact(ctx, span, history, scfg, "manual", instructions)
}

// Clear restarts the working set at a fresh boundary row: nothing is deleted or
// renumbered, only the resume horizon moves. It is a no-op on a conversation
// with no working rows.
func (conv *Conversation) Clear(ctx context.Context) error {
	history, err := conv.loadHistory(ctx)
	if err != nil {
		return fmt.Errorf("llm: %w", err)
	}
	if len(history) == 0 {
		return nil
	}
	boundary := anthropic.NewUserMessage(anthropic.NewTextBlock(store.ClearBoundaryText))
	if conv.client.messages == nil {
		conv.memHorizon = len(conv.mem)
		conv.mem = append(conv.mem, store.Message{
			Ordinal: nextOrdinal(conv.mem),
			Param:   boundary,
			Kind:    ptr(store.KindClear),
		})
	} else if _, err := conv.client.messages.AppendClear(ctx, conv.ID, boundary); err != nil {
		return fmt.Errorf("llm: clear: %w", err)
	}
	conv.usedTokens = 0
	conv.compactSuppressBelow = 0
	return nil
}
