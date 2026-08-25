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

import "context"

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

// Inbound is one submitted message. ID is assigned by Accept.
type Inbound struct {
	ID      string
	ChildID string
	Mode    Mode
	Text    string
}

// Inbox accepts messages for agents and delivers them.
//
// Accept records the message and returns its id. Deliver hands the next
// deliverable message for a child to fn; fn returning nil acks it, and an
// error leaves it for retry. An implementation that delivers synchronously
// inside Accept (see Memory) makes Deliver a no-op.
type Inbox interface {
	Accept(ctx context.Context, m Inbound) (string, error)
	Deliver(ctx context.Context, childID string, fn func(Inbound) error) error
}
