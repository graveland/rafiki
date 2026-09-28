// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/proto"
)

func textEvent(childID, text string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
		}},
	}
}

func TestApplyUserMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_test")
	s.Apply(textEvent("c_test", "hello"))
	c.Require().Len(s.Blocks, 1, "blocks = %d, want 1", len(s.Blocks))
	b := s.Blocks[0]
	c.False(b.Kind != session.KindUser || b.Text != "hello" || !b.Final, "block = %+v, want KindUser text=hello final=true", b)
}

func TestApplyIgnoresAnotherChildsEvent(t *testing.T) {
	s := session.New("c_mine")
	s.Apply(textEvent("c_theirs", "not for me"))
	assert.NewAborting(t).Empty(s.Blocks, "blocks = %d, want 0 -- a session must ignore another child's event", len(s.Blocks))
}

func TestApplyAssistantMessage(t *testing.T) {
	s := session.New("c_test")
	s.Apply(&rafikiv1.Event{
		ChildId: "c_test",
		Payload: &rafikiv1.Event_AssistantMessage{AssistantMessage: &rafikiv1.AssistantMessage{
			Content: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hi back"}},
			}},
		}},
	})
	assert.NewAborting(t).Len(s.Blocks, 1, "blocks = %d, want 1", len(s.Blocks))
	if s.Blocks[0].Kind != session.KindAssistant || !s.Blocks[0].Final {
		t.Errorf("kind=%v final=%v, want KindAssistant final=true", s.Blocks[0].Kind, s.Blocks[0].Final)
	}
}

func TestStreamingTurn(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_test")

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}})
	c.Require().False(len(s.Blocks) != 1 || s.Blocks[0].Final, "TurnStart should create a non-final block")

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: &rafikiv1.ContentBlockDelta{
			Delta: &rafikiv1.ContentBlockDelta_Text{Text: "streaming"},
		}}})
	c.Eq("streaming", s.LastAssistant().Text, "text")

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}})
	c.Require().True(s.LastAssistant().Final, "TurnEnd should finalize the block")
}

func TestToolExecution(t *testing.T) {
	s := session.New("c_test")
	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}})

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_ToolExecutionStart{ToolExecutionStart: &rafikiv1.ToolExecutionStart{
			ToolUseId: "tu_1", Name: "bash",
		}}})
	last := s.LastAssistant()
	assert.NewAborting(t).False(len(last.ToolCalls) != 1 || !last.ToolCalls[0].Running, "ToolExecutionStart should add a running tool call")

	s.Apply(&rafikiv1.Event{ChildId: "c_test",
		Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{
			ToolUseId: "tu_1", DurationMs: 1500,
		}}})
	last = s.LastAssistant()
	if last.ToolCalls[0].Running || last.ToolCalls[0].DurationMs != 1500 {
		t.Errorf("running=%v durationMs=%d, want false/1500",
			last.ToolCalls[0].Running, last.ToolCalls[0].DurationMs)
	}
}

func TestCursorTracksHighestOrdinalAndZeroIsLegal(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_test")
	c.Require().False(s.HasCursor, "a fresh session must have no cursor")

	zero := int32(0)
	ev := textEvent("c_test", "first")
	ev.Ordinal = &zero
	s.Apply(ev)
	if !s.HasCursor || s.Cursor != 0 {
		t.Fatalf("cursor = %d hasCursor = %v, want 0/true -- ordinal 0 is legal",
			s.Cursor, s.HasCursor)
	}

	five := int32(5)
	ev5 := textEvent("c_test", "later")
	ev5.Ordinal = &five
	s.Apply(ev5)

	two := int32(2)
	ev2 := textEvent("c_test", "out of order")
	ev2.Ordinal = &two
	s.Apply(ev2)

	c.Eq(5, s.Cursor, "cursor")
}

