// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/server"
)

// Notifier delivers a notification to an owner's top-level agent by name.
// ownerUserID "" with anonymous=true is the unix-socket caller (owner_user_id
// IS NULL). Errors are *ControllerError where they are answers.
type Notifier interface {
	Notify(ctx context.Context, ownerUserID string, anonymous bool, target, source, key string, fields map[string]string) (childID string, err error)
}

// SetNotifier attaches the notification backend. Post-construction setter for
// the same reason as SetSkillManager: the Controller is built after this
// Server. A nil notifier is refused rather than stored — storing &n for a nil
// interface would defeat the Unavailable path in Notify and nil-panic the
// first call instead.
func (s *Server) SetNotifier(n Notifier) {
	if n == nil {
		return
	}
	s.notifier.Store(&n)
}

// Notify queues a notification onto one of the caller owner's live top-level
// agents. See NotifyRequest's comment in control.proto.
//
// It validates nothing beyond a present target: source, key and fields are the
// Controller's rules (pkg/notify), which refuses a violation with
// ErrInvalidArgs after trimming. The owner comes from the connection —
// identity, not a request field — and a nil identity is the unix socket's
// local trust, addressed at the ownerless (owner_user_id IS NULL) children.
//
// Errors go through ConnectErr, as in Rename.
func (s *Server) Notify(
	ctx context.Context,
	req *connect.Request[rafikiv1.NotifyRequest],
) (*connect.Response[rafikiv1.NotifyResponse], error) {
	target := strings.TrimSpace(req.Msg.GetTarget())
	if target == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("target is required"))
	}
	p := s.notifier.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("notifier not yet wired"))
	}
	ownerUserID, anonymous := "", true
	if id := server.IdentityFromContext(ctx); id != nil {
		ownerUserID, anonymous = id.UserID, false
	}
	childID, err := (*p).Notify(ctx, ownerUserID, anonymous, target,
		req.Msg.GetSource(), req.Msg.GetKey(), req.Msg.GetFields())
	if err != nil {
		var ce *ControllerError
		if !errors.As(err, &ce) {
			slog.Error("connect: notify failed", "target", target,
				"source", req.Msg.GetSource(), "error", err)
		}
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.NotifyResponse{ChildId: childID}), nil
}
