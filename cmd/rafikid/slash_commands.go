// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"
	"go.graveland.dev/rafiki/pkg/server"
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
	cmd, args, ok := slashcmd.Parse(in.Text)
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
	case slashcmd.Model:
		return true, c.switchClaudeModel(ctx, snap, args)
	case slashcmd.Compact:
		// claude passthrough, or the fundi engine's own manual compaction:
		// either way the prompt falls through to the normal path with no side
		// effects here.
		return false, nil
	}
	return false, nil
}

// switchClaudeModel restarts a claude child on another Anthropic model: the
// snapshot's model is rewritten, then the abort cycle (interrupt, restart with
// --resume) relaunches it from that snapshot, so the conversation continues
// under the same child id and session. An in-flight turn is lost, as with an
// abort, and the prompt cache starts cold.
//
// Anything a restart cannot carry is refused with the fork's name rather than
// attempted: the launch environment is fixed (proxyenv.RestartModel), so both
// the current and the requested model must be concrete Anthropic ids.
//
// The command is the operator's. A child credential reaches Send for its own
// subtree, and nothing there should let an agent move a conversation to a model
// of its choosing; the cockpit is the only sender.
func (c *Controller) switchClaudeModel(ctx context.Context, snap childstore.Snapshot, arg string) error {
	if id := server.IdentityFromContext(ctx); id != nil && !id.IsUserCredential() {
		return &connectapi.ControllerError{Code: protocol.ErrPermissionDenied, Message: "/model is an operator command"}
	}
	const fork = "; use `rafiki models switch` to move it with a fork"
	target, ok := proxyenv.RestartModel(arg)
	if !ok {
		return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("/model: %q is not an Anthropic model id a running claude child can restart onto%s", arg, fork)}
	}
	if !proxyenv.AnthropicModel(snap.Model) {
		return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("/model: this child runs %q, which was registered at launch and cannot be changed in place%s", snap.Model, fork)}
	}
	if target == snap.Model {
		return nil
	}
	if err := c.st.Update(snap.ChildID, func(s *childstore.Session) { s.Model = target }); err != nil {
		return err
	}
	if err := c.handleClaudeAbort(snap.ChildID); err != nil {
		if rerr := c.st.Update(snap.ChildID, func(s *childstore.Session) { s.Model = snap.Model }); rerr != nil {
			slog.Error("/model: restoring the previous model after a failed restart", "child", snap.ChildID, "error", rerr)
		}
		return err
	}
	return nil
}
