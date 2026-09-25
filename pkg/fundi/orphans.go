package fundi

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

// RepairOrphans fixes the API-shape invariant an aborted mid-tool-execution
// turn otherwise leaves broken: the Anthropic API rejects a request whose
// assistant message contains a tool_use block with no matching tool_result.
// It scans conv's history for the TRAILING assistant message's tool_use ids
// that have no matching tool_result in the user rows that follow it, and
// appends ONE user message containing a synthetic error result per orphan —
// mirroring agentloop.Resume's own orphan fabrication, but usable
// independently of a Resume (the Engine calls this right after a cancelled
// turn, not at process restart).
//
// It uses only History + AppendUser, so it behaves identically whether conv
// is store-less (in-memory) or DB-backed. Returns the number of synthesized
// results; 0 (with a nil error) when there is nothing to repair — including
// an empty conversation or one whose trailing assistant message already has
// every tool_use resolved.
//
// Orphaned tool_use blocks whose ids carry the prefill_ prefix are NOT
// repaired: they belong to the spawn pre-fill's r1, and completing an
// interrupted pre-fill (r0+r1 persisted, r2 missing) is runPrefill's
// prefillHasR1 job — it re-executes r1's inputs and seeds the real results.
// Fabricating "aborted" results beside them would corrupt the model context
// and diverge the row runPrefill's SeedHistory expects to find. Repair is for
// every other orphan exactly as before.
func RepairOrphans(ctx context.Context, conv *llm.Conversation) (int, error) {
	history, err := conv.History(ctx)
	if err != nil {
		return 0, fmt.Errorf("agent: repair orphans: load history: %w", err)
	}

	lastAssistant := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Param.Role == anthropic.MessageParamRoleAssistant {
			lastAssistant = i
			break
		}
	}
	if lastAssistant == -1 {
		return 0, nil
	}

	resolved := make(map[string]bool)
	for _, m := range history[lastAssistant+1:] {
		if m.Param.Role != anthropic.MessageParamRoleUser {
			continue
		}
		for _, id := range m.ToolUseIDs {
			resolved[id] = true
		}
	}

	var blocks []anthropic.ContentBlockParamUnion
	for _, block := range history[lastAssistant].Param.Content {
		tu := block.OfToolUse
		if tu == nil || resolved[tu.ID] {
			continue
		}
		// A pre-fill's synthetic tool_use ids are never orphans to repair —
		// see this function's doc comment.
		if strings.HasPrefix(tu.ID, prefillIDPrefix) {
			continue
		}
		blocks = append(blocks, anthropic.NewToolResultBlock(tu.ID,
			"Tool execution aborted by user.", true))
	}
	if len(blocks) == 0 {
		return 0, nil
	}

	if err := conv.AppendUser(ctx, blocks); err != nil {
		return 0, fmt.Errorf("agent: repair orphans: append synthetic results: %w", err)
	}
	return len(blocks), nil
}
