// SPDX-License-Identifier: Apache-2.0

// Package compactfork continues an oversized conversation on a model whose
// context window cannot hold the borrowed history: it forks the source into an
// INTERMEDIATE child on the source's own model (the only one that can ingest the
// history), compacts that child, and forks the REAL child from the compacted
// result. The source is never touched — only a model change closes it.
//
// Three children, in the order they matter:
//
//   - S, the source. Read once, for its id. Every request Run sends either
//     targets the intermediate or names S as a fork edge (a read); nothing
//     mutates S.
//   - I, the intermediate: a fork of S on S's own model, told to /compact. It is
//     an ordinary child, VISIBLE in the rail and in `rafiki list` for the
//     duration of the summary call, deliberately: an operator watching spend can
//     see what is being called. It is CLOSED once the fork exists, never
//     deleted — the fork edge from N names a node in I's conversation.
//   - N, the fork: a child on the destination model whose borrowed history is
//     I's compacted result.
//
// The post-compaction size check is an ESTIMATE, not a tokenizer. Every
// text-bearing block at or after the compaction boundary is charged
// len(text)/4 + 32 tokens — text, thinking, and tool-use input JSON, with a tool
// result's nested blocks summed and the result block itself charged the fixed
// overhead. Image blocks are not text and are charged nothing. The estimate
// exists to refuse a fork that plainly cannot fit before it is spawned; a caller
// that supplies no destination window (Options.DestWindow 0) skips the check
// entirely.
//
// This package owns the whole fork layer and knows nothing about the model-change
// tail. It speaks only the Connect control plane, so it links no database
// driver: cmd/rafiki imports it and must never link pgx.
package compactfork

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// DefaultInstructions is what the intermediate child's /compact is given when
// Options.Instructions is empty: this is a HANDOFF, not a summary — the model
// taking over needs task state, not narrative.
const DefaultInstructions = "This conversation is being handed to a model with a smaller " +
	"context window. Preserve: the current task and its goal, decisions already made " +
	"and why, file paths and commands that matter, open threads and known failures. " +
	"Compress: narrative, superseded attempts, and tool output already summarised."

const (
	// defaultPollInterval is how often the wait re-reads the intermediate's
	// history for the compaction boundary.
	defaultPollInterval = 500 * time.Millisecond
	// defaultTimeout bounds that wait. A compaction on a conversation near the
	// source model's window is a slow call, so the ceiling is generous.
	defaultTimeout = 20 * time.Minute
	// blockOverhead is the fixed per-block charge of the size estimate.
	blockOverhead = 32
)

// Options tunes the composition. Zero values take the defaults below.
type Options struct {
	// IntermediateName names the helper child. Default: "<source>-compact".
	IntermediateName string
	// SourceModel is the model the intermediate child runs — the SOURCE's,
	// which is what makes the compaction call possible at all.
	SourceModel string
	// Instructions are passed to /compact after the command word.
	// Empty means DefaultInstructions.
	Instructions string
	// DestWindow is the destination model's context window, for the
	// post-compaction check. 0 = unknown = skip the check.
	DestWindow int32
	// PollInterval and Timeout bound the wait for the compaction boundary.
	PollInterval time.Duration // default 500ms
	Timeout      time.Duration // default 20m
	// Progress, when non-nil, is called with a short human line at each step.
	Progress func(string)
}

