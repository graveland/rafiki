// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"go.graveland.dev/rafiki/pkg/msg"
)

// extWarnCap bounds the dedupe set. Past it the set stops recording and the
// warning stops: an operator needs to know an unmodelled key exists, not one
// log line per response for the life of the process.
const extWarnCap = 256

var (
	extWarnMu   sync.Mutex
	extWarnSeen = make(map[string]struct{})
)

// warnNewExt logs, once per process per key, every response block carrying keys
// the native core does not model (Ext.Raw). A provider quietly growing a field
// is otherwise invisible: the block still decodes, and the key is silently
// dropped on the way back out.
//
// The key is "<dialect>|<block type>|<sorted comma-joined key names>", so a new
// field on one block kind in one dialect is one line, and the same field
// appearing on another kind or dialect is another. Nested tool_result content is
// walked too — a result block's inner blocks are as provider-shaped as any
// other.
//
// Called once per successful response on the send paths, never on a history
// read: stored rows have already been warned about when they were received.
func warnNewExt(logger *slog.Logger, r *msg.Response) {
	if r == nil || logger == nil {
		return
	}
	warnBlockExt(logger, r.Content)
}

// warnBlockExt walks a block list, warning for each block's own residue and
// descending into a tool result's content.
func warnBlockExt(logger *slog.Logger, blocks []msg.Block) {
	for _, b := range blocks {
		ext := blockExtOf(b)
		if ext.Raw != "" {
			warnExtOnce(logger, ext, blockTypeName(b))
		}
		if tr, ok := b.(msg.ToolResult); ok {
			warnBlockExt(logger, tr.Content)
		}
	}
}

// warnExtOnce logs one (dialect, block type, key set) triple at most once.
func warnExtOnce(logger *slog.Logger, ext msg.Ext, typ string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ext.Raw), &fields); err != nil || len(fields) == 0 {
		return
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	joined := strings.Join(keys, ",")
	dedupe := string(ext.Dialect) + "|" + typ + "|" + joined

	extWarnMu.Lock()
	if _, seen := extWarnSeen[dedupe]; seen {
		extWarnMu.Unlock()
		return
	}
	if len(extWarnSeen) >= extWarnCap {
		extWarnMu.Unlock()
		return
	}
	extWarnSeen[dedupe] = struct{}{}
	extWarnMu.Unlock()

	logger.Warn("msg ext: provider block carries unmodelled keys",
		"dialect", string(ext.Dialect), "block", typ, "keys", joined)
}

// blockExtOf returns a block's unmodelled-key residue.
func blockExtOf(b msg.Block) msg.Ext {
	switch t := b.(type) {
	case msg.Text:
		return t.Ext
	case msg.Thinking:
		return t.Ext
	case msg.ToolUse:
		return t.Ext
	case msg.ToolResult:
		return t.Ext
	case msg.Image:
		return t.Ext
	}
	return msg.Ext{}
}

// blockTypeName is the wire type of a block, for the warning's key.
func blockTypeName(b msg.Block) string {
	switch t := b.(type) {
	case msg.Text:
		return "text"
	case msg.Thinking:
		return "thinking"
	case msg.ToolUse:
		return "tool_use"
	case msg.ToolResult:
		return "tool_result"
	case msg.Image:
		return "image"
	case msg.Opaque:
		if typ := msg.OpaqueType(t); typ != "" {
			return typ
		}
		return "opaque"
	}
	return "unknown"
}