// The rail and focus subscriptions overlap on the durable tier, so a focused
// child's turn_end and error events arrive on BOTH. Without ordinal dedupe an
// error appends its block twice and the transcript grows phantom entries.
func TestDuplicateOrdinalIsIgnored(t *testing.T) {
	c := assert.NewAborting(t)
	s := session.New("c_1")
	errEv := func(ord int32) *rafikiv1.Event {
		return &rafikiv1.Event{ChildId: "c_1", Ordinal: &ord,
			Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{
				Code: "boom", Message: "upstream died"}}}
	}
	s.Apply(errEv(4))
	s.Apply(errEv(4)) // same ordinal, delivered by the other subscription
	c.Len(s.Blocks, 1, "blocks = %d, want 1 -- the duplicate must be dropped", len(s.Blocks))
	s.Apply(errEv(5))
	c.Len(s.Blocks, 2, "blocks = %d, want 2 -- a genuinely new ordinal must still apply", len(s.Blocks))
}

// Anthropic puts tool_result in the USER message following the tool_use, and
// TextFromContent reads text blocks only — so those messages rendered as EMPTY
// user bubbles and every tool's output was dropped. One blank bubble per tool
// call, no output anywhere.
func TestToolResultAttachesToItsCallAndAddsNoBubble(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
				Id: "tu_1", Name: "bash", InputJson: `{"command":"ls"}`}},
		}}},
	}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{
				ToolUseId: "tu_1",
				Content: []*rafikiv1.ContentBlock{{
					Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "a.go\nb.go"}}}},
			}}},
		}},
	}})

	c.Require().Len(s.Blocks, 1, "got %d blocks, want 1 — a results-only message must add no user bubble", len(s.Blocks))
	calls := s.Blocks[0].ToolCalls
	c.Require().Len(calls, 1, "got %d tool calls, want 1", len(calls))
	c.Eq(`{"command":"ls"}`, calls[0].Input, "input")
	c.Eq("a.go\nb.go", calls[0].Result, "result")
}

// A user message with real text alongside results still renders its text.
func TestUserTextAlongsideAResultStillRenders(t *testing.T) {
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{
			{Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{ToolUseId: "tu_x"}}},
			{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "and also this"}}},
		}},
	}})
	if len(s.Blocks) != 1 || s.Blocks[0].Text != "and also this" {
		t.Fatalf("blocks = %+v, want the user's text preserved", s.Blocks)
	}
}

// The assistant message naming a tool_use is published BEFORE the tool runs, so
// tool_execution_start is normally not new. Appending unconditionally listed
// every call twice — invisible while fundi published no assistant messages,
// immediate once it did.
func TestToolExecutionStartDoesNotDuplicateAKnownCall(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
				Id: "tu_1", Name: "bash"}},
		}}},
	}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_ToolExecutionStart{
		ToolExecutionStart: &rafikiv1.ToolExecutionStart{ToolUseId: "tu_1", Name: "bash"}}})

	c.Require().Eq(1, len(s.Blocks[0].ToolCalls), "got")
	c.True(s.Blocks[0].ToolCalls[0].Running, "tool_execution_start must mark the known call running")
}

// tool_execution_end is the more direct witness of a failure — it carries the
// tool's own error. A stored tool_result block whose is_error is absent must
// not turn that ✗ back into a ✓: it is the one direction that must never
// happen silently.
func TestAToolResultCannotDowngradeAKnownFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
				Id: "tu_1", Name: "bash"}},
		}}},
	}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_ToolExecutionEnd{
		ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{ToolUseId: "tu_1", IsError: true}}})
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{
				ToolUseId: "tu_1", // is_error absent
				Content: []*rafikiv1.ContentBlock{{
					Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "boom"}}}},
			}}},
		}},
	}})

	c.True(s.Blocks[0].ToolCalls[0].IsError, "a tool_result with no is_error downgraded a known failure to success")
	c.Eq("boom", s.Blocks[0].ToolCalls[0].Result, "result")
}

