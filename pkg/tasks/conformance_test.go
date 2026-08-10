// Package tasks_test provides the conformance suite that both the memory
// and Postgres Store implementations must pass unchanged. A divergence
// between them is how a DB-less unit test passes while production breaks.
package tasks_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/tasks"
)

// RunConformance exercises the Store contract. Both the memory and the
// Postgres implementation must pass it unchanged.
//
// mk returns a Store and the conversation id to use for all operations.
// The memory store returns a literal; the Postgres store creates a real
// conversation row and returns its UUID.
func RunConformance(t *testing.T, mk func(*testing.T) (tasks.Store, string)) {
	ctx := context.Background()

	t.Run("add assigns sequential handles", func(t *testing.T) {
		s, conv := mk(t)
		got, err := s.Add(ctx, conv, "", []tasks.NewTask{
			{Content: "one", ActiveForm: "doing one"},
			{Content: "two", ActiveForm: "doing two"},
		})
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if len(got) != 2 || got[0].Handle != "1" || got[1].Handle != "2" {
			t.Fatalf("handles = %q,%q; want 1,2", got[0].Handle, got[1].Handle)
		}
	})

	t.Run("add under a parent nests", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "top"}}); err != nil {
			t.Fatal(err)
		}
		kids, err := s.Add(ctx, conv, "1", []tasks.NewTask{{Content: "kid"}})
		if err != nil {
			t.Fatalf("Add under parent: %v", err)
		}
		if kids[0].Handle != "1.1" {
			t.Fatalf("handle = %q; want 1.1", kids[0].Handle)
		}
	})

	// THE ordinal test. A dropped sibling must not free its handle.
	t.Run("dropped handles are never reused", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "a"}, {Content: "b"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Drop(ctx, conv, "2", "not needed"); err != nil {
			t.Fatalf("Drop: %v", err)
		}
		added, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "c"}})
		if err != nil {
			t.Fatal(err)
		}
		if added[0].Handle != "3" {
			t.Fatalf("handle after a drop = %q; want 3. A reused handle makes a "+
				"coordinator's stale reference silently address a different task.",
				added[0].Handle)
		}
	})

	t.Run("update changes only what it names", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "a"}, {Content: "b"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Update(ctx, conv, []tasks.Change{{Handle: "1", Status: tasks.StatusCompleted}}); err != nil {
			t.Fatal(err)
		}
		all, _ := s.List(ctx, tasks.ListFilter{ConversationID: conv})
		byHandle := map[string]tasks.Task{}
		for _, task := range all {
			byHandle[task.Handle] = task
		}
		if byHandle["1"].Status != tasks.StatusCompleted {
			t.Error("named task was not updated")
		}
		if byHandle["2"].Status != tasks.StatusPending {
			t.Error("unnamed task was modified — Update must touch only what it names")
		}
	})

	t.Run("drop requires a reason", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "a"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Drop(ctx, conv, "1", ""); !errors.Is(err, tasks.ErrDropReason) {
			t.Fatalf("Drop with no reason: err = %v; want ErrDropReason", err)
		}
	})

	t.Run("drop refuses an assigned task and names the assignee", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "a"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Assign(ctx, conv, "1", "c_worker"); err != nil {
			t.Fatal(err)
		}
		_, err := s.Drop(ctx, conv, "1", "changed my mind")
		if !errors.Is(err, tasks.ErrAssigned) {
			t.Fatalf("err = %v; want ErrAssigned", err)
		}
		if !strings.Contains(err.Error(), "c_worker") {
			t.Errorf("refusal must name the assignee; got %q", err)
		}
	})

	t.Run("dropping a parent cascades to unassigned children", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "top"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Add(ctx, conv, "1", []tasks.NewTask{{Content: "kid"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Drop(ctx, conv, "1", "whole branch unnecessary"); err != nil {
			t.Fatalf("Drop parent: %v", err)
		}
		all, _ := s.List(ctx, tasks.ListFilter{ConversationID: conv, IncludeDropped: true})
		for _, task := range all {
			if task.Status != tasks.StatusDropped {
				t.Errorf("task %s status = %s; want dropped (cascade)", task.Handle, task.Status)
			}
		}
	})

	t.Run("dropping a parent with an assigned descendant refuses and changes nothing", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "top"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Add(ctx, conv, "1", []tasks.NewTask{{Content: "kid"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Assign(ctx, conv, "1.1", "c_worker"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Drop(ctx, conv, "1", "nope"); !errors.Is(err, tasks.ErrAssigned) {
			t.Fatalf("err = %v; want ErrAssigned", err)
		}
		all, _ := s.List(ctx, tasks.ListFilter{ConversationID: conv, IncludeDropped: true})
		for _, task := range all {
			if task.Status == tasks.StatusDropped {
				t.Fatal("a refused cascade must leave NOTHING dropped — no partial state")
			}
		}
	})

	t.Run("orphan sweep marks a dead agent's work", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "a"}, {Content: "b"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Assign(ctx, conv, "1", "c_dead"); err != nil {
			t.Fatal(err)
		}
		n, err := s.OrphanAssigned(ctx, "c_dead")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("orphaned %d; want 1", n)
		}
		all, _ := s.List(ctx, tasks.ListFilter{ConversationID: conv})
		for _, task := range all {
			if task.Handle == "1" {
				if task.Status != tasks.StatusOrphaned {
					t.Errorf("status = %s; want orphaned", task.Status)
				}
				// The assignee is RETAINED. "c_dead died holding this" is
				// exactly what a coordinator needs; nulling it destroys that.
				if task.Assignee != "c_dead" {
					t.Errorf("assignee = %q; want c_dead retained", task.Assignee)
				}
			}
		}
	})

	t.Run("metadata filter selects", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{
			{Content: "a", Metadata: map[string]string{"project": "sentinel"}},
			{Content: "b"},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := s.List(ctx, tasks.ListFilter{
			ConversationID: conv,
			Metadata:       map[string]string{"project": "sentinel"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Content != "a" {
			t.Fatalf("metadata filter returned %d rows; want 1 (task a)", len(got))
		}
	})

	t.Run("list hides dropped by default", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "a"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Drop(ctx, conv, "1", "gone"); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.List(ctx, tasks.ListFilter{ConversationID: conv}); len(got) != 0 {
			t.Error("dropped rows must be hidden by default")
		}
		if got, _ := s.List(ctx, tasks.ListFilter{ConversationID: conv, IncludeDropped: true}); len(got) != 1 {
			t.Error("IncludeDropped must reveal them")
		}
	})

	t.Run("unknown handle is ErrNotFound", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Update(ctx, conv, []tasks.Change{{Handle: "9.9", Status: tasks.StatusCompleted}}); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("err = %v; want ErrNotFound", err)
		}
	})

	t.Run("a batch with one bad handle changes nothing", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{
			{Content: "one", ActiveForm: "one-ing"},
			{Content: "two", ActiveForm: "two-ing"},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Update(ctx, conv, []tasks.Change{
			{Handle: "1", Status: tasks.StatusCompleted},
			{Handle: "99", Status: tasks.StatusCompleted},
		})
		if err == nil {
			t.Fatal("update with an unknown handle must fail")
		}
		rows, err := s.List(ctx, tasks.ListFilter{ConversationID: conv})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Status != tasks.StatusPending {
				t.Fatalf("task %s is %s; a failed batch must apply nothing", r.Handle, r.Status)
			}
		}
	})

	t.Run("update refuses statuses the tool does not offer", func(t *testing.T) {
		s, conv := mk(t)
		if _, err := s.Add(ctx, conv, "", []tasks.NewTask{{Content: "one", ActiveForm: "one-ing"}}); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []tasks.Status{tasks.StatusDropped, tasks.StatusOrphaned} {
			if _, err := s.Update(ctx, conv, []tasks.Change{{Handle: "1", Status: bad}}); err == nil {
				t.Fatalf("Update accepted %q; dropped is task_drop's job and orphaned is the daemon's", bad)
			}
		}
	})
}

func TestMemoryStoreConformance(t *testing.T) {
	RunConformance(t, func(*testing.T) (tasks.Store, string) {
		return tasks.NewMemoryStore(), "conv-1"
	})
}
