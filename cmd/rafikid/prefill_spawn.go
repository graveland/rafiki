package main

import (
	"strings"

	"go.graveland.dev/rafiki/pkg/control"
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
// checked here: a non-fundi kind has no engine that runs a pre-fill at all.
func validatePrefill(req protocol.SpawnRequest) error {
	// prefill.Validate errors with "prefill: no entries" on an empty slice,
	// which is the normal no-prefill case, not a refusal.
	if len(req.Prefill) == 0 {
		return nil
	}

	if req.Kind != "" && req.Kind != protocol.KindFundi {
		return &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "prefill: only kind fundi supports a pre-fill",
		}
	}

	if err := prefill.Validate(req.Prefill); err != nil {
		msg := err.Error()
		// No double prefix: Validate's errors already start with "prefill: ".
		if !strings.HasPrefix(msg, "prefill: ") {
			msg = "prefill: " + msg
		}
		return &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: msg,
		}
	}

	return nil
}