// An image block used to render as nothing at all, which is indistinguishable
// from a tool that returned nothing. The cockpit cannot draw pixels — a
// graphics escape written into a bubbletea View is parsed into ultraviolet's
// cell grid, has nowhere to live, and is dropped — so name the image instead.
func TestImageBlockIsNamedRatherThanDropped(t *testing.T) {
	c := assert.NewCollecting(t)
	// A real 1x1 PNG, so DecodeConfig has a header to read.
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	c.Require().NoError(err)

	s := session.New("c_1")
	s.Apply(&rafikiv1.Event{ChildId: "c_1", Payload: &rafikiv1.Event_UserMessage{
		UserMessage: &rafikiv1.UserMessage{Content: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{
				MediaType: "image/png", Data: png}},
		}}},
	}})

	c.Require().Len(s.Blocks, 1, "got %d blocks, want the image to produce one", len(s.Blocks))
	got := s.Blocks[0].Text
	for _, want := range []string{"image/png", "1×1"} {
		c.StrContains(got, want, "placeholder")
	}
}

// An undecodable or unknown image still gets named — the point is that
// something is there, not that we could parse it.
func TestUnreadableImageStillGetsAPlaceholder(t *testing.T) {
	c := assert.NewCollecting(t)
	got := session.ImagePlaceholder(&rafikiv1.ImageBlock{
		MediaType: "image/webp", Data: []byte("not really an image"),
	})
	c.StrContains(got, "image/webp", "placeholder")
	c.Eq("", session.ImagePlaceholder(nil), "a nil image must render as nothing")
}

// A tool result that arrives AFTER its assistant message must not be walled
// off behind the finalization watermark. renderer.Lines treats every block
// below Finalized as immutable and caches it once, so a block finalized
// before its tool calls resolve is frozen forever showing no result.
func TestAssistantBlockIsNotFinalizedUntilItsToolsResolve(t *testing.T) {
	c := assert.NewAborting(t)
	s := session.New("c1")

	s.Apply(&rafikiv1.Event{
		ChildId: "c1",
		Payload: &rafikiv1.Event_AssistantMessage{
			AssistantMessage: &rafikiv1.AssistantMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
						Id: "tu_1", Name: "bash", InputJson: `{"command":"ls"}`,
					}},
				}},
			},
		},
	})

	c.Eq(0, s.Finalized, "Finalized")

	s.Apply(&rafikiv1.Event{
		ChildId: "c1",
		Payload: &rafikiv1.Event_UserMessage{
			UserMessage: &rafikiv1.UserMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{
						ToolUseId: "tu_1",
						Content: []*rafikiv1.ContentBlock{{
							Index: 0,
							Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "go.mod"}},
						}},
					}},
				}},
			},
		},
	})

	c.Eq(1, s.Finalized, "Finalized")
}

// The anti-stall guard. A tool call that never returns must not park the
// watermark forever: every block after it would stay in the live region and
// be re-rendered on every tick, which is the 10.9ms-per-Update regime the
// two-axis renderer design exists to avoid.
func TestTurnEndSettlesBlocksWithUnansweredToolCalls(t *testing.T) {
	c := assert.NewAborting(t)
	s := session.New("c1")

	s.Apply(&rafikiv1.Event{
		ChildId: "c1",
		Payload: &rafikiv1.Event_AssistantMessage{
			AssistantMessage: &rafikiv1.AssistantMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
						Id: "tu_orphan", Name: "bash",
					}},
				}},
			},
		},
	})
	c.Eq(0, s.Finalized, "Finalized")

	s.Apply(&rafikiv1.Event{
		ChildId: "c1",
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}},
	})

	c.Eq(len(s.Blocks), s.Finalized, "Finalized")
}

// ── compaction boundaries (design §7) ─────────────────────────────────────

func compactionEvent(childID string, pre, post *int32) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_CompactionBoundary{CompactionBoundary: &rafikiv1.CompactionBoundary{
			Trigger:    "auto",
			PreTokens:  pre,
			PostTokens: post,
		}},
	}
}

