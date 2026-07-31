// SPDX-License-Identifier: Apache-2.0

// Package agentloop layers tool-use loop primitives on llm.Conversation, so
// every consumer gets capture + failover + cache hygiene + durability without
// thinking about it. Ported from sc's diagnose loop: deterministic tool
// ordering, 50KB tool-result truncation, max-iterations cap, tool results in
// request order, prompt-too-large trim-retry (via the conversation's
// TrimPolicy).
//
// Recovery semantics (design, resolved 2026-07-10): Resume NEVER re-executes
// a tool whose tool_use has no persisted result — it injects a synthetic
// is_error tool_result (see InterruptedToolResult) and lets the model decide
// whether to re-run. A per-conversation attempt cap guards against crash
// loops where a tool itself kills the process.
package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"golang.org/x/sync/errgroup"

	"go.graveland.dev/rafiki/llm"
	"go.graveland.dev/rafiki/store"
)

const (
	maxIterations      = 20
	maxConcurrentTools = 6
	maxToolResultSize  = 50 * 1024

	// DefaultResumeCap bounds Resume attempts per conversation.
	DefaultResumeCap = 3
)

// ErrResumeCapExceeded is returned by Resume when the per-conversation
// attempt cap is exhausted; the conversation is marked failed.
var ErrResumeCapExceeded = errors.New("agentloop: resume attempt cap exceeded; conversation marked failed")

// ToolSet is host code: the tools the loop may execute. Definitions must
// return a deterministic order across calls (any stable order) — a reordered
// tools array silently busts the Anthropic prompt-cache prefix; prefix_hash
// on captured turns is the drift detector.
type ToolSet interface {
	Definitions() []anthropic.ToolUnionParam
	Execute(ctx context.Context, name string, input json.RawMessage) (string, error)
}

// Events are optional, nil-safe observation callbacks.
type Events struct {
	OnText       func(text string)
	OnToolCall   func(name string, input json.RawMessage)
	OnToolResult func(name string, result string, err error)
	// OnTurn fires after each LLM call with its usage, duration and error —
	// the host metrics hook (sc's ObserveClaudeRequest/RecordClaudeTokens).
	OnTurn func(resp *anthropic.Message, dur time.Duration, err error)
	// OnToolStart/OnToolEnd mirror OnToolCall/OnToolResult but carry the
	// tool_use id, so hosts can correlate execution events with the assistant
	// message's tool_use blocks.
	OnToolStart func(id, name string, input json.RawMessage)
	OnToolEnd   func(id, name, result string, err error)
	// PendingUser, when non-nil, is polled after each tool batch's results are
	// persisted and before the next Continue. Non-empty returned content is
	// appended as an additional user message — the mid-turn steer seam.
	PendingUser func() []anthropic.ContentBlockParamUnion
}

func (e *Events) text(t string) {
	if e != nil && e.OnText != nil {
		e.OnText(t)
	}
}

func (e *Events) toolCall(name string, input json.RawMessage) {
	if e != nil && e.OnToolCall != nil {
		e.OnToolCall(name, input)
	}
}

func (e *Events) toolResult(name, result string, err error) {
	if e != nil && e.OnToolResult != nil {
		e.OnToolResult(name, result, err)
	}
}

func (e *Events) toolStart(id, name string, input json.RawMessage) {
	if e != nil && e.OnToolStart != nil {
		e.OnToolStart(id, name, input)
	}
}

func (e *Events) toolEnd(id, name, result string, err error) {
	if e != nil && e.OnToolEnd != nil {
		e.OnToolEnd(id, name, result, err)
	}
}

type toolCallIDKey struct{}

// ToolCallID returns the tool_use id of the call being executed on this
// context, or "" when called outside a tool execution.
func ToolCallID(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}

func (e *Events) turn(resp *anthropic.Message, dur time.Duration, err error) {
	if e != nil && e.OnTurn != nil {
		e.OnTurn(resp, dur, err)
	}
}

// Stats mirrors the diagnose loop's run statistics.
type Stats struct {
	ToolCalls    int
	Iterations   int
	InputTokens  int64
	OutputTokens int64
}

