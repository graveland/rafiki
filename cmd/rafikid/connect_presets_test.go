// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/presets"

	"github.com/multigres/testkit/assert"
)

// TestPresetWiringConnectPutMapsInvalid pins the Connect-plane contract the
// plan's task 2.1 review called out: a spec that fails the controller's own
// validation — here an unknown tool name, caught by putPreset before any
// store call — comes back wrapping connectapi.ErrInvalidPreset, which
// presetError maps to CodeInvalidArgument rather than CodeInternal. The store
// behind the controller is presets_test.go's in-memory fake; validation
// refusing the spec first is the point.
func TestPresetWiringConnectPutMapsInvalid(t *testing.T) {
	ck := assert.NewAborting(t)
	ctl := &Controller{presetStore: newFakePresetStore("")}
	m := connectPresets{c: ctl}
	tools := []string{"not-a-real-tool"}
	_, err := m.PutPreset(context.Background(), presets.Spec{
		Name:  "review",
		Kind:  presets.KindFundi,
		Tools: &tools,
	})
	ck.Error(err, "PutPreset naming an unknown tool: got nil error")
	ck.ErrorIs(err, connectapi.ErrInvalidPreset, "PutPreset error does not wrap connectapi.ErrInvalidPreset")
}
