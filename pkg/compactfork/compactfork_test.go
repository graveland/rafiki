// SPDX-License-Identifier: Apache-2.0

package compactfork

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

const (
	sourceID       = "src"
	sourceModel    = "source-model"
	destModel      = "dest-model"
	intermediateID = "i-1"
	forkID         = "n-1"
	// defaultIntermediateName is what Run derives when Options.IntermediateName
	// is empty.
	defaultIntermediateName = sourceID + "-compact"
)

// fakeControl implements the four RPCs the fork layer uses and nothing else: the
// embedded interface is nil, so a call to any other verb panics. That is the
// loud answer — the layer is supposed to be incapable of touching the source.
type fakeControl struct {
	rafikiv1connect.ControlClient

	// spawnIDs are the child ids returned by successive Spawn calls.
	spawnIDs []string
	// history answers the nth (1-based) GetHistory call.
	history func(call int) []*rafikiv1.Event

	calls  []string
	spawns []*rafikiv1.SpawnRequest
	sends  []*rafikiv1.SendRequest
	closes []*rafikiv1.CloseRequest
	// historyCalls counts GetHistory calls, including the watermark read.
	historyCalls int
}

func (f *fakeControl) Spawn(_ context.Context, req *connect.Request[rafikiv1.SpawnRequest]) (*connect.Response[rafikiv1.SpawnResponse], error) {
	f.spawns = append(f.spawns, req.Msg)
	f.calls = append(f.calls, "spawn:"+req.Msg.GetName())
	id := ""
	if n := len(f.spawns); n <= len(f.spawnIDs) {
		id = f.spawnIDs[n-1]
	}
	return connect.NewResponse(&rafikiv1.SpawnResponse{ChildId: id}), nil
}

func (f *fakeControl) Send(_ context.Context, req *connect.Request[rafikiv1.SendRequest]) (*connect.Response[rafikiv1.SendResponse], error) {
	f.sends = append(f.sends, req.Msg)
	f.calls = append(f.calls, "send:"+req.Msg.GetChildId())
	return connect.NewResponse(&rafikiv1.SendResponse{}), nil
}

func (f *fakeControl) GetHistory(_ context.Context, req *connect.Request[rafikiv1.GetHistoryRequest]) (*connect.Response[rafikiv1.GetHistoryResponse], error) {
	f.historyCalls++
	f.calls = append(f.calls, "gethistory:"+req.Msg.GetChildId())
	var events []*rafikiv1.Event
	if f.history != nil {
		events = f.history(f.historyCalls)
	}
	return connect.NewResponse(&rafikiv1.GetHistoryResponse{Events: events}), nil
}

func (f *fakeControl) Close(_ context.Context, req *connect.Request[rafikiv1.CloseRequest]) (*connect.Response[rafikiv1.CloseResponse], error) {
	f.closes = append(f.closes, req.Msg)
	f.calls = append(f.calls, "close:"+req.Msg.GetChildId())
	return connect.NewResponse(&rafikiv1.CloseResponse{}), nil
}

func ordinal(n int32) *int32 { return &n }

func textBlock(text string) *rafikiv1.ContentBlock {
	return &rafikiv1.ContentBlock{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}}}
}

func userTextEvent(n int32, text string) *rafikiv1.Event {
	return &rafikiv1.Event{Ordinal: ordinal(n), Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{textBlock(text)}},
	}}
}

func assistantTextEvent(n int32, text string) *rafikiv1.Event {
	return &rafikiv1.Event{Ordinal: ordinal(n), Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{textBlock(text)}},
	}}
}

func boundaryEvent(n int32) *rafikiv1.Event {
	return &rafikiv1.Event{Ordinal: ordinal(n), Payload: &rafikiv1.Event_CompactionBoundary{
		CompactionBoundary: &rafikiv1.CompactionBoundary{Trigger: "manual"},
	}}
}

// baseRequest is what a caller hands Run: a fork of the source onto the
// destination model, carrying the fields Run must leave alone.
func baseRequest() *rafikiv1.SpawnRequest {
	return &rafikiv1.SpawnRequest{
		Cwd:      "/work",
		Name:     "dest",
		Model:    destModel,
		Kind:     "fundi",
		Labels:   map[string]string{"seat": "implementer"},
		Preset:   "implementer",
		ForkFrom: &rafikiv1.SpawnRequest_ForkFrom{ChildId: sourceID},
	}
}

func fastOptions() Options {
	return Options{
		SourceModel:  sourceModel,
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
	}
}