// Result is a run outcome. On error, Run/Resume still return a non-nil
// Result carrying the partial Stats accumulated before the failure (hosts
// report token spend on failed runs too).
type Result struct {
	Text  string
	Stats Stats
}

// Run drives a fresh tool-use loop: append userContent, then iterate
// Continue → execute tools → persist results until end_turn or the
// iteration cap. opts are additional llm.SendOptions (e.g.
// llm.WithStreamHandler) forwarded to every conv.Continue call in the loop,
// after llm.WithTools(defs) — so a caller-supplied option can override tools
// if it ever needs to, but never displaces them by omission.
//
// opts are forwarded to EVERY Continue call in the loop, not just the first.
// A tool-calling run therefore delivers several assistant turns through one
// llm.WithStreamHandler with no boundary event between them; a consumer
// that concatenates handler output will glue one turn's prose onto the
// next. Use Events.OnTurn, which fires once per LLM call (see drive), as
// the turn boundary.
func Run(ctx context.Context, conv *llm.Conversation, tools ToolSet, ev *Events, userContent []anthropic.ContentBlockParamUnion, opts ...llm.SendOption) (*Result, error) {
	before, err := conv.History(ctx)
	if err != nil {
		return nil, err
	}
	ordinal := 0
	if len(before) > 0 {
		ordinal = before[len(before)-1].Ordinal + 1
	}
	if err := conv.AppendUser(ctx, userContent); err != nil {
		return nil, err
	}
	result, err := drive(ctx, conv, tools, ev, opts...)
	if err != nil && llm.IsPromptTooLarge(err) {
		// The very first call overflowed even after the library's own trim
		// retries (a too-large FIRST message is structurally untrimmable).
		// Retract the write-ahead user row so the host can rebuild a smaller
		// prompt and retry on the SAME conversation instead of minting a new
		// one per attempt. Only when nothing was persisted after it.
		if after, hErr := conv.History(ctx); hErr == nil && len(after) == len(before)+1 {
			if rErr := conv.RetractTailUser(ctx, ordinal); rErr != nil {
				return result, fmt.Errorf("%w (retracting oversized prompt also failed: %v)", err, rErr)
			}
		}
	}
	return result, err
}

// Resume recovers an interrupted loop from persisted state:
//
//   - orphaned tool_use blocks (no persisted tool_result) get synthetic
//     is_error results — NEVER re-execution — appended after any persisted
//     partial results (per-tool persistence is in tool_use order, so the
//     merged wire message stays in that order);
//   - a pending turn (call never completed) is simply re-issued via Continue;
//   - a conversation whose last message is a clean end_turn assistant text is
//     already complete and returns immediately (a max_tokens-truncated tail
//     is NOT complete — it re-issues via Continue);
//   - a successful resume resets the attempt counter: the cap bounds
//     CONSECUTIVE failed recoveries, not lifetime interruptions.
//
// Each Resume bumps the conversation's attempt counter; past DefaultResumeCap
// the conversation is marked failed and ErrResumeCapExceeded returned. opts
// are forwarded to drive exactly as in Run.
func Resume(ctx context.Context, conv *llm.Conversation, tools ToolSet, ev *Events, opts ...llm.SendOption) (*Result, error) {
	attempts, err := conv.IncrementResumeAttempts(ctx)
	if err != nil {
		return nil, err
	}
	if attempts > DefaultResumeCap {
		if mErr := conv.MarkFailed(ctx); mErr != nil {
			return nil, fmt.Errorf("%w (and marking failed also failed: %v)", ErrResumeCapExceeded, mErr)
		}
		return nil, ErrResumeCapExceeded
	}

	history, err := conv.History(ctx)
	if err != nil {
		return nil, err
	}
	if len(history) == 0 {
		return nil, errors.New("agentloop: nothing to resume (no persisted messages)")
	}

	// Fabricate synthetic results for orphaned tool_use blocks of the LAST
	// assistant tool_use message. This read (History above) plus the
	// AppendUser writes below are NOT concurrency-safe: two Resume calls
	// racing on the same conversation can each observe the same orphan
	// before either has persisted its synthetic result, fabricating it
	// twice (see TestConcurrentResume, which documents this as a known,
	// currently unfixed race — it is NOT an emergent "once per id"
	// property of persistence alone). That is acceptable today only
	// because Resume has no production caller in either rafiki or fundi;
	// fundi's live orphan-fabrication path is a separate implementation
	// (internal/agent/orphans.go's RepairOrphans) with its own reachability
	// analysis. If Resume ever gains a caller that can run concurrently for
	// the same conversation, this needs external serialization before that
	// lands — do not assume this loop is safe without it.
	orphans, lastAssistantClean := analyzeOrphans(history)
	for _, o := range orphans {
		synthetic := []anthropic.ContentBlockParamUnion{
			anthropic.NewToolResultBlock(o.id, InterruptedToolResult(o.name, o.input), true),
		}
		if err := conv.AppendUser(ctx, synthetic); err != nil {
			return nil, fmt.Errorf("agentloop: persist synthetic result: %w", err)
		}
	}

	if len(orphans) == 0 && lastAssistantClean != nil {
		// Already complete: surface the final text without another call.
		resetAttempts(ctx, conv)
		return &Result{Text: *lastAssistantClean}, nil
	}
	result, err := drive(ctx, conv, tools, ev, opts...)
	if err == nil {
		resetAttempts(ctx, conv)
	}
	return result, err
}

