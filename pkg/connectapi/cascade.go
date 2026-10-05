// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// DescendantLister is the optional slice of the daemon's Controller that
// Kill/Close's include_descendants needs: the ids beneath a child, deepest
// first, so every child is ended before the parent that spawned it.
//
// It is asserted off the lifecycle rather than added to ChildLifecycle, which
// every test double of the lifecycle would otherwise have to grow.
type DescendantLister interface {
	DescendantIDs(childID string) []string
}

// descendantsOf lists childID's descendants, deepest first. An implementation
// that cannot list them is refused rather than silently treated as childless:
// a cascade that quietly ends only the parent is the failure it exists to
// prevent.
func descendantsOf(lc ChildLifecycle, childID string) ([]string, error) {
	dl, ok := lc.(DescendantLister)
	if !ok {
		return nil, connect.NewError(connect.CodeUnimplemented,
			errors.New("include_descendants is not supported by this daemon"))
	}
	return dl.DescendantIDs(childID), nil
}

// cascade runs op over ids in order and returns the ids it ended. An id op
// says is already gone (already exited, or no longer present) counts as done
// but is not reported: nothing was ended by this call. Every other failure is
// collected and the sweep carries on, so one stuck descendant does not strand
// the rest of the subtree; the caller must not end the parent when the
// returned error is non-nil.
func cascade(ids []string, op func(id string) error) (ended []string, _ error) {
	var failed []string
	for _, id := range ids {
		err := op(id)
		if err == nil {
			ended = append(ended, id)
			continue
		}
		var ce *ControllerError
		if errors.As(err, &ce) && (ce.Code == protocol.ErrChildExited ||
			ce.Code == protocol.ErrNotFound || ce.Code == protocol.ErrChildNotFound) {
			continue
		}
		failed = append(failed, fmt.Sprintf("%s: %s", id, err))
	}
	if len(failed) > 0 {
		return ended, &ControllerError{
			Code: protocol.ErrInternal,
			Message: fmt.Sprintf("%d descendant(s) could not be ended, parent left alone: %s",
				len(failed), strings.Join(failed, "; ")),
		}
	}
	return ended, nil
}

// cascadeErr maps a cascade failure onto the wire: an error already carrying a
// Connect code is returned as is (ConnectErr would redact it as internal), an
// authored ControllerError goes through the daemon's code table.
func cascadeErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return ConnectErr(err)
}

// cascadeKill ends childID's live descendants and returns the ids it ended.
func cascadeKill(ctx context.Context, lc ChildLifecycle, childID string, shutdownTimeout, killTimeout time.Duration) ([]string, error) {
	ids, err := descendantsOf(lc, childID)
	if err != nil {
		return nil, err
	}
	return cascade(ids, func(id string) error {
		_, err := lc.Kill(ctx, id, shutdownTimeout, killTimeout)
		return err
	})
}

// cascadeClose closes childID's descendants and returns the ids it closed.
func cascadeClose(ctx context.Context, lc ChildLifecycle, childID string) ([]string, error) {
	ids, err := descendantsOf(lc, childID)
	if err != nil {
		return nil, err
	}
	return cascade(ids, func(id string) error {
		return lc.Close(ctx, id)
	})
}
