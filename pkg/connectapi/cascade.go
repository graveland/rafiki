// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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

// ClosableDescendantLister is the optional slice of the daemon's Controller
// that EXITED_DESCENDANTS_ONLY needs: the descendants of a child that are
// already exited AND have no live descendant beneath them, deepest first.
// Closing an exited row above a live one would strand the live child, so those
// rows are left out. Close itself still refuses a row that went live since the
// snapshot; that is a skip, not a failure.
type ClosableDescendantLister interface {
	ClosableDescendantIDs(childID string) []string
}

// scopeSet is what a DescendantScope (or the legacy include_descendants flag)
// asks a Kill or Close to do.
type scopeSet struct {
	target, descendants, exitedOnly bool
}

// resolveScope maps a request's scope and legacy include_descendants flag onto
// what to touch. UNSPECIFIED defers to include; an explicit scope that the
// flag contradicts is refused rather than guessed at.
func resolveScope(include bool, scope rafikiv1.DescendantScope) (scopeSet, error) {
	switch scope {
	case rafikiv1.DescendantScope_DESCENDANT_SCOPE_UNSPECIFIED:
		return scopeSet{target: true, descendants: include}, nil
	case rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY:
		if include {
			return scopeSet{}, errScopeConflict
		}
		return scopeSet{target: true}, nil
	case rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_AND_DESCENDANTS:
		return scopeSet{target: true, descendants: true}, nil
	case rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY:
		return scopeSet{descendants: true}, nil
	case rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY:
		return scopeSet{descendants: true, exitedOnly: true}, nil
	}
	return scopeSet{}, connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("unknown descendant scope %d", int32(scope)))
}

var errScopeConflict = connect.NewError(connect.CodeInvalidArgument,
	errors.New("scope contradicts include_descendants"))

// errSkipped is what a cascade op returns for an id it deliberately left
// alone: counted as done, never reported as ended.
var errSkipped = errors.New("skipped")

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

// cascade runs op over ids in order and returns the ids it ended. See
// cascadeAll for the accounting.
func cascade(ids []string, op func(id string) error) ([]string, error) {
	errs := make([]error, len(ids))
	for i, id := range ids {
		errs[i] = op(id)
	}
	return cascadeAll(ids, errs)
}

// cascadeParallel is cascade with every op running at once. Used where an op
// WAITS (a kill rides out the kill ladder), so a subtree of n children costs
// the slowest one rather than the sum. Ordering between ids is lost, which is
// only acceptable because no id's op depends on another's: the parent is still
// ended after the whole sweep returns.
func cascadeParallel(ids []string, op func(id string) error) ([]string, error) {
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() { errs[i] = op(id) })
	}
	wg.Wait()
	return cascadeAll(ids, errs)
}

// cascadeAll turns per-id results into the sweep's outcome. An id op says is
// already gone (already exited, or no longer present) counts as done but is not
// reported: nothing was ended by this call. Every other failure is collected,
// so one stuck descendant does not strand the rest of the subtree; the caller
// must not end the parent when the returned error is non-nil.
func cascadeAll(ids []string, errs []error) (ended []string, _ error) {
	var failed []string
	for i, id := range ids {
		err := errs[i]
		if err == nil {
			ended = append(ended, id)
			continue
		}
		if errors.Is(err, errSkipped) {
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

// cascadeKill ends childID's live descendants concurrently and returns the ids
// it ended.
func cascadeKill(ctx context.Context, lc ChildLifecycle, childID string, shutdownTimeout, killTimeout time.Duration) ([]string, error) {
	ids, err := descendantsOf(lc, childID)
	if err != nil {
		return nil, err
	}
	return cascadeParallel(ids, func(id string) error {
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

// cascadeCloseExited closes the descendants of childID that are already exited
// and not holding a live descendant, returning the ids it closed. A row that
// went live since the lister ran is skipped.
func cascadeCloseExited(ctx context.Context, lc ChildLifecycle, childID string) ([]string, error) {
	cl, ok := lc.(ClosableDescendantLister)
	if !ok {
		return nil, connect.NewError(connect.CodeUnimplemented,
			errors.New("exited-descendant close is not supported by this daemon"))
	}
	return cascade(cl.ClosableDescendantIDs(childID), func(id string) error {
		err := lc.Close(ctx, id)
		var ce *ControllerError
		if errors.As(err, &ce) && ce.Code == protocol.ErrNotExited {
			return errSkipped
		}
		return err
	})
}