// Run performs the fork layer and returns the NEW child's id.
//
// req is the request the caller would have sent directly: its ForkFrom names the
// source and its Model the destination. Run rewrites ForkFrom onto the
// intermediate child and leaves every other field as the caller set it.
//
// Ordering: I is spawned from the SOURCE; /compact is sent to I; Run waits for a
// CompactionBoundary event; N is spawned from I with an EMPTY cursor (I's last
// completed turn, i.e. the end of the compacted result); I is closed. Nothing
// here touches the source.
//
// A wait failure leaves I alone — the caller decides whether to close it, which
// is why the error names it.
func Run(ctx context.Context, c rafikiv1connect.ControlClient, req *rafikiv1.SpawnRequest, opt Options) (string, error) {
	source := req.GetForkFrom().GetChildId()
	if source == "" || opt.SourceModel == "" {
		return "", errors.New("compactfork: a compact fork needs a fork source and the source's model")
	}
	poll := opt.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	progress := opt.Progress
	if progress == nil {
		progress = func(string) {}
	}
	name := opt.IntermediateName
	if name == "" {
		name = source + "-compact"
	}

	// Spawn the intermediate: a fork of the source's last completed turn, on the
	// source's own model. Preset and FromTag are cleared because the
	// intermediate is a plain model call — a destination preset must not shape
	// it, and from_tag is exclusive with fork_from.
	intermediateReq := cloneRequest(req)
	intermediateReq.ForkFrom = &rafikiv1.SpawnRequest_ForkFrom{ChildId: source}
	intermediateReq.Model = opt.SourceModel
	intermediateReq.Name = name
	intermediateReq.Preset = ""
	intermediateReq.SkipDerivedIndex = true
	intermediateReq.FromTag = ""
	spawned, err := c.Spawn(ctx, connect.NewRequest(intermediateReq))
	if err != nil {
		return "", fmt.Errorf("spawn intermediate %s: %w", name, err)
	}
	intermediate := spawned.Msg.GetChildId()
	if intermediate == "" {
		return "", fmt.Errorf("spawn intermediate %s: the daemon returned no child id", name)
	}
	progress("compacting on " + opt.SourceModel + " as " + name)

	// The watermark: a source that was already compacted carries a boundary in
	// its borrowed history, so a boundary found before this point would satisfy
	// the wait instantly with a stale event.
	watermark, err := historyWatermark(ctx, c, intermediate)
	if err != nil {
		return "", fmt.Errorf("read the intermediate history: %w", err)
	}

	instructions := opt.Instructions
	if strings.TrimSpace(instructions) == "" {
		instructions = DefaultInstructions
	}
	_, err = c.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: intermediate,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "/compact " + instructions}},
		}},
	}))
	if err != nil {
		return "", fmt.Errorf("send /compact: %w", err)
	}
	progress("waiting for the compaction boundary")

	boundary, events, err := waitForBoundary(ctx, c, intermediate, watermark, poll, timeout)
	if err != nil {
		return "", fmt.Errorf("wait for compaction: %w", err)
	}

	// Post-compaction overflow is an ERROR, never a silent fall-through to the
	// reactive overflow net: the caller picks a larger model instead.
	if opt.DestWindow > 0 {
		if estimate := estimateFrom(events, boundary.GetOrdinal()); estimate > int(opt.DestWindow) {
			sizeErr := fmt.Errorf("compacted history still exceeds %d tokens (estimated %d): pick a larger model", opt.DestWindow, estimate)
			if cerr := closeIntermediate(ctx, c, intermediate); cerr != nil {
				return "", fmt.Errorf("%w (and closing the intermediate %s failed: %v)", sizeErr, intermediate, cerr)
			}
			return "", sizeErr
		}
	}

	// The fork: the caller's request, with the fork edge moved onto the
	// intermediate and an empty cursor (the end of the compacted result).
	forkReq := cloneRequest(req)
	forkReq.ForkFrom = &rafikiv1.SpawnRequest_ForkFrom{ChildId: intermediate}
	spawned, err = c.Spawn(ctx, connect.NewRequest(forkReq))
	if err != nil {
		return "", fmt.Errorf("spawn the fork from %s: %w", intermediate, err)
	}
	fork := spawned.Msg.GetChildId()
	if fork == "" {
		return "", fmt.Errorf("spawn the fork from %s: the daemon returned no child id", intermediate)
	}
	progress("forked " + fork + " from the compacted " + intermediate)

	if cerr := closeIntermediate(ctx, c, intermediate); cerr != nil {
		return "", fmt.Errorf("close the intermediate %s (the fork %s exists): %w", intermediate, fork, cerr)
	}
	progress("closed " + intermediate)
	return fork, nil
}

// historyWatermark is the highest ordinal in a child's durable history, or -1
// when it has none — so a boundary at ordinal 0 still counts as new.
func historyWatermark(ctx context.Context, c rafikiv1connect.ControlClient, childID string) (int32, error) {
	events, err := fetchHistory(ctx, c, childID)
	if err != nil {
		return 0, err
	}
	watermark := int32(-1)
	for _, ev := range events {
		if ordinal := ev.GetOrdinal(); ordinal > watermark {
			watermark = ordinal
		}
	}
	return watermark, nil
}

