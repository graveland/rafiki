package main

import (
	"strings"

	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/prefill"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// validatePrefill refuses a pre-fill the child could not run. Called in
// Controller.Spawn right after applyPreset, so the preset's kind and tool
// shaping are already resolved.
//
// The entries themselves are checked by prefill.Validate (shape: non-empty
// paths, sane 1-based ranges, no range on a glob, entry count). The remaining
// checks are about the CHILD the pre-fill is headed for: a pre-fill is history
// the engine fabricates from the child's own Read (and, for globs, Glob) tool
// results, so a child whose tool set excludes those tools could never produce
// it — and a non-fundi kind has no engine that runs a pre-fill at all.
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

	if req.NoBuiltinTools {
		return &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "prefill: the child needs the read tool, but this spawn disables every built-in tool",
		}
	}

	// An empty req.Tools means "all built-in tools", so read and glob are both
	// available; only a non-empty allowlist can omit them.
	hasGlob := false
	for _, e := range req.Prefill {
		if prefill.IsGlob(e.Path) {
			hasGlob = true
			break
		}
	}
	if req.Tools != "" {
		tools := map[string]bool{}
		for _, t := range strings.Split(req.Tools, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tools[t] = true
			}
		}
		if !tools["read"] {
			return &control.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: "prefill: the child needs the read tool, but its tool allowlist omits it",
			}
		}
		if hasGlob && !tools["glob"] {
			return &control.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: "prefill: a glob entry needs the glob tool, but the tool allowlist omits it",
			}
		}
	}

	return nil
}
