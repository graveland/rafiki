// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
)

// prefillReadCmd parses a fake read call's input the way the real tool does.
type prefillReadCmd struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// prefillFakeRead returns a fake read tool that reports each call as
// "<path>@<offset>" (or fails, when failPaths says its path should).
func prefillFakeRead(failPaths map[string]bool) fakeToolSet {
	return fakeToolSet{
		"read": func(_ context.Context, in json.RawMessage) (string, error) {
			var cmd prefillReadCmd
			if err := json.Unmarshal(in, &cmd); err != nil {
				return "", fmt.Errorf("read: invalid input: %w", err)
			}
			if failPaths[cmd.Path] {
				return "", fmt.Errorf("read: open %s: no such file or directory", cmd.Path)
			}
			return fmt.Sprintf("%s@%d", cmd.Path, cmd.Offset), nil
		},
	}
}

// prefillSimpleEngine builds an engine over a fresh in-memory conversation
// with the given pre-fill entries, wired to a capturing sender scripted with
// one clean end_turn reply (so a prompt turn, if the test sends one,
// completes without further calls).
func prefillSimpleEngine(t *testing.T, ts fakeToolSet, entries []protocol.PrefillRead, extra ...func(*EngineConfig)) (*Engine, *syncBuffer, *capturingSender) {
	t.Helper()
	sender := newCapturingSender(t, sampleEndTurn)
	eng, out := newTestEngineWithConfig(t, ts, sender, func(cfg *EngineConfig) {
		cfg.Prefill = entries
		for _, fn := range extra {
			if fn != nil {
				fn(cfg)
			}
		}
	})
	return eng, out, sender
}

// assertPrefillRow checks one persisted pre-fill row's role and block types.
func assertPrefillRow(t *testing.T, m store.Message, ordinal int, role anthropic.MessageParamRole, blockKind string) {
	t.Helper()
	if m.Ordinal != ordinal {
		t.Fatalf("row %d has ordinal %d", ordinal, m.Ordinal)
	}
	if m.Param.Role != role {
		t.Fatalf("row %d role = %v, want %v", ordinal, m.Param.Role, role)
	}
	if len(m.Param.Content) == 0 {
		t.Fatalf("row %d has no content blocks", ordinal)
	}
	for i, b := range m.Param.Content {
		switch blockKind {
		case "text":
			if b.OfText == nil {
				t.Fatalf("row %d block %d is not a text block", ordinal, i)
			}
		case "tool_use":
			if b.OfToolUse == nil {
				t.Fatalf("row %d block %d is not a tool_use block", ordinal, i)
			}
			if b.OfToolUse.Name != "read" {
				t.Fatalf("row %d block %d tool name = %q, want read", ordinal, i, b.OfToolUse.Name)
			}
			if !strings.HasPrefix(b.OfToolUse.ID, "prefill_") {
				t.Fatalf("row %d block %d tool_use id %q lacks the prefill_ prefix", ordinal, i, b.OfToolUse.ID)
			}
		case "tool_result":
			if b.OfToolResult == nil {
				t.Fatalf("row %d block %d is not a tool_result block", ordinal, i)
			}
			if !strings.HasPrefix(b.OfToolResult.ToolUseID, "prefill_") {
				t.Fatalf("row %d block %d tool_result id %q lacks the prefill_ prefix", ordinal, i, b.OfToolResult.ToolUseID)
			}
		default:
			t.Fatalf("unknown block kind %q", blockKind)
		}
	}
}