// waitForBoundary polls the intermediate's history until a compaction boundary
// appears past the watermark, and returns it along with the history it was found
// in (the post-compaction estimate reads the events at or after it). The poll
// reads the whole history rather than filtering with after_ordinal: the same
// read feeds the estimate, so a filter would save little and give one read two
// shapes.
func waitForBoundary(ctx context.Context, c rafikiv1connect.ControlClient, childID string, watermark int32, poll, timeout time.Duration) (*rafikiv1.Event, []*rafikiv1.Event, error) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		events, err := fetchHistory(ctx, c, childID)
		if err != nil {
			return nil, nil, err
		}
		for _, ev := range events {
			if ev.GetCompactionBoundary() != nil && ev.GetOrdinal() > watermark {
				return ev, events, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return nil, nil, context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func fetchHistory(ctx context.Context, c rafikiv1connect.ControlClient, childID string) ([]*rafikiv1.Event, error) {
	resp, err := c.GetHistory(ctx, connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetEvents(), nil
}

// closeIntermediate closes the helper child and nothing else: N's fork edge
// names a node in its conversation, so a descendant-scoped close would be both
// impossible (N is not its descendant) and wrong.
func closeIntermediate(ctx context.Context, c rafikiv1connect.ControlClient, childID string) error {
	_, err := c.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{
		ChildId: childID,
		Scope:   rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY,
	}))
	return err
}

// estimateFrom sums the size estimate over the events at or after ordinal from.
func estimateFrom(events []*rafikiv1.Event, from int32) int {
	total := 0
	for _, ev := range events {
		if ev.GetOrdinal() < from {
			continue
		}
		total += estimateEvent(ev)
	}
	return total
}

// estimateEvent charges a message-bearing event's text blocks; every other
// payload (turn markers, tool executions, status) carries no prompt text.
func estimateEvent(ev *rafikiv1.Event) int {
	switch p := ev.GetPayload().(type) {
	case *rafikiv1.Event_UserMessage:
		return estimateBlocks(p.UserMessage.GetContent()) + estimateBlocks(p.UserMessage.GetInjected())
	case *rafikiv1.Event_AssistantMessage:
		return estimateBlocks(p.AssistantMessage.GetContent())
	case *rafikiv1.Event_SystemMessage:
		return estimateBlocks(p.SystemMessage.GetContent())
	default:
		return 0
	}
}

func estimateBlocks(blocks []*rafikiv1.ContentBlock) int {
	total := 0
	for _, b := range blocks {
		switch v := b.GetBlock().(type) {
		case *rafikiv1.ContentBlock_Text:
			total += estimateText(v.Text.GetText())
		case *rafikiv1.ContentBlock_Thinking:
			total += estimateText(v.Thinking.GetThinking())
		case *rafikiv1.ContentBlock_ToolUse:
			total += estimateText(v.ToolUse.GetInputJson())
		case *rafikiv1.ContentBlock_ToolResult:
			total += blockOverhead + estimateBlocks(v.ToolResult.GetContent())
		}
	}
	return total
}

// estimateText is the whole estimate: four characters per token plus a fixed
// per-block overhead.
func estimateText(text string) int {
	return len(text)/4 + blockOverhead
}

// cloneRequest copies every field of a SpawnRequest so Run can rewrite the fork
// edge without touching the caller's message. It is field-by-field rather than
// proto.Clone because this package imports nothing beyond the generated types
// and connect — and TestRunCloneRequestKeepsEveryField fails when a new
// SpawnRequest field is not copied here. Nested messages, maps and slices are
// shared, never mutated: Run replaces only top-level fields.
func cloneRequest(req *rafikiv1.SpawnRequest) *rafikiv1.SpawnRequest {
	if req == nil {
		return &rafikiv1.SpawnRequest{}
	}
	return &rafikiv1.SpawnRequest{
		Cwd:                req.Cwd,
		Name:               req.Name,
		Model:              req.Model,
		Kind:               req.Kind,
		Labels:             req.Labels,
		ParentChildId:      req.ParentChildId,
		ExecutorSelector:   req.ExecutorSelector,
		ExecutorRef:        req.ExecutorRef,
		Preset:             req.Preset,
		Prefill:            req.Prefill,
		Script:             req.Script,
		ConfigDir:          req.ConfigDir,
		AppendSystemPrompt: req.AppendSystemPrompt,
		Thinking:           req.Thinking,
		NoSession:          req.NoSession,
		ResumeSession:      req.ResumeSession,
		ForkSession:        req.ForkSession,
		Extensions:         req.Extensions,
		NoExtensions:       req.NoExtensions,
		Verbose:            req.Verbose,
		ExtraArgs:          req.ExtraArgs,
		SkillsDirs:         req.SkillsDirs,
		McpConfig:          req.McpConfig,
		Env:                req.Env,
		RecordRequests:     req.RecordRequests,
		PassthroughAuth:    req.PassthroughAuth,
		SkipDerivedIndex:   req.SkipDerivedIndex,
		Sandbox:            req.Sandbox,
		ForkFrom:           req.ForkFrom,
		FromTag:            req.FromTag,
		MaxDepth:           req.MaxDepth,
		MaxCost:            req.MaxCost,
		MaxChildren:        req.MaxChildren,
	}
}