func i32(v int32) *int32 { return &v }

// A live event carries both tokens: the divider shows the rewrite as a ratio.
func TestCompactionBoundaryShowsBothTokenCounts(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_test")
	s.Apply(compactionEvent("c_test", i32(150000), i32(12000)))

	c.Require().Len(s.Blocks, 1, "blocks = %d, want 1", len(s.Blocks))
	b := s.Blocks[0]
	c.False(b.Kind != session.KindSystem || !b.Final, "block = kind %v final %v, want KindSystem final=true", b.Kind, b.Final)
	c.Eq("— context compacted · 150k → 12k tokens —", b.Text, "text")
}

// A reattach-synthesized event (built from a stored kind='compaction_summary'
// row, not the live stream) can only ever supply pre_tokens, approximate: no
// "→" and no post count.
func TestCompactionBoundaryWithPreTokensOnlyIsApproximate(t *testing.T) {
	s := session.New("c_test")
	s.Apply(compactionEvent("c_test", i32(90000), nil))

	b := s.Blocks[0]
	assert.NewCollecting(t).False(!strings.Contains(b.Text, "~") || strings.Contains(b.Text, "→"), "text = %q, want the approximate pre-only divider", b.Text)
}

func TestCompactionBoundaryWithoutTokensIsBare(t *testing.T) {
	s := session.New("c_test")
	s.Apply(compactionEvent("c_test", nil, nil))

	assert.NewCollecting(t).Eq("— context compacted —", s.Blocks[0].Text, "text")
}

// The cockpit clears its ⏳ pending echo only when an event appends a KindUser
// block — that predicate is the whole pending state machine, and the session's
// only contribution to it is which Kind it appends. A compaction boundary is
// mid-conversation, never the acknowledgement of a sent prompt, so it lands as
// a KindSystem divider and leaves the echo (and the user_message that finally
// confirms it) alone.
func TestCompactionBoundaryDoesNotClearPending(t *testing.T) {
	c := assert.NewAborting(t)
	s := session.New("c_test")
	// A prompt is in flight: the pending echo sits at the tail of the blocks.
	// The session never creates one itself (the cockpit renders its own ⏳ from
	// its pending field), so seed it by hand the way the state machine sees it.
	s.Blocks = append(s.Blocks, session.Block{Kind: session.KindPendingUser, Text: "go for it"})
	n := len(s.Blocks)

	s.Apply(compactionEvent("c_test", i32(150000), i32(12000)))
	c.Len(s.Blocks, n+1, "blocks = %d, want %d: the divider must append exactly one block", len(s.Blocks), n+1)
	if last := s.Blocks[len(s.Blocks)-1]; last.Kind != session.KindSystem {
		t.Errorf("compaction appended kind %v; a KindUser block here would clear the cockpit's ⏳ as if the sent message had come back", last.Kind)
	}

	// The confirming user_message still lands after the divider.
	s.Apply(textEvent("c_test", "go for it"))
	c.Len(s.Blocks, n+2, "blocks = %d, want %d: the confirming message must still append", len(s.Blocks), n+2)
	if last := s.Blocks[len(s.Blocks)-1]; last.Kind != session.KindUser || last.Text != "go for it" {
		t.Errorf("last block = kind %v text %q, want the confirming KindUser message", last.Kind, last.Text)
	}
	if s.Blocks[0].Kind != session.KindPendingUser || s.Blocks[0].Text != "go for it" {
		t.Errorf("pending echo = kind %v text %q, want it untouched by the compaction event", s.Blocks[0].Kind, s.Blocks[0].Text)
	}
}

