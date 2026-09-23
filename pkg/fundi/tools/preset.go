// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"

	"go.graveland.dev/rafiki/pkg/presets"
)

// PresetStore manages the caller's own presets. Bound to one owner (and,
// for an agent, its child id as the write attribution) at construction;
// no method takes an identity.
type PresetStore interface {
	List(ctx context.Context, prefix string) ([]presets.Record, error)
	Get(ctx context.Context, name string) (presets.Record, error)
	History(ctx context.Context, name string) ([]presets.Record, error)
	Put(ctx context.Context, spec presets.Spec) (presets.Record, error)
	Delete(ctx context.Context, name string) error
}

const presetConvention = "Presets are the operator's seat policy: named " +
	"`<group>:<role>` (for example default:implementer, default:reviewer), each " +
	"fixing a model, tools, system prompt and budget. Spawn workers with " +
	"agent_spawn's `preset` instead of choosing models yourself. Only create, " +
	"change or delete a preset when the human asked you to; for a one-off, " +
	"override `model` on agent_spawn instead of editing the seat."
