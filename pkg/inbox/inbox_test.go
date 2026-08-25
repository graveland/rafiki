// SPDX-License-Identifier: Apache-2.0

package inbox_test

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/inbox"
)

func TestMemoryAcceptDeliversSynchronously(t *testing.T) {
	var got []inbox.Inbound
	m := inbox.NewMemory(func(childID string, in inbox.Inbound) error {
		got = append(got, in)
		return nil
	})

	id, err := m.Accept(context.Background(), inbox.Inbound{
		ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hello",
	})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if id == "" {
		t.Error("Accept returned an empty id; it must return a non-empty identifier")
	}
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(got))
	}
	if got[0].Text != "hello" || got[0].Mode != inbox.ModePrompt {
		t.Errorf("delivered %+v, want text=hello mode=prompt", got[0])
	}
}

func TestMemoryAcceptPropagatesSendError(t *testing.T) {
	sentinel := errors.New("child not found")
	m := inbox.NewMemory(func(string, inbox.Inbound) error { return sentinel })

	_, err := m.Accept(context.Background(), inbox.Inbound{ChildID: "c_x", Mode: inbox.ModePrompt})
	if !errors.Is(err, sentinel) {
		t.Errorf("Accept error = %v, want %v", err, sentinel)
	}
}

func TestMemoryAcceptRejectsEmptyChildID(t *testing.T) {
	m := inbox.NewMemory(func(string, inbox.Inbound) error { return nil })
	if _, err := m.Accept(context.Background(), inbox.Inbound{Mode: inbox.ModePrompt}); err == nil {
		t.Error("Accept with an empty ChildID returned nil error, want an error")
	}
}

func TestMemoryAcceptAssignsUniqueIDs(t *testing.T) {
	m := inbox.NewMemory(func(string, inbox.Inbound) error { return nil })
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := m.Accept(context.Background(), inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt})
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestMemoryDeliverIsANoOp(t *testing.T) {
	m := inbox.NewMemory(func(string, inbox.Inbound) error { return nil })
	called := false
	err := m.Deliver(context.Background(), "c_1", func(inbox.Inbound) error {
		called = true
		return nil
	})
	if err != nil {
		t.Errorf("Deliver: %v", err)
	}
	if called {
		t.Error("Deliver invoked fn; the memory implementation delivers inside Accept and Deliver must be a no-op")
	}
}

// Compile-time proof the memory type satisfies the interface.
var _ inbox.Inbox = (*inbox.Memory)(nil)
