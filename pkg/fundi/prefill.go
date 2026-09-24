// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/prefill"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
)

// PrefillPreamble is the first row (r0) of a persisted pre-fill: one user
// text block telling the model why files appear before its first turn.
const PrefillPreamble = "[rafiki prefill] The files for this task were read for you before this conversation started; their contents follow."

const (
	// prefillIDPrefix marks the synthetic tool_use/tool_result rows the
	// pre-fill persists. The prefix on r1/r2 plus NULL usage on r1 is the
	// provenance marker distinguishing a pre-fill from a real turn.
	prefillIDPrefix = "prefill_"
	// prefillFallbackContext is the context window the token cap assumes when
	// the model catalog doesn't know the child's model.
	prefillFallbackContext = 128000
	// prefillCapPercent is the share of the context window the pre-fill's
	// estimated token footprint may not exceed.
	prefillCapPercent = 60
	// prefillOpenLimit is the line limit passed to read for an entry with no
	// End (an open-ended read). Larger than any real file: the tool's own
	// byte budget is what actually caps the result.
	prefillOpenLimit = 1_000_000
)

// prefillState classifies a conversation's history against the pre-fill row
// shape (r0 preamble, r1 tool_use, r2 tool_result) so a restart can finish a
// pre-fill a dead process left partial — and so a pre-fill-shaped tail can
// never be handed to agentloop.Resume.
type prefillState int

const (
	// prefillNone: no pre-fill work to do. Either the history is empty and no
	// pre-fill is configured, or the history has grown past (or away from)
	// the pre-fill shape and must be left alone.
	prefillNone prefillState = iota
	// prefillEmpty: empty history, pre-fill configured — run the whole thing.
	prefillEmpty
	// prefillHasR0: only the preamble row exists — a previous process died
	// before persisting r1/r2. Persist the missing rows.
	prefillHasR0
	// prefillHasR1: r0+r1 exist — the process died after persisting the
	// tool_use row but before its results. Re-execute r1's inputs verbatim
	// and persist r2.
	prefillHasR1
	// prefillComplete: exactly r0, r1, r2. Nothing to run — and startupResume
	// must also skip: the tail is user tool_results, and Resume would
	// Continue, calling the model with the files and no task.
	prefillComplete
)

// classifyPrefill maps a conversation's history to a prefillState. configured
// only separates the empty-history cases: a pre-fill-SHAPED history is
// recognised even when the engine was rebuilt without the field, so the
// shaped tail is never misread as resumable work.
func classifyPrefill(history []store.Message, configured bool) prefillState {
	if len(history) == 0 {
		if configured {
			return prefillEmpty
		}
		return prefillNone
	}
	if len(history) > 3 {
		return prefillNone
	}
	if !prefillRowIsR0(history[0]) {
		return prefillNone
	}
	if len(history) == 1 {
		return prefillHasR0
	}
	if !prefillRowIsR1(history[1]) {
		return prefillNone
	}
	if len(history) == 2 {
		return prefillHasR1
	}
	if !prefillRowIsR2(history[2]) {
		return prefillNone
	}
	return prefillComplete
}

// prefillRowIsR0: user with exactly one text block equal to PrefillPreamble.
func prefillRowIsR0(m store.Message) bool {
	if m.Param.Role != anthropic.MessageParamRoleUser {
		return false
	}
	if len(m.Param.Content) != 1 || m.Param.Content[0].OfText == nil {
		return false
	}
	return m.Param.Content[0].OfText.Text == PrefillPreamble
}

// prefillRowIsR1: assistant whose blocks are all tool_use with prefill_ ids.
func prefillRowIsR1(m store.Message) bool {
	if m.Param.Role != anthropic.MessageParamRoleAssistant {
		return false
	}
	if len(m.Param.Content) == 0 {
		return false
	}
	for _, b := range m.Param.Content {
		if b.OfToolUse == nil || !strings.HasPrefix(b.OfToolUse.ID, prefillIDPrefix) {
			return false
		}
	}
	return true
}

// prefillRowIsR2: user whose blocks are all tool_result with prefill_ ids.
func prefillRowIsR2(m store.Message) bool {
	if m.Param.Role != anthropic.MessageParamRoleUser {
		return false
	}
	if len(m.Param.Content) == 0 {
		return false
	}
	for _, b := range m.Param.Content {
		if b.OfToolResult == nil || !strings.HasPrefix(b.OfToolResult.ToolUseID, prefillIDPrefix) {
			return false
		}
	}
	return true
}

