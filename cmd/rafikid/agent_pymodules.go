// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// pymoduleWriter adapts *Controller to tools.PyModuleStore, bound to one
// caller's owner user id at construction -- the same reasoning as
// conversationReader being bound to one Scope.
type pymoduleWriter struct {
	ctrl        *Controller
	ownerUserID string
}

// newControllerPyModuleWriter binds a fundi child's own owner. An empty
// ownerUserID (anonymous spawn) is passed through unchanged: pymodules.Store
// treats an empty owner as one shared "unattributed" bucket, never global.
func newControllerPyModuleWriter(c *Controller, ownerUserID string) *pymoduleWriter {
	return &pymoduleWriter{ctrl: c, ownerUserID: ownerUserID}
}

func (w *pymoduleWriter) Put(ctx context.Context, name, code, description string) (int64, error) {
	rec, err := w.ctrl.pymoduleStore.Put(ctx, w.ownerUserID, name, code, description)
	if err != nil {
		return 0, err
	}
	if w.ctrl.pymodulePusher != nil {
		w.ctrl.pymodulePusher.pushAll(ctx)
	}
	return rec.ID, nil
}

// Delete soft-deletes the module under this writer's bound owner and pushes
// the owner's corpus so executors prune it promptly. Nothing is pushed when
// the delete reports not-found -- the corpus did not change.
func (w *pymoduleWriter) Delete(ctx context.Context, name string) error {
	if err := w.ctrl.pymoduleStore.Delete(ctx, w.ownerUserID, name); err != nil {
		return err
	}
	if w.ctrl.pymodulePusher != nil {
		w.ctrl.pymodulePusher.pushAll(ctx)
	}
	return nil
}

// Get reads the latest live version of a module under this writer's bound
// owner. Read-only: no push, nothing to fan out.
func (w *pymoduleWriter) Get(ctx context.Context, name string) (pymodules.Record, error) {
	return w.ctrl.pymoduleStore.Get(ctx, w.ownerUserID, name)
}

// pymoduleInventory renders ownerUserID's saved modules as "name —
// description" lines, one per line, for the dynamic skill body. Returns a
// clear "nothing saved yet" line rather than an empty string, since an empty
// tool result reads as an error to a model, not as an empty list.
func pymoduleInventory(ctrl *Controller, ownerUserID string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		recs, err := ctrl.pymoduleStore.List(ctx, ownerUserID)
		if err != nil {
			return "", err
		}
		if len(recs) == 0 {
			return "No pymodules saved yet. Use pymodule_put to save one.", nil
		}
		var b strings.Builder
		for _, r := range recs {
			fmt.Fprintf(&b, "%s — %s\n", r.Name, r.Description)
		}
		return b.String(), nil
	}
}
