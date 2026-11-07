// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
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

// switchClaudeModel relaunches a claude child on another Anthropic model: the
// stored model is rewritten, then the process is relaunched from that
// snapshot with --resume, so the conversation continues under the same child
// id and session. An in-flight turn is lost and the prompt cache starts cold.
//
// A child hosted by an executor's daraja is relaunched through Kill and Resume
// rather than Pool.Restart: the host builds its argv from launch-wide options
// (the proxied --model, the registered custom model option) that a Restart
// spec cannot change, so a restart there comes back on the OLD model.
//
// Anything a relaunch cannot carry is refused with the fork's name rather than
// attempted: the launch environment of a local child is rebuilt but a hosted
// one's is not, so both the current and the requested model must be concrete
// Anthropic ids (proxyenv.RestartModel).
//
// The command is the operator's. A child credential reaches Send for its own
// subtree, and nothing there should let an agent move a conversation to a model
// of its choosing; the cockpit is the only sender. It always relaunches, even
// onto the model already stored, so it also converges a child whose process
// and stored model have drifted apart.
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
	if ch, ok := c.cm.Get(snap.ChildID); ok && liveOrRecordedSessionID(ch, snap) == "" {
		return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs,
			Message: "/model: this child has no session yet (it has not taken a turn), so there is nothing to carry over; spawn it on the model you want"}
	}

	previous := joinModel(snap.Provider, snap.Model)
	c.handleModelChange(snap.ChildID, target)
	var err error
	if snap.Labels["rafiki/executor"] != "" {
		err = c.relaunchClaude(ctx, snap.ChildID)
	} else {
		err = c.handleClaudeAbort(snap.ChildID)
	}
	if err != nil {
		if cur, ok := c.st.Get(snap.ChildID); ok && cur.Status != protocol.StatusExited {
			// Still running its old process: keep the stored model the one it
			// runs on. An exited child keeps the requested one, which is what a
			// later resume should start on.
			c.handleModelChange(snap.ChildID, previous)
		}
		return err
	}
	return nil
}

// relaunchClaude stops a daraja-hosted claude child and resumes it from its
// snapshot, so the replacement is launched with argv and environment built
// from the stored model. The session id is recorded first: Resume reads it
// from the store.
func (c *Controller) relaunchClaude(ctx context.Context, childID string) error {
	ch, ok := c.cm.Get(childID)
	if !ok {
		return &connectapi.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return &connectapi.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	sessionID := liveOrRecordedSessionID(ch, snap)
	if sessionID == "" {
		return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: "cannot relaunch claude child before its session is established"}
	}
	if snap.SessionID != sessionID {
		if err := c.st.Update(childID, func(s *childstore.Session) { s.SessionID = sessionID }); err != nil {
			return err
		}
	}
	if _, err := c.Kill(ctx, childID, abortShutdownGrace, abortKillGrace); err != nil {
		var ce *connectapi.ControllerError
		if !errors.As(err, &ce) || (ce.Code != protocol.ErrChildExited && ce.Code != protocol.ErrChildShuttingDown) {
			return fmt.Errorf("claude relaunch stop: %w", err)
		}
	}
	// Resume adds the child to the manager, which can race the exit handler's
	// removal of the old one: wait for both the exited status and the removal.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, ok := c.st.Get(childID)
		_, live := c.cm.Get(childID)
		if ok && cur.Status == protocol.StatusExited && !live {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.Resume(ctx, childID, ""); err != nil {
		return fmt.Errorf("claude relaunch resume (the child is stopped; resume it to continue on the new model): %w", err)
	}
	return nil
}