// TestRunSpawnsCompactsThenForksTheIntermediate is the whole composition: the
// intermediate forks from the source on the source's model, is told to /compact,
// is waited on, and the fork is spawned from it and it is closed. The source is
// named only as a fork edge (a read) and never as a mutation target.
func TestRunSpawnsCompactsThenForksTheIntermediate(t *testing.T) {
	c := assert.NewAborting(t)
	var lines []string
	f := &fakeControl{
		spawnIDs: []string{intermediateID, forkID},
		history: func(call int) []*rafikiv1.Event {
			events := []*rafikiv1.Event{userTextEvent(0, "the prompt"), assistantTextEvent(1, "the reply")}
			if call > 1 {
				events = append(events, boundaryEvent(2))
			}
			return events
		},
	}
	opt := fastOptions()
	opt.DestWindow = 200_000
	opt.Progress = func(line string) { lines = append(lines, line) }

	request := baseRequest()
	fork, err := Run(context.Background(), f, request, opt)
	c.NoError(err, "run")
	c.Eq(forkID, fork, "Run returns the fork's id")
	c.Eq(sourceID, request.GetForkFrom().GetChildId(), "the caller's request is not rewritten")
	c.Eq("implementer", request.GetPreset(), "the caller's request is not mutated")
	c.Eq(destModel, request.GetModel(), "the caller's destination model is untouched")

	c.Len(f.spawns, 2, "one intermediate and one fork")
	intermediate := f.spawns[0]
	c.Eq(sourceModel, intermediate.GetModel(), "the intermediate runs the source's model")
	c.Eq(defaultIntermediateName, intermediate.GetName(), "the intermediate's default name")
	c.True(intermediate.GetSkipDerivedIndex(), "the intermediate is not embedded or summarised")
	c.Eq("", intermediate.GetPreset(), "a destination preset must not shape the intermediate")
	c.Eq("", intermediate.GetFromTag(), "from_tag is exclusive with fork_from")
	c.Eq(sourceID, intermediate.GetForkFrom().GetChildId(), "the intermediate forks from the source")
	c.Eq("", intermediate.GetForkFrom().GetCursor(), "an empty cursor is the source's last completed turn")

	forked := f.spawns[1]
	c.Eq(destModel, forked.GetModel(), "the fork keeps the destination model")
	c.Eq(intermediateID, forked.GetForkFrom().GetChildId(), "the fork's edge names the intermediate")
	c.Eq("", forked.GetForkFrom().GetCursor(), "an empty cursor is the end of the compacted result")
	c.Eq("dest", forked.GetName(), "the fork keeps the caller's name")
	c.Eq("implementer", forked.GetPreset(), "the fork keeps the caller's preset")
	c.Eq("/work", forked.GetCwd(), "the fork keeps the caller's cwd")

	c.Len(f.sends, 1, "one /compact")
	sent := f.sends[0]
	c.Eq(intermediateID, sent.GetChildId(), "/compact goes to the intermediate")
	c.Eq(rafikiv1.SendMode_SEND_MODE_PROMPT, sent.GetMode(), "a slash command is a prompt")
	c.Len(sent.GetBlocks(), 1, "one text block")
	text := sent.GetBlocks()[0].GetText().GetText()
	c.True(strings.HasPrefix(text, "/compact "), "the block is the /compact command, got %q", text)
	c.True(strings.Contains(text, DefaultInstructions), "empty instructions fall back to the handoff, got %q", text)

	calls := f.calls
	c.GreaterOrEqual(6, len(calls), "spawn, watermark, send, history, spawn, close")
	c.Eq("spawn:"+defaultIntermediateName, calls[0], "the intermediate is spawned first")
	// The watermark read precedes /compact: it must be taken before the
	// compaction can publish anything, or a fast boundary would be mistaken for
	// one the source already carried.
	c.Eq("gethistory:"+intermediateID, calls[1], "then the watermark read")
	c.Eq("send:"+intermediateID, calls[2], "then /compact")
	c.Eq("spawn:dest", calls[len(calls)-2], "the fork is spawned after the boundary")
	c.Eq("close:"+intermediateID, calls[len(calls)-1], "the intermediate is closed last")
	for _, call := range calls[3 : len(calls)-2] {
		c.Eq("gethistory:"+intermediateID, call, "the wait polls the intermediate's history")
	}

	// The source is never MUTATED. The one request that names it is the
	// intermediate's fork_from, which is a read edge: that is how I borrows S's
	// history. Kill is absent from the fake entirely, so a kill would panic.
	for _, s := range f.sends {
		c.NotEq(sourceID, s.GetChildId(), "no send may target the source")
	}
	for _, cl := range f.closes {
		c.NotEq(sourceID, cl.GetChildId(), "no close may target the source")
	}
	for _, sp := range f.spawns {
		c.NotEq(sourceID, sp.GetParentChildId(), "the source may not be a parent")
	}
	c.NotEq(sourceID, forked.GetForkFrom().GetChildId(), "the fork borrows the intermediate, never the source")

	c.NotEmpty(lines, "progress was reported")
	c.Eq("compacting on "+sourceModel+" as "+defaultIntermediateName, lines[0], "the first progress line")
}

