// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/notify"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// Notify queues a third-party notification onto one of the caller owner's live
// top-level agents (connectapi.Notifier). The gateway answers its sender only
// after this returns nil, so the fragment is DURABLE before the webhook is
// acknowledged: PushDurable persists it or reports the failure, and a failed
// accept leaves nothing queued for a later delivery.
//
// The CALLER'S owner is the whole addressing rule, and it comes from the
// connection identity (the Connect handler), never from a request field. An
// anonymous caller — the unix socket's local trust — is addressed at the
// ownerless rows (owner_user_id IS NULL), which is what the empty owner string
// means to NameHolder.
//
// A killed target still receives: Kill exits a child and leaves its row live,
// so its name still resolves and the row stays pending for a resume. Only
// CLOSING a child frees its name (child_sibling_name_live).
func (c *Controller) Notify(ctx context.Context, ownerUserID string, anonymous bool, target, source, key string, fields map[string]string) (string, error) {
	// Render validates first: source, key and the field charsets and caps are
	// pkg/notify's rules, and it refuses rather than truncating, so a caller
	// that gets an error gets no fragment at all.
	fragment, err := notify.Render(source, key, fields, time.Now())
	if err != nil {
		var invalid *notify.InvalidError
		if !errors.As(err, &invalid) {
			return "", err
		}
		return "", &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: invalid.Msg}
	}

	// No durable child store means no name to resolve; no event buffer means no
	// durable inbox to write, and answering success there would promise a
	// sender a delivery nothing holds.
	if c.children == nil || c.evbuf == nil {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrUnavailable,
			Message: "notify requires the durable child store",
		}
	}

	// An authenticated caller that resolved to an EMPTY owner is refused
	// before the store is touched. The Connect handler derives (owner, anonymous)
	// from identity alone — a nil identity is the unix socket's local trust and
	// is legitimately ownerless, but a non-nil identity with no user id is not:
	// resolving it as "" would address the ownerless namespace (and so, in
	// practice, somebody else's agents). InvalidArgument rather than
	// Unavailable on purpose: a retryable code would have the gateway re-post the
	// same event forever against a defect no retry can fix.
	if !anonymous && ownerUserID == "" {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "notify requires an owner: the caller's identity carries no user id",
		}
	}

	owner := ownerUserID
	if anonymous {
		owner = ""
	}
	childID, _, ok, err := c.children.NameHolder(ctx, owner, "", target)
	if err != nil {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrUnavailable,
			Message: fmt.Sprintf("resolve target: %v", err),
		}
	}
	if !ok {
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: fmt.Sprintf("no live top-level agent named %q", target),
		}
	}

	if err := c.evbuf.PushDurable(ctx, childID, notifySourcePrefix+source, key, fragment); err != nil {
		// An over-cap fragment is the sender's own doing and no retry can fix
		// it: Repeatable-as-is would have the gateway re-post the same event
		// forever. InvalidArgument names the number the sender must get under,
		// which the buffer refuses to do by truncation (a cut frame reaches the
		// agent without its closing tag).
		if errors.Is(err, eventbuf.ErrFragmentTooLarge) {
			return "", &connectapi.ControllerError{
				Code: protocol.ErrInvalidArgs,
				Message: fmt.Sprintf(
					"notification is %d bytes, over the event buffer's %d-byte per-fragment limit; send fewer or smaller fields",
					len(fragment), c.evbuf.MaxBytesPerFrag()),
			}
		}
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrUnavailable,
			Message: fmt.Sprintf("notification not stored: %v", err),
		}
	}
	return childID, nil
}

// The Controller is what SetNotifier is handed at startup (main.go); this
// compile-time assertion keeps the method set from drifting away from the
// connectapi seam.
var _ connectapi.Notifier = (*Controller)(nil)

// notifySourcePrefix is the eventbuf source every notification is queued under:
// "notify:" + the caller's source, so one webhook source's burst coalesces as
// its own group.
const notifySourcePrefix = "notify:"

// notifyAccepter is the event buffer's durable accepter. For every other
// fragment it is the inbox queue unchanged; a NOTIFICATION is written straight
// to the same store instead.
//
// The queue's ValidateFn asks a command-plane question — can this child still
// be sent to — and refuses an exited child. A notification has no such client:
// its sender retries, and a KILLED target keeps its name and its pending rows so
// the notification is delivered when the child resumes (the design's resolved
// question). Asking the command-plane question here would turn "the target
// happens not to be running" into a lost notification the gateway is told to
// retry forever.
type notifyAccepter struct{ c *Controller }

func (a notifyAccepter) Accept(ctx context.Context, in inbox.Inbound) (string, error) {
	if !strings.HasPrefix(in.Source, notifySourcePrefix) {
		if a.c.inbox == nil {
			return "", errors.New("inbox: no queue configured")
		}
		return a.c.inbox.Accept(ctx, in)
	}
	if a.c.inboxStore == nil {
		return "", errors.New("inbox: no store configured")
	}
	rec, err := a.c.inboxStore.Accept(ctx, in)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}
