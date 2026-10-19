package main

import (
	"strings"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/prefill"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// validatePrefill refuses a pre-fill the child could not run. Called in
// Controller.Spawn right after applyPreset, so the preset's kind shaping is
// already resolved.
//
// The entries themselves are checked by prefill.Validate (shape: non-empty
// paths, sane 1-based ranges, no range on a glob, entry count). The child's
// TOOL SET is deliberately not checked here: the reads run through the
// engine's internal read/glob reader (materialized from the same executor
// routing as the child's tools, never offered to the model), so a tool-less
// child carries a pre-fill as one text row — the engine refuses a spawn
// whose executor cannot serve reads, at worker start. Only the kind is
// checked here: only fundi (an engine that runs the reads) and claude (told to
// read them, see foldClaudePrefill) can carry a pre-fill.
func validatePrefill(req protocol.SpawnRequest) error {
	// prefill.Validate errors with "prefill: no entries" on an empty slice,
	// which is the normal no-prefill case, not a refusal.
	if len(req.Prefill) == 0 {
		return nil
	}

	if req.Kind != "" && req.Kind != protocol.KindFundi && req.Kind != protocol.KindClaude {
		return &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "prefill: only kinds fundi and claude support a pre-fill",
		}
	}

	if err := prefill.Validate(req.Prefill); err != nil {
		msg := err.Error()
		// No double prefix: Validate's errors already start with "prefill: ".
		if !strings.HasPrefix(msg, "prefill: ") {
			msg = "prefill: " + msg
		}
		return &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: msg,
		}
	}

	return nil
}

// foldClaudePrefill turns a claude child's pre-fill into appended
// system-prompt text: the Claude Code binary cannot be handed synthetic tool
// results, so the child is instead told to read the listed files first. Run
// once in Controller.Spawn; the result rides AppendSystemPrompt, so the
// snapshot and every restart path carry it without re-rendering.
func foldClaudePrefill(req protocol.SpawnRequest) protocol.SpawnRequest {
	if req.Kind != protocol.KindClaude || len(req.Prefill) == 0 {
		return req
	}
	req.AppendSystemPrompt = joinNonEmpty(req.AppendSystemPrompt, prefill.Instructions(req.Prefill))
	return req
}