// prefillReadInput marshals a read call's input. Field order is the byte
// stability contract: two runs over the same entries must produce identical
// r1 content, which SeedHistory's divergence check (and
// TestPrefillInputsAreByteStable) rely on.
type prefillReadInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// prefillGlobInput marshals a glob call's input, in the tool schema's field
// order. Glob calls are executed but never recorded as history.
type prefillGlobInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// prefillCall is one read the pre-fill executed (or attempted), in call
// order: one tool_use block in r1 and one tool_result block in r2. Glob
// executions are never recorded — only the reads their matches expand to.
type prefillCall struct {
	id      string          // prefill_0001-style; assigned in call order
	input   json.RawMessage // the marshalled tool_use input, byte-stable
	path    string          // for the over-cap listing
	result  string          // the tool result, or the Go error's text
	isError bool
}

// runPrefill executes the spawn's pre-fill and persists the synthetic
// tool_use/tool_result rows. Called by worker() at engine start, before any
// queued prompt is consumed, on a history that is empty (prefillEmpty) or a
// partial pre-fill (prefillHasR0/prefillHasR1).
//
// It emits NO frontend frames: an AgentStart/AgentEnd pair outside a turn
// would drive the settle path, and a child quietly reading its files before
// the first turn is exactly the intent.
func (e *Engine) runPrefill(ctx context.Context, history []store.Message, st prefillState) error {
	if err := e.prefillToolsAvailable(); err != nil {
		return err
	}

	// Build the call list. A persisted r1 is replayed verbatim — ids and
	// inputs come from history, so r2 matches what those ids promise.
	var calls []prefillCall
	if st == prefillHasR1 {
		for _, b := range history[1].Param.Content {
			tu := b.OfToolUse
			if tu == nil {
				continue // classifyPrefill guarantees all tool_use
			}
			input, err := json.Marshal(tu.Input)
			if err != nil {
				return fmt.Errorf("prefill: re-marshal tool_use %s input: %w", tu.ID, err)
			}
			result, execErr := e.tools.Execute(ctx, "read", input)
			call := prefillCall{id: tu.ID, input: input, path: prefillInputPath(input), result: result}
			if execErr != nil {
				call.isError = true
				call.result = execErr.Error()
			}
			calls = append(calls, call)
		}
	} else {
		var err error
		if calls, err = e.executePrefillEntries(ctx, e.prefill); err != nil {
			return err
		}
		for i := range calls {
			calls[i].id = fmt.Sprintf("prefill_%04d", i+1)
		}
	}

	// A child that gained nothing cannot start its task.
	if len(calls) == 0 {
		return errors.New("prefill: every read failed")
	}
	allFailed := true
	for _, c := range calls {
		if !c.isError {
			allFailed = false
			break
		}
	}
	if allFailed {
		return errors.New("prefill: every read failed")
	}

	// Token cap, checked BEFORE any persistence: a pre-fill that overflows
	// the model's window must not poison the conversation with half of
	// itself.
	total := 0
	for _, c := range calls {
		total += len(c.input) + len(c.result)
	}
	est := (total + 3) / 4
	ctxLen := prefillContextWindow(e.client, e.state.ModelID)
	tokenCap := ctxLen * prefillCapPercent / 100
	if est > tokenCap {
		return fmt.Errorf("prefill: estimated %d tokens exceeds the %d-token cap (%d%% of the model's context window); largest reads: %s",
			est, tokenCap, prefillCapPercent, prefillLargestCalls(calls, 5))
	}

	// Persist r0/r1/r2. In the resumed-partial cases the existing rows are
	// passed through VERBATIM so SeedHistory's idempotence check sees
	// identical content; only the missing rows are new.
	r0 := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(PrefillPreamble)},
	}
	r1 := anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant}
	switch st {
	case prefillHasR0:
		r0 = history[0].Param
	case prefillHasR1:
		r0 = history[0].Param
		r1 = history[1].Param
	}
	r2 := anthropic.MessageParam{Role: anthropic.MessageParamRoleUser}
	// In the HasR1 replay r1 already carries its tool_use blocks — the calls
	// were built FROM them, so appending again would duplicate every block
	// and diverge the persisted row.
	if st != prefillHasR1 {
		for _, c := range calls {
			r1.Content = append(r1.Content, anthropic.NewToolUseBlock(c.id, json.RawMessage(c.input), "read"))
		}
	}
	for _, c := range calls {
		r2.Content = append(r2.Content, anthropic.NewToolResultBlock(c.id, c.result, c.isError))
	}
	if err := e.conv.SeedHistory(ctx, []llm.Message{r0, r1, r2}); err != nil {
		return fmt.Errorf("prefill: seed history: %w", err)
	}

	slog.Info("agent: prefill complete", "conversation", e.conv.ID, "reads", len(calls), "est_tokens", est)
	return nil
}