// TestPrefillRowsShape is the core contract: two entries produce exactly
// r0/r1/r2 with the preamble, the prefill_ ids and the read tool name, and
// the first prompt's request merges r2's tool_results with the task text
// into ONE user message, task text last.
func TestPrefillRowsShape(t *testing.T) {
	eng, out, sender := prefillSimpleEngine(t, prefillFakeRead(nil),
		[]protocol.PrefillRead{{Path: "/tmp/a.txt"}, {Path: "/tmp/b.txt"}})

	eng.HandlePrompt("do the task")
	eng.Wait()

	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history has %d rows, want 5 (r0, r1, r2, task, reply)", len(hist))
	}

	// r0: user, one text block, exactly the preamble.
	assertPrefillRow(t, hist[0], 0, anthropic.MessageParamRoleUser, "text")
	if got := hist[0].Param.Content[0].OfText.Text; got != PrefillPreamble {
		t.Fatalf("r0 text = %q, want %q", got, PrefillPreamble)
	}

	// r1: assistant, only tool_use blocks named read with prefill_ ids.
	assertPrefillRow(t, hist[1], 1, anthropic.MessageParamRoleAssistant, "tool_use")
	wantIDs := []string{"prefill_0001", "prefill_0002"}
	for i, b := range hist[1].Param.Content {
		if b.OfToolUse.ID != wantIDs[i] {
			t.Fatalf("r1 block %d id = %q, want %q", i, b.OfToolUse.ID, wantIDs[i])
		}
		var cmd prefillReadCmd
		in, mErr := json.Marshal(b.OfToolUse.Input)
		if mErr != nil {
			t.Fatalf("marshal r1 block %d input: %v", i, mErr)
		}
		if err := json.Unmarshal(in, &cmd); err != nil {
			t.Fatalf("unmarshal r1 block %d input: %v", i, err)
		}
		wantPath := "/tmp/a.txt"
		if i == 1 {
			wantPath = "/tmp/b.txt"
		}
		if cmd.Path != wantPath {
			t.Fatalf("r1 block %d input path = %q, want %q", i, cmd.Path, wantPath)
		}
		if cmd.Offset != 1 || cmd.Limit != 1_000_000 {
			t.Fatalf("r1 block %d input offset/limit = %d/%d, want 1/1000000", i, cmd.Offset, cmd.Limit)
		}
	}

	// r2: user, one tool_result per r1 id, same order, none an error.
	assertPrefillRow(t, hist[2], 2, anthropic.MessageParamRoleUser, "tool_result")
	for i, b := range hist[2].Param.Content {
		if b.OfToolResult.ToolUseID != wantIDs[i] {
			t.Fatalf("r2 block %d tool_use id = %q, want %q", i, b.OfToolResult.ToolUseID, wantIDs[i])
		}
		if b.OfToolResult.IsError.Value {
			t.Fatalf("r2 block %d is an error result", i)
		}
	}

	// r3: the task prompt, appended by the normal loop — the pre-fill rows
	// sit at ordinals 0-2, strictly before it.
	assertPrefillRow(t, hist[3], 3, anthropic.MessageParamRoleUser, "text")
	if got := hist[3].Param.Content[0].OfText.Text; got != "do the task" {
		t.Fatalf("r3 text = %q, want the task prompt", got)
	}
	if hist[4].Param.Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("row 4 role = %v, want the turn's assistant reply", hist[4].Param.Role)
	}

	// No prefill activity is framed to the frontend, and no read leaked
	// before the prefill rows were in place.
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}

	// The request the sender received: r2's tool_results and the task text
	// merged into ONE user message, task text last.
	params := sender.lastParams(t)
	if len(params.Messages) != 3 {
		t.Fatalf("request has %d messages, want 3 (preamble, tool_use, merged tool_results+task)", len(params.Messages))
	}
	merged := params.Messages[2]
	if merged.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("request message 2 role = %v, want user", merged.Role)
	}
	if len(merged.Content) != 3 {
		t.Fatalf("merged user message has %d blocks, want 2 tool_results + 1 text", len(merged.Content))
	}
	for i, id := range wantIDs {
		tr := merged.Content[i].OfToolResult
		if tr == nil || tr.ToolUseID != id {
			t.Fatalf("merged block %d is not a tool_result for %q", i, id)
		}
	}
	last := merged.Content[2]
	if last.OfText == nil || last.OfText.Text != "do the task" {
		t.Fatalf("merged block 2 = %+v, want the task text last", last)
	}
}

