// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/server"

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

// TestConnectPresetsAuthoringIsTopLevelOnly pins the Connect twin of
// presetStoreForChild: PutPreset/DeletePreset are childScoped at the gate, and
// the adapter admits a per-child secret only for a top-level child — stamped
// as the writer — refusing a parented or unknown one with ErrPresetAuthoring.
func TestConnectPresetsAuthoringIsTopLevelOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	m := connectPresets{c: &Controller{st: lineageStore(), presetStore: newFakePresetStore("u-owner")}}
	as := func(childID string) context.Context {
		return server.WithIdentity(context.Background(),
			&server.Identity{UserID: "u-owner", ChildID: childID, Via: server.ProvenanceChildToken})
	}

	rec, err := m.PutPreset(as("c_root"), presets.Spec{Name: "p"})
	ck.NoError(err, "top-level PutPreset")
	ck.Eq("c_root", rec.WrittenByChild, "WrittenByChild")
	ck.NoError(m.DeletePreset(as("c_root"), "p"), "top-level DeletePreset")

	for _, id := range []string{"c_kid", "c_gone"} {
		_, err := m.PutPreset(as(id), presets.Spec{Name: "p"})
		ck.ErrorIs(err, connectapi.ErrPresetAuthoring, "PutPreset as %s", id)
		ck.ErrorIs(m.DeletePreset(as(id), "p"), connectapi.ErrPresetAuthoring, "DeletePreset as %s", id)
	}

	// A user credential is the operator: authored, attributed to nobody.
	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u-owner", Via: server.ProvenanceUser})
	rec, err = m.PutPreset(user, presets.Spec{Name: "op"})
	ck.NoError(err, "user PutPreset")
	ck.Eq("", rec.WrittenByChild, "WrittenByChild")
}
