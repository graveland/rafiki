// SPDX-License-Identifier: Apache-2.0

package inbox

import (
	"context"
	"errors"
)

// Memory delivers synchronously inside Accept, which reproduces today's
// behaviour exactly: ctrl_send writes straight to the child with no queue.
// Nothing is retained, so a message accepted here is lost if the daemon dies
// before the agent consumes it — the gap the durable implementation closes.
type Memory struct {
	send func(childID string, m Inbound) error
}

// NewMemory builds a Memory that hands each accepted message to send.
func NewMemory(send func(childID string, m Inbound) error) *Memory {
	return &Memory{send: send}
}

// Accept validates, assigns an id, and delivers immediately.
func (m *Memory) Accept(_ context.Context, in Inbound) (string, error) {
	if in.ChildID == "" {
		return "", errors.New("inbox: ChildID is required")
	}
	if m.send == nil {
		return "", errors.New("inbox: no send func configured")
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	in.ID = id
	if err := m.send(in.ChildID, in); err != nil {
		return "", err
	}
	return id, nil
}

// Deliver is a no-op: Memory has already delivered inside Accept, so there is
// never anything pending. The method exists to satisfy Inbox so the durable
// implementation can replace Memory without touching a caller.
func (m *Memory) Deliver(context.Context, string, func(Inbound) error) error { return nil }