// TestPrefillMissingFileIsErrorResult: one existing and one missing file —
// the failed read is recorded as an is_error result and the walk continues;
// the child keeps running and its first turn still happens.
func TestPrefillMissingFileIsErrorResult(t *testing.T) {
	eng, out, sender := prefillSimpleEngine(t, prefillFakeRead(map[string]bool{"/tmp/missing.txt": true}),
		[]protocol.PrefillRead{{Path: "/tmp/a.txt"}, {Path: "/tmp/missing.txt"}})

	eng.HandlePrompt("go")
	eng.Wait()

	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history has %d rows, want 5 — the engine must keep running past a failed read", len(hist))
	}
	errResults := 0
	for i, b := range hist[2].Param.Content {
		tr := b.OfToolResult
		if tr == nil {
			t.Fatalf("r2 block %d is not a tool_result", i)
		}
		if tr.IsError.Value {
			errResults++
			if !strings.Contains(blockText(t, tr), "no such file") {
				t.Fatalf("error result %q does not name the failure", blockText(t, tr))
			}
		}
	}
	if errResults != 1 {
		t.Fatalf("r2 has %d is_error results, want 1", errResults)
	}
	if sender.callCount() != 1 {
		t.Fatalf("sender was called %d times, want 1 (the prompt turn)", sender.callCount())
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("a failed read must not be fatal: %s", msg)
	}
}

// blockText flattens a tool_result block's text content for assertions.
func blockText(t *testing.T, tr *anthropic.ToolResultBlockParam) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range tr.Content {
		if c.OfText != nil {
			sb.WriteString(c.OfText.Text)
		}
	}
	return sb.String()
}

// TestPrefillAllReadsFailIsFatal: when every read failed the child gained
// nothing, so the pre-fill fails and the engine ends the child.
func TestPrefillAllReadsFailIsFatal(t *testing.T) {
	fatalCalled := make(chan error, 1)
	eng, out, _ := prefillSimpleEngine(t, prefillFakeRead(map[string]bool{"/tmp/a.txt": true}),
		[]protocol.PrefillRead{{Path: "/tmp/a.txt"}}, func(cfg *EngineConfig) {
			cfg.OnFatal = func(err error) { fatalCalled <- err }
		})

	select {
	case err := <-fatalCalled:
		if err == nil || !strings.Contains(err.Error(), "prefill: every read failed") {
			t.Fatalf("fatal error = %v, want it to name every read failed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnFatal was never called for an all-failing pre-fill")
	}
	if msg := out.String(); !strings.Contains(msg, "agent_error") {
		t.Fatalf("expected an agent_error frame, got %q", msg)
	}
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("history has %d rows, want none persisted on failure", len(hist))
	}
}

// TestPrefillPagesLargeFile: an open-ended entry whose first read carries a
// continuation trailer pages again — two tool_use blocks for one entry, at
// offsets 1 and next.
func TestPrefillPagesLargeFile(t *testing.T) {
	var cmds []prefillReadCmd
	ts := fakeToolSet{
		"read": func(_ context.Context, in json.RawMessage) (string, error) {
			var cmd prefillReadCmd
			if err := json.Unmarshal(in, &cmd); err != nil {
				return "", err
			}
			cmds = append(cmds, cmd)
			if cmd.Offset == 1 {
				return "     1\tfirst\n     2\tsecond" +
					"\n[showing lines 1-2; more lines remain — pass offset=3 to continue]\n", nil
			}
			return fmt.Sprintf("%s@%d full page", cmd.Path, cmd.Offset), nil
		},
	}
	eng, out, _ := prefillSimpleEngine(t, ts, []protocol.PrefillRead{{Path: "/tmp/big.txt"}})

	eng.HandlePrompt("go")
	eng.Wait()

	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history has %d rows, want 5 (prefill, task, reply)", len(hist))
	}
	if len(hist[1].Param.Content) != 2 {
		t.Fatalf("r1 has %d blocks, want 2 pages for one entry", len(hist[1].Param.Content))
	}
	if len(cmds) != 2 {
		t.Fatalf("fake read was called %d times, want 2", len(cmds))
	}
	if cmds[0].Offset != 1 || cmds[1].Offset != 3 {
		t.Fatalf("read calls at offsets %v, want [1 3]", cmds)
	}
	if cmds[0].Limit != 1_000_000 || cmds[1].Limit != 1_000_000 {
		t.Fatalf("open-ended pages carry limit %d/%d, want the open limit both times", cmds[0].Limit, cmds[1].Limit)
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}
}