// resetAttempts is best-effort: a failed reset must not fail a successful
// resume; it only means the counter stays conservative.
func resetAttempts(ctx context.Context, conv *llm.Conversation) {
	_ = conv.ResetResumeAttempts(ctx)
}

type orphan struct {
	id    string
	name  string
	input json.RawMessage
}

// analyzeOrphans finds tool_use blocks of the last assistant message that
// have no tool_result row anywhere after it (in block order). When the last
// assistant message is a clean end-of-conversation text (no tool_use, final
// row, stop reason end_turn), its text is returned instead — a truncated or
// unknown stop reason is never treated as completion.
func analyzeOrphans(history []store.Message) ([]orphan, *string) {
	lastAssistant := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Param.Role == anthropic.MessageParamRoleAssistant {
			lastAssistant = i
			break
		}
	}
	if lastAssistant == -1 {
		return nil, nil
	}

	resolved := map[string]bool{}
	for _, m := range history[lastAssistant+1:] {
		if m.Param.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for _, id := range m.ToolUseIDs {
			resolved[id] = true
		}
	}

	var orphans []orphan
	hasToolUse := false
	var text string
	for _, block := range history[lastAssistant].Param.Content {
		if tu := block.OfToolUse; tu != nil {
			hasToolUse = true
			if !resolved[tu.ID] {
				input, _ := json.Marshal(tu.Input)
				orphans = append(orphans, orphan{id: tu.ID, name: tu.Name, input: input})
			}
		}
		if tb := block.OfText; tb != nil && text == "" {
			text = tb.Text
		}
	}
	if !hasToolUse && lastAssistant == len(history)-1 &&
		history[lastAssistant].StopReason == "end_turn" {
		return nil, &text
	}
	return orphans, nil
}