// TestRunWaitsForANewBoundary pins the watermark: a boundary the borrowed
// history already carried at the watermark must not satisfy the wait, or an
// already-compacted source would fork instantly onto a stale summary.
func TestRunWaitsForANewBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeControl{
		spawnIDs: []string{intermediateID, forkID},
		history: func(call int) []*rafikiv1.Event {
			events := []*rafikiv1.Event{userTextEvent(0, "the prompt"), boundaryEvent(1)}
			if call >= 3 {
				events = append(events, boundaryEvent(3))
			}
			return events
		},
	}

	fork, err := Run(context.Background(), f, baseRequest(), fastOptions())
	c.NoError(err, "run")
	c.Eq(forkID, fork, "the fork's id")

	c.Eq(3, f.historyCalls, "one watermark read and two polls")
	c.Len(f.spawns, 2, "the fork is spawned once the new boundary appears")
	c.EqDiff([]string{
		"spawn:" + defaultIntermediateName,
		"gethistory:" + intermediateID,
		"send:" + intermediateID,
		"gethistory:" + intermediateID,
		"gethistory:" + intermediateID,
		"spawn:dest",
		"close:" + intermediateID,
	}, f.calls, "the stale boundary is skipped, the new one proceeds")
}

// TestRunRefusesWhenTheCompactedHistoryStillDoesNotFit pins post-compaction
// overflow as an error: no fork is spawned, the intermediate is closed, and the
// message names both the window and the estimate.
func TestRunRefusesWhenTheCompactedHistoryStillDoesNotFit(t *testing.T) {
	c := assert.NewAborting(t)
	// 4000 characters is 1000 tokens plus the block overhead: 1032.
	big := strings.Repeat("x", 4000)
	f := &fakeControl{
		spawnIDs: []string{intermediateID, forkID},
		history: func(call int) []*rafikiv1.Event {
			events := []*rafikiv1.Event{userTextEvent(0, "hi")}
			if call > 1 {
				events = append(events, boundaryEvent(1), assistantTextEvent(2, big))
			}
			return events
		},
	}
	opt := fastOptions()
	opt.DestWindow = 1000

	_, err := Run(context.Background(), f, baseRequest(), opt)
	c.Error(err, "an overflow after compaction is an error")
	c.ErrorContains(err, "still exceeds 1000 tokens", "the window is named")
	c.ErrorContains(err, "estimated 1032", "the estimate is named")
	c.ErrorContains(err, "pick a larger model", "the remedy is named")

	c.Len(f.spawns, 1, "no fork is spawned")
	c.Len(f.closes, 1, "the intermediate is closed")
	c.Eq(intermediateID, f.closes[0].GetChildId(), "the intermediate is what is closed")
}

// TestRunClosesTheIntermediateOnAPostCompactRefusal pins the same refusal when
// the overflow rides a USER message rather than an assistant one, so the close
// is proven independent of which payload carried the text.
func TestRunClosesTheIntermediateOnAPostCompactRefusal(t *testing.T) {
	c := assert.NewAborting(t)
	big := strings.Repeat("y", 8000)
	f := &fakeControl{
		spawnIDs: []string{intermediateID, forkID},
		history: func(call int) []*rafikiv1.Event {
			events := []*rafikiv1.Event{userTextEvent(0, "hi")}
			if call > 1 {
				events = append(events, boundaryEvent(2), userTextEvent(3, big))
			}
			return events
		},
	}
	opt := fastOptions()
	opt.DestWindow = 500

	_, err := Run(context.Background(), f, baseRequest(), opt)
	c.Error(err, "an overflow after compaction is an error")
	c.Len(f.spawns, 1, "no fork is spawned")
	c.Len(f.closes, 1, "the intermediate is closed even though the fork was refused")
	c.Eq(rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY, f.closes[0].GetScope(), "only the intermediate is closed")
	c.Eq("close:"+intermediateID, f.calls[len(f.calls)-1], "the close is the last thing Run does")
}