// TestPrefillBoundedRangeStopsAtEnd: a bounded entry (Start 10, End 20) is
// one call with offset 10 and limit 11, even when the result carries a
// trailer — next (21) is past End.
func TestPrefillBoundedRangeStopsAtEnd(t *testing.T) {
	var cmds []prefillReadCmd
	ts := fakeToolSet{
		"read": func(_ context.Context, in json.RawMessage) (string, error) {
			var cmd prefillReadCmd
			if err := json.Unmarshal(in, &cmd); err != nil {
				return "", err
			}
			cmds = append(cmds, cmd)
			return "     10\tx" + "\n[showing lines 10-10; more lines remain — pass offset=21 to continue]\n", nil
		},
	}
	eng, out, _ := prefillSimpleEngine(t, ts, []protocol.PrefillRead{{Path: "/tmp/ranged.txt", Start: 10, End: 20}})

	eng.HandlePrompt("go")
	eng.Wait()

	if len(cmds) != 1 {
		t.Fatalf("read was called %d times, want 1 (bounded range stops at End)", len(cmds))
	}
	if cmds[0].Offset != 10 || cmds[0].Limit != 11 {
		t.Fatalf("read input offset/limit = %d/%d, want 10/11", cmds[0].Offset, cmds[0].Limit)
	}
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history has %d rows, want 5 (prefill, task, reply)", len(hist))
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}
}

// TestPrefillQuotedTrailerStopsPaging: a COMPLETE read whose last CONTENT
// line quotes the read trailer (e.g. a doc quoting rafiki's own trailer, or
// saved read output) parses as a continuation — ReadContinuation matches the
// trailer's tail anywhere — with an offset that does not advance past the
// page just shown. Paging must stop on the quote instead of re-reading the
// same page forever: exactly one recorded call per entry, open-ended and
// bounded ranges alike.
func TestPrefillQuotedTrailerStopsPaging(t *testing.T) {
	var cmds []prefillReadCmd
	ts := fakeToolSet{
		"read": func(_ context.Context, in json.RawMessage) (string, error) {
			var cmd prefillReadCmd
			if err := json.Unmarshal(in, &cmd); err != nil {
				return "", err
			}
			cmds = append(cmds, cmd)
			// No real trailer: the last line is file CONTENT quoting the
			// trailer text with offset=1 — self-consistent with either
			// entry's first page, so paging it again makes no progress.
			return "     1\ta doc quoting our read trailer:\n     2\tpass offset=1 to continue]\n", nil
		},
	}
	eng, out, _ := prefillSimpleEngine(t, ts, []protocol.PrefillRead{
		{Path: "/tmp/quoted-open.txt"},
		{Path: "/tmp/quoted-ranged.txt", Start: 1, End: 10},
	})

	eng.HandlePrompt("go")
	eng.Wait()

	if len(cmds) != 2 {
		t.Fatalf("read was called %d times, want 2 (one page per entry, none on the quote)", len(cmds))
	}
	for i, cmd := range cmds {
		if cmd.Offset != 1 {
			t.Fatalf("call %d paged to offset %d, want a single page at 1", i, cmd.Offset)
		}
	}
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history has %d rows, want 5 (prefill, task, reply)", len(hist))
	}
	if len(hist[1].Param.Content) != 2 {
		t.Fatalf("r1 has %d blocks, want 2 (one per entry)", len(hist[1].Param.Content))
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}
}

