// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/presets"
)

// presetBinding satisfies the fundi tools' PresetStore interface (the daemon
// hands presetBinding to every in-process child). The assertion lives here
// rather than in presets.go because presets.go is outside this change's
// declared surface.
var _ tools.PresetStore = presetBinding{}

// connectPresets adapts *Controller to connectapi.PresetManager, modelled on
// connectPyModules: the owner comes from the request CONTEXT via spawnOwner —
// never a request field. No credential gate is applied, deliberately, for the
// same reason connectPyModules gives: an anonymous unix-socket caller writes
// the shared unattributed bucket, which is the same owner its own children
// resolve to, and a child-attributed caller is a legitimate preset writer
// through its owner.
//
// The manager never mutates a Spec handed to it: presets.ToProto aliases the
// record's value-shaped data (Labels and the tri-state slices) into the proto
// response, so a write into spec.Labels or a spec slice here could surface in
// a later conversion of the same values. putPreset's Spec.Record copies.
type connectPresets struct{ c *Controller }

// ListPresets lists the caller's latest live presets whose names start with
// prefix ("" = all). presets.ErrNotFound cannot arise on a list, so every
// error surfaces unwrapped for presetError to map to CodeInternal.
func (m connectPresets) ListPresets(ctx context.Context, prefix string) ([]presets.Record, error) {
	return m.c.presetStore.List(ctx, spawnOwner(ctx).UserID, prefix)
}

// GetPreset returns the latest live row as a one-element slice, or with
// history every row newest first (deleted ones included). The errors surface
// unwrapped: presets.ErrNotFound maps to CodeNotFound in presetError.
func (m connectPresets) GetPreset(ctx context.Context, name string, history bool) ([]presets.Record, error) {
	owner := spawnOwner(ctx).UserID
	if history {
		return m.c.presetStore.History(ctx, owner, name)
	}
	rec, err := m.c.presetStore.Get(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	return []presets.Record{rec}, nil
}

// PutPreset validates and stores spec as a new version, attributed to the
// operator (childID ""). A validation failure wraps connectapi.ErrInvalidPreset
// — presetError maps that to CodeInvalidArgument rather than CodeInternal, so
// a bad preset reads as the caller's fault — and every other failure surfaces
// as the store error it is.
func (m connectPresets) PutPreset(ctx context.Context, spec presets.Spec) (presets.Record, error) {
	rec, err := m.c.putPreset(ctx, spawnOwner(ctx).UserID, "", spec)
	if err != nil {
		if errors.Is(err, errPresetInvalid) {
			return presets.Record{}, fmt.Errorf("%w: %v", connectapi.ErrInvalidPreset, err)
		}
		return presets.Record{}, err
	}
	return rec, nil
}

// DeletePreset stamps deleted_at on every live row for name.
func (m connectPresets) DeletePreset(ctx context.Context, name string) error {
	return m.c.presetStore.Delete(ctx, spawnOwner(ctx).UserID, name)
}