// TestRunRefusesWithoutASourceModel pins that an impossible composition spawns
// nothing at all.
func TestRunRefusesWithoutASourceModel(t *testing.T) {
	t.Run("no fork source", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := &fakeControl{}
		_, err := Run(context.Background(), f, &rafikiv1.SpawnRequest{Model: destModel}, Options{SourceModel: sourceModel})
		c.EqualError(err, "compactfork: a compact fork needs a fork source and the source's model")
		c.Len(f.calls, 0, "nothing is spawned")
	})
	t.Run("no source model", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := &fakeControl{}
		_, err := Run(context.Background(), f, baseRequest(), Options{})
		c.EqualError(err, "compactfork: a compact fork needs a fork source and the source's model")
		c.Len(f.calls, 0, "nothing is spawned")
	})
}

// TestRunLeavesTheIntermediateOnATimeout pins that a wait that never sees a
// boundary reports the failure and closes nothing: the caller decides what to do
// with an intermediate that is still running.
func TestRunLeavesTheIntermediateOnATimeout(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeControl{spawnIDs: []string{intermediateID, forkID}}
	opt := fastOptions()
	opt.Timeout = 20 * time.Millisecond

	_, err := Run(context.Background(), f, baseRequest(), opt)
	c.ErrorContains(err, "wait for compaction", "the failure names the step")
	c.ErrorIs(err, context.DeadlineExceeded, "the timeout is unwrappable")
	c.Len(f.spawns, 1, "no fork is spawned")
	c.Len(f.closes, 0, "the intermediate is left for the caller")
}

// TestRunFakeControlPanicsOnAnUnusedVerb pins the fake's loud answer: the
// embedded interface is nil, so a verb Run is not supposed to use panics rather
// than silently succeeding.
func TestRunFakeControlPanicsOnAnUnusedVerb(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeControl{}
	c.Panics(func() {
		_, _ = f.Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{ChildId: sourceID}))
	}, "an unimplemented verb must panic")
}

// TestRunCloneRequestKeepsEveryField pins cloneRequest's field list against the
// descriptor. The copy is hand-written because this package imports nothing but
// the generated types and connect, so a new SpawnRequest field would otherwise be
// dropped silently — and a dropped field is a destination preset or budget
// leaking away from the fork, or a caller's setting vanishing from the
// intermediate.
func TestRunCloneRequestKeepsEveryField(t *testing.T) {
	c := assert.NewAborting(t)
	depth, cost, children := int32(2), 1.5, int32(3)
	req := &rafikiv1.SpawnRequest{
		Cwd:                "cwd",
		Name:               "name",
		Model:              "model",
		Kind:               "kind",
		Labels:             map[string]string{"k": "v"},
		ParentChildId:      "parent",
		ExecutorSelector:   "selector",
		ExecutorRef:        "ref",
		Preset:             "preset",
		Prefill:            []*rafikiv1.PrefillRead{{Path: "path"}},
		Script:             &rafikiv1.SpawnRequest_ScriptSpec{},
		ConfigDir:          "config",
		AppendSystemPrompt: "system",
		Thinking:           "thinking",
		NoSession:          true,
		ResumeSession:      "resume",
		ForkSession:        "fork-session",
		Extensions:         []string{"extension"},
		NoExtensions:       true,
		Verbose:            true,
		ExtraArgs:          []string{"arg"},
		SkillsDirs:         []string{"dir"},
		McpConfig:          "mcp",
		Env:                map[string]string{"e": "1"},
		RecordRequests:     true,
		PassthroughAuth:    "on",
		SkipDerivedIndex:   true,
		Sandbox:            &rafikiv1.SandboxSpec{},
		ForkFrom:           &rafikiv1.SpawnRequest_ForkFrom{ChildId: sourceID, Cursor: "m:1"},
		FromTag:            "tag",
		MaxDepth:           &depth,
		MaxCost:            &cost,
		MaxChildren:        &children,
	}
	// The fixture must set every field the descriptor declares, or it stops
	// proving anything the day a field is added.
	m := req.ProtoReflect()
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		c.True(m.Has(fd), "the fixture must set %s", fd.Name())
	}

	clone := cloneRequest(req)
	c.True(proto.Equal(req, clone), "cloneRequest must copy every field")
	clone.Name = "changed"
	c.Eq("name", req.GetName(), "the clone is not the caller's message")
}