// TestPrefillGlobSortedAndExpanded: a glob entry expands to one open-ended
// read per match, in lexicographic order — not the tool's mtime order.
func TestPrefillGlobSortedAndExpanded(t *testing.T) {
	var paths []string
	ts := fakeToolSet{
		"glob": func(_ context.Context, in json.RawMessage) (string, error) {
			var gi struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(in, &gi); err != nil {
				return "", err
			}
			if gi.Pattern != "*.txt" || gi.Path != "/b" {
				return "", fmt.Errorf("glob input = %s, want pattern *.txt path /b", in)
			}
			return "/b/zzz.txt\n/b/aaa.txt\n/b/mmm.txt\n", nil
		},
		"read": func(_ context.Context, in json.RawMessage) (string, error) {
			var cmd prefillReadCmd
			if err := json.Unmarshal(in, &cmd); err != nil {
				return "", err
			}
			paths = append(paths, cmd.Path)
			return cmd.Path + " body", nil
		},
	}
	eng, out, _ := prefillSimpleEngine(t, ts, []protocol.PrefillRead{{Path: "/b/*.txt"}})

	eng.HandlePrompt("go")
	eng.Wait()

	want := []string{"/b/aaa.txt", "/b/mmm.txt", "/b/zzz.txt"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("reads in order %v, want lexicographic %v", paths, want)
	}
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history has %d rows, want 5 (prefill, task, reply)", len(hist))
	}
	// Glob calls are NOT recorded; r1 carries one tool_use per matched path.
	if len(hist[1].Param.Content) != 3 {
		t.Fatalf("r1 has %d blocks, want 3 reads (no glob block)", len(hist[1].Param.Content))
	}
	for i, b := range hist[1].Param.Content {
		in, _ := json.Marshal(b.OfToolUse.Input)
		var cmd prefillReadCmd
		if err := json.Unmarshal(in, &cmd); err != nil {
			t.Fatalf("unmarshal r1 block %d: %v", i, err)
		}
		if cmd.Path != want[i] {
			t.Fatalf("r1 block %d path = %q, want %q", i, cmd.Path, want[i])
		}
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}
}

// TestPrefillGlobNoMatchIsFatal: a glob that matched nothing must not look
// like a completed pre-fill — the child ends with an agent_error instead.
func TestPrefillGlobNoMatchIsFatal(t *testing.T) {
	testPrefillGlobFatal(t, "no files matched\n", "matched nothing")
}

// TestPrefillGlobOverflowIsFatal: an overflowing glob is refused rather than
// silently reading an arbitrary 200 of 4000 files.
func TestPrefillGlobOverflowIsFatal(t *testing.T) {
	testPrefillGlobFatal(t, "/b/a.txt\n/b/b.txt\n[more matches omitted]\n", "more than 200 files")
}

func testPrefillGlobFatal(t *testing.T, globResult, wantErrPart string) {
	t.Helper()
	ts := fakeToolSet{
		"glob": func(context.Context, json.RawMessage) (string, error) { return globResult, nil },
		"read": func(context.Context, json.RawMessage) (string, error) {
			return "should not be reached", nil
		},
	}
	fatalCalled := make(chan error, 1)
	eng, out, _ := prefillSimpleEngine(t, ts, []protocol.PrefillRead{{Path: "/b/*.txt"}}, func(cfg *EngineConfig) {
		cfg.OnFatal = func(err error) { fatalCalled <- err }
	})

	select {
	case err := <-fatalCalled:
		if !strings.Contains(err.Error(), wantErrPart) {
			t.Fatalf("fatal error = %v, want it to name %q", err, wantErrPart)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnFatal was never called for a failed glob")
	}
	if msg := out.String(); !strings.Contains(msg, "agent_error") {
		t.Fatalf("expected an agent_error frame, got %q", msg)
	}
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("history has %d rows, want none persisted on failure", len(hist))
	}
}