// drive is the shared loop body: Continue with tools, execute any requested
// batch, persist results per tool in tool_use order, repeat until end_turn.
// opts are extra llm.SendOptions forwarded to every Continue call, after
// llm.WithTools(defs) (built once, loop-invariant, matching defs above it) so
// a caller-supplied option can override tools if it ever needs to.
func drive(ctx context.Context, conv *llm.Conversation, tools ToolSet, ev *Events, opts ...llm.SendOption) (*Result, error) {
	defs := tools.Definitions()
	sendOpts := append([]llm.SendOption{llm.WithTools(defs)}, opts...)
	var stats Stats

	for iteration := 1; iteration <= maxIterations; iteration++ {
		stats.Iterations = iteration
		start := time.Now()
		resp, err := conv.Continue(ctx, sendOpts...)
		ev.turn(resp, time.Since(start), err)
		if err != nil {
			return &Result{Stats: stats}, fmt.Errorf("agentloop: iteration %d: %w", iteration, err)
		}
		stats.InputTokens += resp.Usage.InputTokens
		stats.OutputTokens += resp.Usage.OutputTokens

		switch resp.StopReason {
		case "end_turn":
			text, ok := firstText(resp.Content)
			if !ok {
				return &Result{Stats: stats}, errors.New("agentloop: no text in final response")
			}
			ev.text(text)
			return &Result{Text: text, Stats: stats}, nil

		case "tool_use":
			toolUses := extractToolUses(resp.Content)
			if len(toolUses) == 0 {
				return &Result{Stats: stats}, errors.New("agentloop: tool_use stop with no tool_use blocks")
			}
			stats.ToolCalls += len(toolUses)
			results := executeBatch(ctx, tools, ev, toolUses)
			// Persist per tool, in the assistant's tool_use order: a crash
			// mid-persist leaves an in-order prefix; unpersisted results
			// become orphans that Resume resolves synthetically.
			for _, r := range results {
				if err := conv.AppendUser(ctx, []anthropic.ContentBlockParamUnion{r}); err != nil {
					return &Result{Stats: stats}, fmt.Errorf("agentloop: persist tool result: %w", err)
				}
			}
			// Mid-turn steer seam: inject any pending user content after the
			// batch's results are persisted, before the next Continue.
			if ev != nil && ev.PendingUser != nil {
				if extra := ev.PendingUser(); len(extra) > 0 {
					if err := conv.AppendUser(ctx, extra); err != nil {
						return &Result{Stats: stats}, fmt.Errorf("agentloop: appending steer content: %w", err)
					}
				}
			}

		default:
			return &Result{Stats: stats}, fmt.Errorf("agentloop: unexpected stop reason %q", resp.StopReason)
		}
	}
	return &Result{Stats: stats}, fmt.Errorf("agentloop: exceeded maximum tool iterations (%d)", maxIterations)
}

type toolUse struct {
	id    string
	name  string
	input json.RawMessage
}

func extractToolUses(content []anthropic.ContentBlockUnion) []toolUse {
	var out []toolUse
	for _, block := range content {
		if block.Type == "tool_use" {
			tu := block.AsToolUse()
			out = append(out, toolUse{id: tu.ID, name: tu.Name, input: json.RawMessage(tu.Input)})
		}
	}
	return out
}

func firstText(content []anthropic.ContentBlockUnion) (string, bool) {
	for _, block := range content {
		if block.Type == "text" {
			return block.Text, true
		}
	}
	return "", false
}

// executeBatch runs the requested tools (bounded concurrency) and returns one
// tool_result block per request IN REQUEST ORDER — order matters for
// prompt-cache stability of the next turn. Failures are IsError results, not
// loop errors, so the model can distinguish a failed tool from missing data.
func executeBatch(ctx context.Context, tools ToolSet, ev *Events, uses []toolUse) []anthropic.ContentBlockParamUnion {
	results := make([]anthropic.ContentBlockParamUnion, len(uses))
	var emitMu sync.Mutex // serializes host callbacks across goroutines
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentTools)
	for i, use := range uses {
		g.Go(func() error {
			emitMu.Lock()
			ev.toolCall(use.name, use.input)
			ev.toolStart(use.id, use.name, use.input)
			emitMu.Unlock()

			tctx := context.WithValue(gctx, toolCallIDKey{}, use.id)
			result, err := tools.Execute(tctx, use.name, use.input)
			if err != nil && result == "" {
				result = fmt.Sprintf("Error executing tool: %v", err)
			}
			result = truncateToolResult(result, maxToolResultSize)

			emitMu.Lock()
			ev.toolResult(use.name, result, err)
			ev.toolEnd(use.id, use.name, result, err)
			emitMu.Unlock()

			results[i] = anthropic.NewToolResultBlock(use.id, result, err != nil)
			return nil // a failed tool is a marked result, never a group error
		})
	}
	_ = g.Wait()
	return results
}

// truncateToolResult bounds one tool result, breaking at a newline near the
// limit (ported from sc's diagnose loop).
func truncateToolResult(result string, maxSize int) string {
	if len(result) <= maxSize {
		return result
	}
	truncateAt := maxSize - 100 // room for the truncation note
	if idx := strings.LastIndex(result[:truncateAt], "\n"); idx > truncateAt/2 {
		truncateAt = idx
	}
	return result[:truncateAt] + fmt.Sprintf("\n... (truncated, %d bytes omitted)", len(result)-truncateAt)
}