// executePrefillEntries executes the configured entries in order, expanding
// globs and paging truncated reads. A glob that matched nothing, overflowed
// the tool's cap, or errored is FATAL (returned): silently reading nothing —
// or an arbitrary 200 of 4000 files — would look like a completed pre-fill
// while the child missed its own task context. A read's Go error, by
// contrast, is recorded as an is_error call and the walk continues: a
// missing file must not take the child down.
func (e *Engine) executePrefillEntries(ctx context.Context, entries []protocol.PrefillRead) ([]prefillCall, error) {
	var calls []prefillCall
	for _, entry := range entries {
		if prefill.IsGlob(entry.Path) {
			matches, err := e.executePrefillGlob(ctx, entry.Path)
			if err != nil {
				return nil, err
			}
			for _, path := range matches {
				pageCalls, err := e.readPrefillPath(ctx, path, 0, 0)
				if err != nil {
					return nil, err
				}
				calls = append(calls, pageCalls...)
			}
			continue
		}
		pageCalls, err := e.readPrefillPath(ctx, entry.Path, entry.Start, entry.End)
		if err != nil {
			return nil, err
		}
		calls = append(calls, pageCalls...)
	}
	return calls, nil
}

// readPrefillPath executes one read entry, following continuation trailers
// while the entry's range allows it. Every page is its own recorded call.
// A Go error from the tool is recorded (is_error) and the walk stops — a
// missing file is not fatal.
func (e *Engine) readPrefillPath(ctx context.Context, path string, start, end int) ([]prefillCall, error) {
	offset := start
	if offset == 0 {
		offset = 1
	}
	unbounded := end == 0
	limit := prefillOpenLimit
	if !unbounded {
		limit = end - offset + 1
	}
	var calls []prefillCall
	for {
		input, err := json.Marshal(prefillReadInput{Path: path, Offset: offset, Limit: limit})
		if err != nil {
			return nil, fmt.Errorf("prefill: marshal read input for %q: %w", path, err)
		}
		call := prefillCall{input: input, path: path}
		result, execErr := e.tools.Execute(ctx, "read", input)
		call.result = result
		if execErr != nil {
			call.isError = true
			call.result = execErr.Error()
		}
		calls = append(calls, call)
		if execErr != nil {
			return calls, nil
		}
		next, ok := tools.ReadContinuation(call.result)
		// next > offset is the progress guard. ReadContinuation matches the
		// trailer's tail anywhere in a result, so a COMPLETE read whose last
		// content line merely quotes the trailer (with an offset pointing back
		// into the file) parses as a continuation without advancing — paging
		// it again would re-read the same page forever. A real trailer always
		// resumes past the page just shown (next = lastShown+1 > offset).
		if !ok || next <= offset || (!unbounded && next > end) {
			return calls, nil
		}
		if !unbounded {
			limit = end - next + 1
		}
		offset = next
	}
}