// TestPrefillOverCapPersistsNothing: an estimated footprint beyond 60% of
// the model's context window (128000 fallback, catalog empty in tests)
// persists NOTHING and ends the child with the estimate, the cap and the
// five largest reads named.
func TestPrefillOverCapPersistsNothing(t *testing.T) {
	huge := strings.Repeat("x", 76800*4+4000) // > 76800 tokens' worth of bytes
	ts := fakeToolSet{
		"read": func(context.Context, json.RawMessage) (string, error) { return huge, nil },
	}
	fatalCalled := make(chan error, 1)
	eng, out, _ := prefillSimpleEngine(t, ts, []protocol.PrefillRead{{Path: "/tmp/huge.txt"}}, func(cfg *EngineConfig) {
		cfg.OnFatal = func(err error) { fatalCalled <- err }
	})

	select {
	case err := <-fatalCalled:
		for _, part := range []string{"estimated", "exceeds", "/tmp/huge.txt"} {
			if !strings.Contains(err.Error(), part) {
				t.Fatalf("over-cap error %q does not name %q", err.Error(), part)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnFatal was never called for an over-cap pre-fill")
	}
	if msg := out.String(); !strings.Contains(msg, "agent_error") {
		t.Fatalf("expected an agent_error frame, got %q", msg)
	}
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("history has %d rows, want NOTHING persisted before the cap check", len(hist))
	}
}

// TestPrefillNeedsReadTool: without the read tool the pre-fill is refused at
// worker start and nothing is persisted.
func TestPrefillNeedsReadTool(t *testing.T) {
	testPrefillToolRefused(t, fakeToolSet{
		"grep": func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}, []protocol.PrefillRead{{Path: "/tmp/a.txt"}}, "read tool")
}

// TestPrefillGlobNeedsGlobTool: a glob entry with a read tool but no glob
// tool is refused the same way.
func TestPrefillGlobNeedsGlobTool(t *testing.T) {
	testPrefillToolRefused(t, prefillFakeRead(nil),
		[]protocol.PrefillRead{{Path: "/b/*.txt"}}, "glob tool")
}

func testPrefillToolRefused(t *testing.T, ts fakeToolSet, entries []protocol.PrefillRead, wantErrPart string) {
	t.Helper()
	fatalCalled := make(chan error, 1)
	eng, out, _ := prefillSimpleEngine(t, ts, entries, func(cfg *EngineConfig) {
		cfg.OnFatal = func(err error) { fatalCalled <- err }
	})

	select {
	case err := <-fatalCalled:
		if !strings.Contains(err.Error(), wantErrPart) {
			t.Fatalf("fatal error = %v, want it to name the missing %q", err, wantErrPart)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnFatal was never called for a pre-fill with missing tools")
	}
	if msg := out.String(); !strings.Contains(msg, "agent_error") {
		t.Fatalf("expected an agent_error frame, got %q", msg)
	}
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("history has %d rows, want none persisted", len(hist))
	}
}

// TestPrefillInputsAreByteStable: two runs over the same entries must
// produce byte-identical r1 content — SeedHistory's idempotence check (and
// every restart's HasR1 replay) relies on it.
func TestPrefillInputsAreByteStable(t *testing.T) {
	entries := []protocol.PrefillRead{
		{Path: "/tmp/a.txt"},
		{Path: "/tmp/ranged.txt", Start: 10, End: 20},
	}
	hist1 := runPrefillInMemory(t, entries)
	hist2 := runPrefillInMemory(t, entries)

	got1, err := json.Marshal(hist1[1].Param)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := json.Marshal(hist2[1].Param)
	if err != nil {
		t.Fatal(err)
	}
	if string(got1) != string(got2) {
		t.Fatalf("r1 JSON differs between runs:\n%s\nvs\n%s", got1, got2)
	}
	if !strings.Contains(string(got1), `"path":"/tmp/a.txt","offset":1,"limit":1000000`) &&
		!strings.Contains(string(got1), `{"path":"/tmp/a.txt","offset":1,"limit":1000000}`) {
		t.Fatalf("r1 JSON does not carry the byte-stable field order: %s", got1)
	}
}

// runPrefillInMemory runs one engine's pre-fill (via a prompt turn, which
// serialises behind it) and returns the resulting history.
func runPrefillInMemory(t *testing.T, entries []protocol.PrefillRead) []store.Message {
	t.Helper()
	eng, out, _ := prefillSimpleEngine(t, prefillFakeRead(nil), entries)
	eng.HandlePrompt("go")
	eng.Wait()
	hist, err := eng.conv.History(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history has %d rows, want 5 (prefill, task, reply)", len(hist))
	}
	if msg := out.String(); strings.Contains(msg, "agent_error") {
		t.Fatalf("unexpected agent_error frames: %s", msg)
	}
	return hist
}

// prefillTestRow builds one store.Message for classifyPrefill's table.
func prefillTestRow(param llm.Message) store.Message {
	return store.Message{Param: param, ToolUseIDs: store.ToolUseIDsOf(param)}
}

// TestPrefillClassify is the table over histories classifyPrefill must
// recognise — and, critically, must leave alone.
func TestPrefillClassify(t *testing.T) {
	r0 := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(PrefillPreamble)},
	}
	r1 := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolUseBlock("prefill_0001", json.RawMessage(`{"path":"a.txt","offset":1,"limit":1000000}`), "read"),
		},
	}
	r2 := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolResultBlock("prefill_0001", "body", false),
		},
	}
	task := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("do it")},
	}
	wrongPreamble := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("please read these files first.")},
	}
	realID := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolUseBlock("toolu_01ABC", json.RawMessage(`{"path":"a.txt","offset":1,"limit":1000000}`), "read"),
		},
	}

	tests := []struct {
		name       string
		history    []store.Message
		configured bool
		want       prefillState
	}{
		{"empty configured", nil, true, prefillEmpty},
		{"empty unconfigured", nil, false, prefillNone},
		{"r0 only", []store.Message{prefillTestRow(r0)}, true, prefillHasR0},
		{"r0+r1", []store.Message{prefillTestRow(r0), prefillTestRow(r1)}, true, prefillHasR1},
		{"complete", []store.Message{prefillTestRow(r0), prefillTestRow(r1), prefillTestRow(r2)}, true, prefillComplete},
		// A pre-fill-SHAPED history is recognised even without the field, so
		// the shaped tail can never reach Resume.
		{"complete unconfigured", []store.Message{prefillTestRow(r0), prefillTestRow(r1), prefillTestRow(r2)}, false, prefillComplete},
		{"complete plus task row", []store.Message{prefillTestRow(r0), prefillTestRow(r1), prefillTestRow(r2), prefillTestRow(task)}, true, prefillNone},
		{"wrong preamble", []store.Message{prefillTestRow(wrongPreamble)}, true, prefillNone},
		{"non-prefill id", []store.Message{prefillTestRow(r0), prefillTestRow(realID)}, true, prefillNone},
		{"four rows not shaped", []store.Message{prefillTestRow(r0), prefillTestRow(r1), prefillTestRow(r2), prefillTestRow(task)}, false, prefillNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPrefill(tc.history, tc.configured); got != tc.want {
				t.Fatalf("classifyPrefill(%d rows, configured=%v) = %d, want %d", len(tc.history), tc.configured, got, tc.want)
			}
		})
	}
}

// TestPrefillAssistantRowHasNoUsage: r1 is persisted with a nil meta, so the
// in-memory row carries no stop reason (the DB test pins the NULL tokens).
func TestPrefillAssistantRowHasNoUsage(t *testing.T) {
	hist := runPrefillInMemory(t, []protocol.PrefillRead{{Path: "/tmp/a.txt"}})
	if hist[1].StopReason != "" {
		t.Fatalf("r1 stop reason = %q, want empty (nil meta — usage not reported)", hist[1].StopReason)
	}
	if hist[1].Param.Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("r1 role = %v", hist[1].Param.Role)
	}
}