// A compaction boundary is mid-conversation: unlike the Error and ChildExited
// cases it must NOT settleAll, or every tool call still in flight would be
// forced to resolve early. settleAll moves the watermark rather than touching
// Running, so the watermark — not Running alone — is what proves it was not
// called.
func TestCompactionBoundaryDoesNotSettleRunningToolCalls(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_test")
	s.Apply(&rafikiv1.Event{
		ChildId: "c_test",
		Payload: &rafikiv1.Event_AssistantMessage{
			AssistantMessage: &rafikiv1.AssistantMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
						Id: "tu_1", Name: "bash", InputJson: `{"command":"sleep 60"}`,
					}},
				}},
			},
		},
	})
	s.Apply(&rafikiv1.Event{
		ChildId: "c_test",
		Payload: &rafikiv1.Event_ToolExecutionStart{ToolExecutionStart: &rafikiv1.ToolExecutionStart{
			ToolUseId: "tu_1", Name: "bash",
		}},
	})

	s.Apply(compactionEvent("c_test", i32(150000), i32(12000)))

	last := s.LastAssistant()
	c.True(last.ToolCalls[0].Running, "the tool call's Running was cleared by a compaction boundary; nothing mid-conversation may resolve an in-flight call")
	c.NotEq(len(s.Blocks), s.Finalized, "Finalized")
}

func retryEvent(childID string, willRetry bool, attempt int32, reason string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{
			Attempt:   attempt,
			WillRetry: willRetry,
			Reason:    reason,
		}},
	}
}

// scheduleEvent is the will_retry=true half as the live daemon publishes it:
// the cause rides in reason, the fire instant structurally in
// resume_at_unix_ms. Anchored to time.Local because the session renders the
// instant in the viewer's zone — the same digits assert on any machine.
func scheduleEvent(childID string, resumeAt time.Time) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{
			Attempt:        1,
			WillRetry:      true,
			Reason:         "rate limited (HTTP 429)",
			ResumeAtUnixMs: proto.Int64(resumeAt.UnixMilli()),
			MaxAttempts:    3,
		}},
	}
}

// The daemon's rate-limit auto-resume is Event_Retry's only producer: a
// scheduled resume must appear in the transcript as a system block naming
// when it fires, exactly as the rail's ⟳ names it in the tree — in the
// VIEWER's local zone, since the producing daemon's clock zone is arbitrary
// (a container runs UTC).
func TestRetryNoticeAppendsSystemBlock(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_test")
	s.Apply(scheduleEvent("c_test", time.Date(2026, 9, 26, 15, 4, 5, 0, time.Local)))

	c.Require().Len(s.Blocks, 1, "blocks = %d, want 1", len(s.Blocks))
	b := s.Blocks[0]
	c.False(b.Kind != session.KindSystem || !b.Final, "block = kind %v final %v, want KindSystem final=true", b.Kind, b.Final)
	c.StrContains(b.Text, "auto-resume scheduled for 15:04:05", "text")
	c.StrContains(b.Text, "rate limited (HTTP 429); ", "text")
	c.StrContains(b.Text, "(attempt 1/3)", "text")
}

// A schedule event with no fire instant — a producer that names none, or a
// row written before resume_at_unix_ms existed — falls back to the reason
// text verbatim rather than rendering a bogus zero time.
func TestRetryScheduleWithoutInstantFallsBackToReason(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_test")
	s.Apply(retryEvent("c_test", true, 1, "rate limited (HTTP 429); auto-resume scheduled for 01:10:30 (attempt 1/3)"))

	c.Require().Len(s.Blocks, 1, "blocks = %d, want 1", len(s.Blocks))
	c.StrContains(s.Blocks[0].Text, "01:10:30", "text")
}

// The resolution half (fired, cleared by a success, abandoned) clears the
// rail's glyph and must NOT append a transcript block: the turn that follows
// appends its own, and a "firing" divider between them would be noise.
func TestRetryResolutionAppendsNoBlock(t *testing.T) {
	s := session.New("c_test")
	s.Apply(scheduleEvent("c_test", time.Date(2026, 9, 26, 15, 4, 5, 0, time.Local)))
	s.Apply(retryEvent("c_test", false, 1, "auto-resume 1 firing"))

	assert.NewAborting(t).Len(s.Blocks, 1, "blocks = %d, want 1: the will_retry=false half appends nothing", len(s.Blocks))
}

