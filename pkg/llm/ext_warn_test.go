// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// captureHandler records every log record it sees.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// warnedKeys returns the "keys" attribute of every warning recorded.
func (h *captureHandler) warnedKeys(t *testing.T) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "keys" {
				out = append(out, a.Value.String())
			}
			return true
		})
	}
	return out
}

// resetExtWarnings clears the process-wide dedupe set, so one test's keys
// cannot silence another's.
func resetExtWarnings() {
	extWarnMu.Lock()
	extWarnSeen = make(map[string]struct{})
	extWarnMu.Unlock()
}

// extResponse is a one-block response whose block carries ext.
func extResponse(block msg.Block) *msg.Response {
	return &msg.Response{ID: "m", Content: []msg.Block{block}}
}

// TestWarnNewExtOncePerKey pins the dedupe: a provider growing a field is worth
// ONE line per process per (dialect, block type, key set), not one per response.
// The same key on a different block type is a different fact and warns again.
func TestWarnNewExtOncePerKey(t *testing.T) {
	c := assert.NewCollecting(t)
	resetExtWarnings()
	t.Cleanup(resetExtWarnings)

	h := &captureHandler{}
	logger := slog.New(h)

	ext := msg.Ext{Dialect: msg.DialectAnthropic, Raw: `{"caller":{"type":"x"},"citations":[]}`}
	resp := extResponse(msg.Text{Text: "hi", Ext: ext})
	warnNewExt(logger, resp)
	warnNewExt(logger, resp)
	warnNewExt(logger, resp)

	keys := h.warnedKeys(t)
	c.Require().Len(keys, 1, "warnings")
	c.Eq("caller,citations", keys[0], "the key set must be sorted and comma-joined")

	// A nested tool_result's inner block is walked too, and its own key set is
	// a separate line.
	warnNewExt(logger, extResponse(msg.ToolResult{ToolUseID: "t", Content: []msg.Block{
		msg.Text{Text: "inner", Ext: msg.Ext{Dialect: msg.DialectAnthropic, Raw: `{"annotations":1}`}},
	}}))
	c.Eq(2, len(h.warnedKeys(t)), "a nested block's residue must warn on its own")

	// A block with no residue is not a warning at all.
	warnNewExt(logger, extResponse(msg.Text{Text: "clean"}))
	c.Eq(2, len(h.warnedKeys(t)), "a clean block must not warn")

	// A nil response (no turn completed) is not a warning either.
	warnNewExt(logger, nil)
	c.Eq(2, len(h.warnedKeys(t)), "a nil response must not warn")
}

// TestWarnNewExtCap pins the ceiling: past extWarnCap distinct keys the set
// stops recording, so a provider that mangles every response cannot grow an
// unbounded map or an unbounded log.
func TestWarnNewExtCap(t *testing.T) {
	c := assert.NewCollecting(t)
	resetExtWarnings()
	t.Cleanup(resetExtWarnings)

	h := &captureHandler{}
	logger := slog.New(h)

	for i := 0; i < extWarnCap+10; i++ {
		raw := `{"k` + strconv.Itoa(i) + `":1}`
		warnNewExt(logger, extResponse(msg.Text{Text: "x", Ext: msg.Ext{Dialect: msg.DialectAnthropic, Raw: raw}}))
	}

	extWarnMu.Lock()
	size := len(extWarnSeen)
	extWarnMu.Unlock()
	c.Eq(extWarnCap, size, "the dedupe set must stop at the cap")
	c.Eq(extWarnCap, len(h.warnedKeys(t)), "one warning per recorded key, and none past the cap")
}
