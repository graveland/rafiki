// SPDX-License-Identifier: Apache-2.0

// Package inboxtest provides the conformance suite every inbox.Store
// implementation must pass unchanged.
//
// The caveat that applies to pkg/tasks and pkg/eventlog applies here verbatim:
// the memory store is atomic under its own mutex and Postgres is not, so this
// suite structurally CANNOT catch a Postgres-only race. Concurrent-claim
// defects belong in pkg/inboxdb/postgres_test.go, which opens two
// transactions explicitly.
package inboxtest

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/inbox"
)

// RunConformance exercises the Store contract. mk returns a fresh store and a
// child id prefix that is unique to this run.
func RunConformance(t *testing.T, mk func(*testing.T) (inbox.Store, string)) {
	t.Run("AcceptAssignsIdAndTime", func(t *testing.T) {
		s, pfx := mk(t)
		ctx := context.Background()
		got, err := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "hi"})
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if got.ID == "" {
			t.Error("Accept must assign an ID")
		}
		if got.AcceptedAt.IsZero() {
			t.Error("Accept must assign AcceptedAt")
		}
	})

	t.Run("PendingIsPerChildAndOrdered", func(t *testing.T) {
		s, pfx := mk(t)
		ctx := context.Background()
		for _, text := range []string{"one", "two", "three"} {
			if _, err := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: text}); err != nil {
				t.Fatalf("Accept: %v", err)
			}
		}
		if _, err := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "b", Mode: inbox.ModePrompt, Text: "other"}); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		rows, err := s.Pending(ctx, pfx+"a")
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("want 3 rows for child a, got %d", len(rows))
		}
		for i, want := range []string{"one", "two", "three"} {
			if rows[i].Text != want {
				t.Errorf("row %d = %q, want %q", i, rows[i].Text, want)
			}
		}
	})

	t.Run("SentRowsAreNotPending", func(t *testing.T) {
		s, pfx := mk(t)
		ctx := context.Background()
		rec, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "hi"})
		if err := s.MarkSent(ctx, []string{rec.ID}); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		rows, _ := s.Pending(ctx, pfx+"a")
		if len(rows) != 0 {
			t.Fatalf("a sent row must not come back as pending; got %d", len(rows))
		}
	})

	t.Run("ResetSentIsScopedToOneChild", func(t *testing.T) {
		s, pfx := mk(t)
		ctx := context.Background()
		a, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "mine"})
		b, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "b", Mode: inbox.ModePrompt, Text: "someone else's"})
		if err := s.MarkSent(ctx, []string{a.ID, b.ID}); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		n, err := s.ResetSent(ctx, pfx+"a")
		if err != nil {
			t.Fatalf("ResetSent: %v", err)
		}
		if n != 1 {
			t.Errorf("ResetSent moved %d rows, want 1", n)
		}
		if rows, _ := s.Pending(ctx, pfx+"a"); len(rows) != 1 {
			t.Errorf("child a should have 1 pending row, got %d", len(rows))
		}
		// The load-bearing half: another daemon's child is untouched.
		if rows, _ := s.Pending(ctx, pfx+"b"); len(rows) != 0 {
			t.Errorf("ResetSent reached another child's rows: %d became pending", len(rows))
		}
	})

	t.Run("ConsumedIsTerminal", func(t *testing.T) {
		s, pfx := mk(t)
		ctx := context.Background()
		rec, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "hi"})
		if err := s.MarkSent(ctx, []string{rec.ID}); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		if err := s.MarkConsumed(ctx, []string{rec.ID}); err != nil {
			t.Fatalf("MarkConsumed: %v", err)
		}
		if n, err := s.ResetSent(ctx, pfx+"a"); err != nil || n != 0 {
			t.Errorf("ResetSent after consume = (%d, %v), want (0, nil)", n, err)
		}
		if rows, _ := s.Pending(ctx, pfx+"a"); len(rows) != 0 {
			t.Errorf("a consumed row came back as pending")
		}
	})

	t.Run("DropTerminatesEveryNonTerminalRow", func(t *testing.T) {
		s, pfx := mk(t)
		ctx := context.Background()
		p, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "pending"})
		sent, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "sent"})
		done, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "consumed"})
		_ = p
		// A different child's pending row must survive Drop entirely -- the
		// load-bearing half, since Drop is one of the two scoping-critical
		// methods and this is what would catch an unscoped UPDATE.
		other, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "b", Mode: inbox.ModePrompt, Text: "someone else's"})
		if err := s.MarkSent(ctx, []string{sent.ID}); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		if err := s.MarkConsumed(ctx, []string{done.ID}); err != nil {
			t.Fatalf("MarkConsumed: %v", err)
		}
		n, err := s.Drop(ctx, pfx+"a", "child forgotten")
		if err != nil {
			t.Fatalf("Drop: %v", err)
		}
		if n != 2 {
			t.Errorf("Drop returned %d, want 2 (pending + sent, not the consumed one)", n)
		}
		rows, err := s.Pending(ctx, pfx+"b")
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != other.ID {
			t.Errorf("Drop reached another child's rows: %+v", rows)
		}
	})

	t.Run("SweepDeletesOnlyTerminalRows", func(t *testing.T) {
		s, pfx := mk(t)
		ctx := context.Background()
		keep, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "still pending"})
		gone, _ := s.Accept(ctx, inbox.Inbound{ChildID: pfx + "a", Mode: inbox.ModePrompt, Text: "done"})
		if err := s.MarkConsumed(ctx, []string{gone.ID}); err != nil {
			t.Fatalf("MarkConsumed: %v", err)
		}
		n, err := s.Sweep(ctx, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if n != 1 {
			t.Errorf("Sweep deleted %d rows, want 1", n)
		}
		rows, _ := s.Pending(ctx, pfx+"a")
		if len(rows) != 1 || rows[0].ID != keep.ID {
			t.Errorf("Sweep removed a pending row: %+v", rows)
		}
	})
}