// executePrefillGlob runs one glob entry and returns the matched paths,
// sorted lexicographically, each to be read as an open-ended entry.
func (e *Engine) executePrefillGlob(ctx context.Context, entry string) ([]string, error) {
	base, pattern := splitGlobEntry(entry)
	input, err := json.Marshal(prefillGlobInput{Pattern: pattern, Path: base})
	if err != nil {
		return nil, fmt.Errorf("prefill: marshal glob input for %q: %w", entry, err)
	}
	result, err := e.tools.Execute(ctx, "glob", input)
	if err != nil {
		return nil, fmt.Errorf("prefill: glob %q: %w", entry, err)
	}
	lines := strings.Split(strings.TrimRight(result, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "no files matched" {
		return nil, fmt.Errorf("prefill: glob %q matched nothing", entry)
	}
	if lines[len(lines)-1] == "[more matches omitted]" {
		return nil, fmt.Errorf("prefill: glob %q matched more than 200 files; narrow it", entry)
	}
	paths := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		paths = append(paths, l)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("prefill: glob %q matched nothing", entry)
	}
	sort.Strings(paths)
	return paths, nil
}

// splitGlobEntry splits a glob entry into the base directory glob executes
// from and the pattern relative to it, at the first path segment that
// carries a glob metachar. base is "." when every segment is globby
// (e.g. "*.go"), and keeps the leading slash of an absolute path.
func splitGlobEntry(path string) (base, pattern string) {
	slashed := filepath.ToSlash(path)
	for off := 0; ; {
		idx := strings.IndexByte(slashed[off:], '/')
		seg := slashed[off:]
		if idx >= 0 {
			seg = slashed[off : off+idx]
		}
		if prefill.IsGlob(seg) {
			if off == 0 {
				return ".", slashed
			}
			return strings.TrimSuffix(slashed[:off], "/"), slashed[off:]
		}
		if idx < 0 {
			// No globby segment — unreachable for an entry IsGlob accepted,
			// but the safe split is everything as the pattern.
			return ".", slashed
		}
		off += idx + 1
	}
}

// prefillToolsAvailable re-checks, at worker start, that the child's built-in
// tool set can actually carry out the pre-fill: read always, glob when any
// entry is a glob. The controller refuses a spawn whose tool set lacks
// these; this is the same defence on the executor side.
func (e *Engine) prefillToolsAvailable() error {
	names := make(map[string]bool)
	for _, d := range e.tools.Definitions() {
		if d.OfTool != nil {
			names[d.OfTool.Name] = true
		}
	}
	if !names["read"] {
		return errors.New("prefill: the read tool is not available to this child")
	}
	for _, entry := range e.prefill {
		if prefill.IsGlob(entry.Path) && !names["glob"] {
			return errors.New("prefill: the glob tool is not available to this child")
		}
	}
	return nil
}

// prefillContextWindow returns the context window the token cap is computed
// against: the catalog's figure for the child's model when it knows it, else
// prefillFallbackContext.
//
// modelID must be the provider-local id (EngineConfig.ModelID, already split
// and alias-resolved by Providers.Split). The catalog indexes OpenRouter-native
// ids, so a provider-qualified "openrouter/z-ai/…" never matches and would
// silently cap every pre-fill at the fallback's 60%.
func prefillContextWindow(client *llm.Client, modelID string) int {
	if cat := catalogOf(client); cat != nil {
		if ctxLen, _, ok := cat.ContextWindow(modelID); ok {
			return ctxLen
		}
	}
	return prefillFallbackContext
}

// prefillInputPath extracts the path field from a persisted tool_use input
// for the over-cap listing. A parse failure yields the raw JSON — the
// listing is diagnostic, not load-bearing.
func prefillInputPath(input json.RawMessage) string {
	var in prefillReadInput
	if err := json.Unmarshal(input, &in); err != nil || in.Path == "" {
		return string(input)
	}
	return in.Path
}

// prefillLargestCalls names the n biggest calls by estimated tokens, for the
// over-cap error.
func prefillLargestCalls(calls []prefillCall, n int) string {
	type sized struct {
		path   string
		tokens int
	}
	sizes := make([]sized, len(calls))
	for i, c := range calls {
		sizes[i] = sized{path: c.path, tokens: (len(c.input) + len(c.result) + 3) / 4}
	}
	sort.Slice(sizes, func(i, j int) bool {
		if sizes[i].tokens != sizes[j].tokens {
			return sizes[i].tokens > sizes[j].tokens
		}
		return sizes[i].path < sizes[j].path
	})
	if len(sizes) > n {
		sizes = sizes[:n]
	}
	parts := make([]string, len(sizes))
	for i, s := range sizes {
		parts[i] = fmt.Sprintf("%s ~%d tok", s.path, s.tokens)
	}
	return strings.Join(parts, ", ")
}
