// SPDX-License-Identifier: Apache-2.0

// Package inbox is the seam between "a client submitted something for an
// agent" and "the agent received it".
//
// It exists so the durable, Postgres-backed queue described in
// docs/plans/2026-08-23-rafiki-tui-phase-b-design.md §5 can land later without
// reshaping any caller. The shape that matters is accept -> (persist) ->
// deliver -> ack: a fire-and-forget Send would have no consume point to make
// durable, which is exactly the reshaping this package prevents.
//
// This package must never import pgx or pkg/store. It is the interface a
// database implementation satisfies, not the implementation.
package inbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"
)

// Mode is how an inbound message reaches a running agent.
type Mode int

const (
	// ModePrompt queues work; the agent picks it up when it is next free.
	ModePrompt Mode = iota
	// ModeSteer injects into the turn already running, when one is.
	ModeSteer
	// ModeAbort cancels the running turn.
	ModeAbort
)

func (m Mode) String() string {
	switch m {
	case ModePrompt:
		return "prompt"
	case ModeSteer:
		return "steer"
	case ModeAbort:
		return "abort"
	default:
		return "unknown"
	}
}

// ParseMode is String's inverse. An unrecognised spelling is ModePrompt: the
// column is CHECK-constrained, so this can only be reached by a hand-edited
// row, and queueing work is a safer failure than injecting a steer into an
// unrelated turn.
func ParseMode(s string) Mode {
	switch s {
	case "steer":
		return ModeSteer
	case "abort":
		return ModeAbort
	default:
		return ModePrompt
	}
}

// State is where a message sits in its lifecycle.
//
// The distinction that matters is pending vs sent. A pending row has not been
// written to a child; a sent row has, and is waiting for the child to confirm
// it entered a turn. Only that separation stops the idle-drain retry path from
// delivering a second copy of something already queued inside the child.
type State string

const (
	StatePending  State = "pending"
	StateSent     State = "sent"
	StateConsumed State = "consumed"
	StateDropped  State = "dropped"
)

// Inbound is one submitted message. ID and AcceptedAt are assigned by the store.
//
// Source and Key exist because eventbuf's fragments are inbox rows too:
// Source is the per-concern grouping ("subagents", "executor"), Key is the
// last-write-wins key within that group. A direct message from a human or a
// coordinator has both empty, and is never coalesced with anything.
type Inbound struct {
	ID         string
	ChildID    string
	Mode       Mode
	Source     string
	Key        string
	Text       string
	AcceptedAt time.Time
}

// Accepter is the narrow seam a submitter needs: validate, persist, return an
// id the caller may quote back. pkg/connectapi holds one of these and nothing
// more, so the Connect face cannot reach delivery or acking.
type Accepter interface {
	Accept(ctx context.Context, in Inbound) (string, error)
}

// Store is the durable backing for an inbox.
//
// Every mutating method is scoped to ONE child. That is not a convenience:
// child rows are shared across daemons, so an unscoped state transition
// reaches into another live daemon's in-flight messages. There is no
// cross-child variant of any of these, and adding one is a bug.
type Store interface {
	// Accept persists in as StatePending, assigning ID and AcceptedAt, and
	// returns the stored row.
	Accept(ctx context.Context, in Inbound) (Inbound, error)

	// Pending returns childID's StatePending rows, oldest first, ties broken
	// by ID so the order is total and reproducible.
	Pending(ctx context.Context, childID string) ([]Inbound, error)

	// MarkSent moves rows to StateSent. Called after a successful write to a
	// child that will confirm consumption.
	MarkSent(ctx context.Context, ids []string) error

	// MarkConsumed moves rows to StateConsumed — terminal.
	MarkConsumed(ctx context.Context, ids []string) error

	// ResetSent moves childID's StateSent rows back to StatePending and
	// returns how many moved. Called when the child that held them died.
	ResetSent(ctx context.Context, childID string) (int, error)

	// Drop terminates every non-terminal row for childID with a reason, and
	// returns how many were dropped.
	Drop(ctx context.Context, childID, reason string) (int, error)

	// Sweep deletes terminal rows accepted before the cutoff, across all
	// children, and returns how many were deleted. Deleting a terminal row
	// cannot affect any daemon's delivery decisions, which is why this one is
	// allowed to be unscoped.
	Sweep(ctx context.Context, before time.Time) (int, error)
}

// NewID returns a fresh 128-bit identifier. Used for row ids by the stores and
// for frame ids by the daemon, which is deliberate: both are opaque handles
// with the same uniqueness requirement and no ordering meaning.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("inbox: generate id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