// A retry notice is mid-conversation, not turn-ending: like the compaction
// boundary it must not settleAll, or a future mid-turn producer would freeze
// its in-flight tool calls without their results.
func TestRetryNoticeDoesNotSettleRunningToolCalls(t *testing.T) {
	s := session.New("c_test")
	s.Apply(&rafikiv1.Event{
		ChildId: "c_test",
		Payload: &rafikiv1.Event_AssistantMessage{
			AssistantMessage: &rafikiv1.AssistantMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
						Id: "tu_1", Name: "bash", InputJson: `{"command":"sleep 60"}`,
					}},
				}},
			},
		},
	})
	s.Apply(&rafikiv1.Event{ChildId: "c_test", Payload: &rafikiv1.Event_ToolExecutionStart{
		ToolExecutionStart: &rafikiv1.ToolExecutionStart{ToolUseId: "tu_1", Name: "bash"},
	}})

	s.Apply(scheduleEvent("c_test", time.Date(2026, 9, 26, 15, 4, 5, 0, time.Local)))

	assert.NewCollecting(t).NotEq(len(s.Blocks), s.Finalized, "Finalized")
}

// A script child's stdout lands as a KindScriptOutput block carrying its
// stream name; stderr is distinguishable from stdout.
func TestApplyScriptOutput(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_s")
	s.Apply(&rafikiv1.Event{ChildId: "c_s", Payload: &rafikiv1.Event_ScriptOutput{
		ScriptOutput: &rafikiv1.ScriptOutput{Stream: "stdout", Text: "step 1\n"},
	}})
	s.Apply(&rafikiv1.Event{ChildId: "c_s", Payload: &rafikiv1.Event_ScriptOutput{
		ScriptOutput: &rafikiv1.ScriptOutput{Stream: "stderr", Text: "warn\n"},
	}})
	c.Require().Len(s.Blocks, 2, "blocks")
	c.Eq(session.KindScriptOutput, s.Blocks[0].Kind, "block[0] kind")
	c.Eq("stdout", s.Blocks[0].Stream, "block[0] stream")
	c.Eq("stderr", s.Blocks[1].Stream, "block[1] stream")
}

// ApplyHistory folds a script_output the same way, and — the invariant —
// never touches the cursor: a replayed log event's ordinal is the EVENT log's,
// and a session fed through ApplyHistory keeps whatever cursor its live feed
// established.
func TestApplyHistoryScriptOutputDoesNotMoveTheCursor(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_s")
	ord := int32(41)
	s.ApplyHistory(&rafikiv1.Event{ChildId: "c_s", Ordinal: &ord,
		Payload: &rafikiv1.Event_ScriptOutput{ScriptOutput: &rafikiv1.ScriptOutput{
			Stream: "stdout", Text: "from the log replay"}}})
	c.Require().Len(s.Blocks, 1, "blocks")
	c.False(s.HasCursor, "ApplyHistory set the cursor; the two ordinal spaces are unrelated")
}

// A ScriptReport renders as a system line naming the caller's kind.
func TestApplyScriptReport(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c_s")
	s.Apply(&rafikiv1.Event{ChildId: "c_s", Payload: &rafikiv1.Event_ScriptReport{
		ScriptReport: &rafikiv1.ScriptReport{Kind: "progress", DataJson: `{"step": 3}`},
	}})
	c.Require().Len(s.Blocks, 1, "blocks")
	c.Eq(session.KindSystem, s.Blocks[0].Kind, "block kind")
	c.StrContains(s.Blocks[0].Text, "report: progress", "report text")
	c.StrContains(s.Blocks[0].Text, `"step": 3`, "report payload")
}
