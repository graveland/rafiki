// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/slashcmd"
)

// clearMarker is the capture seam a /clear needs: flag the child's main
// conversation so capture records the next divergent head as a clear boundary.
type clearMarker interface {
	MarkClearPending(ctx context.Context, externalRef string) error
}

// handleSlashCommand classifies in. handled is true when the command was
// fully executed here (nothing is queued; the returned id is empty). When
// handled is false and err is nil the caller continues with the normal
// persist-and-deliver path; the command's side effects are already done.
func (c *Controller) handleSlashCommand(ctx context.Context, in inbox.Inbound) (handled bool, err error) {
	if in.Mode != inbox.ModePrompt {
		return false, nil
	}
	cmd, _, ok := slashcmd.Parse(in.Text)
	if !ok {
		return false, nil
	}
	snap, found := c.st.Get(in.ChildID)
	if !found {
		return false, &connectapi.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + in.ChildID}
	}
	if !slashcmd.Supports(snap.Kind, cmd) {
		return false, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: "/" + string(cmd) + " is not supported for " + snap.Kind + " children"}
	}
	switch cmd {
	case slashcmd.Exit:
		_, err := c.Kill(ctx, in.ChildID, 5*time.Second, 5*time.Second)
		return true, err
	case slashcmd.Clear:
		if snap.Kind != protocol.KindClaude {
			// A fundi engine owns its history and moves its own horizon when the
			// prompt reaches it (pkg/fundi Engine.runSlash); there is no session
			// id to adopt and no capture boundary to flag.
			return false, nil
		}
		// Mark first, arm second: a failed mark must leave nothing armed, or
		// the next unrelated session id change would be adopted as a clear.
		if c.clearMarker != nil {
			if err := c.clearMarker.MarkClearPending(ctx, in.ChildID); err != nil {
				return false, err
			}
		}
		c.clearExpected.Store(in.ChildID, struct{}{})
		return false, nil
	case slashcmd.Compact:
		// claude passthrough, or the fundi engine's own manual compaction:
		// either way the prompt falls through to the normal path with no side
		// effects here.
		return false, nil
	}
	return false, nil
}
