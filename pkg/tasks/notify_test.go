package tasks

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// noticeRecorder collects every Notice the wrapper reports, in call order.
type noticeRecorder struct {
	got []Notice
}

func (r *noticeRecorder) record(n Notice) { r.got = append(r.got, n) }

// notifyingMemory returns a wrapped memory store and its recorder. The
// wrapper is synchronous, so every assertion below reads a settled recorder.
func notifyingMemory() (Store, *noticeRecorder) {
	rec := &noticeRecorder{}
	return Notifying(NewMemoryStore(), rec.record), rec
}

func TestNotifyingAddReportsOnlyTheAddedRows(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	s, rec := notifyingMemory()

	if _, err := s.Add(ctx, "conv", "", []NewTask{{Content: "a"}, {Content: "b"}}); err != nil {
		t.Fatal(err)
	}
	c.Len(rec.got, 1, "one Add is one notice")
	c.Eq(NoticeAdded, rec.got[0].Kind)
	c.Eq("conv", rec.got[0].ConversationID)
	c.Len(rec.got[0].Tasks, 2, "a two-item Add reports both of its rows")

	if _, err := s.Add(ctx, "conv", "", []NewTask{{Content: "c"}}); err != nil {
		t.Fatal(err)
	}
	c.Len(rec.got, 2)
	c.Len(rec.got[1].Tasks, 1, "the second Add must not re-report the rows the first one added")
	c.Eq("3", rec.got[1].Tasks[0].Handle)
}

func TestNotifyingUpdateReportsOnlyNamedHandles(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	s, rec := notifyingMemory()

	if _, err := s.Add(ctx, "conv", "", []NewTask{{Content: "a"}, {Content: "b"}, {Content: "c"}}); err != nil {
		t.Fatal(err)
	}
	// Update returns the conversation's full list; only the named row may be
	// reported, or a subscriber repaints rows that did not change.
	if _, err := s.Update(ctx, "conv", []Change{{Handle: "2", Status: StatusCompleted}}); err != nil {
		t.Fatal(err)
	}
	c.Len(rec.got, 2, "Add then Update is two notices")
	upd := rec.got[1]
	c.Eq(NoticeUpdated, upd.Kind)
	c.Eq("conv", upd.ConversationID)
	c.Len(upd.Tasks, 1, "one named handle reports one row")
	c.Eq("2", upd.Tasks[0].Handle)
	c.Eq(StatusCompleted, upd.Tasks[0].Status)
}

func TestNotifyingDropReportsTheSubtree(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	s, rec := notifyingMemory()

	if _, err := s.Add(ctx, "conv", "", []NewTask{{Content: "top"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, "conv", "1", []NewTask{{Content: "kid"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Drop(ctx, "conv", "1", "whole branch unnecessary"); err != nil {
		t.Fatal(err)
	}
	c.Len(rec.got, 3, "Add, Add, Drop")
	dropped := rec.got[2]
	c.Eq(NoticeDropped, dropped.Kind)
	c.Eq("conv", dropped.ConversationID)
	got := map[string]Task{}
	for _, task := range dropped.Tasks {
		got[task.Handle] = task
	}
	c.Len(dropped.Tasks, 2, "dropping a parent reports its subtree")
	for _, h := range []string{"1", "1.1"} {
		c.Eq(StatusDropped, got[h].Status, "handle %s must be reported dropped", h)
		c.Eq("whole branch unnecessary", got[h].DropReason, "handle %s reason", h)
	}
}

func TestNotifyingAssignReportsTheRow(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	s, rec := notifyingMemory()

	if _, err := s.Add(ctx, "conv", "", []NewTask{{Content: "a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assign(ctx, "conv", "1", "c_worker"); err != nil {
		t.Fatal(err)
	}
	c.Len(rec.got, 2)
	asg := rec.got[1]
	c.Eq(NoticeAssigned, asg.Kind)
	c.Eq("conv", asg.ConversationID)
	c.Len(asg.Tasks, 1)
	c.Eq("1", asg.Tasks[0].Handle)
	c.Eq("c_worker", asg.Tasks[0].Assignee)
}

func TestNotifyingOrphanedGroupsByConversation(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	s, rec := notifyingMemory()

	for _, conv := range []string{"conv-b", "conv-a"} {
		if _, err := s.Add(ctx, conv, "", []NewTask{{Content: "held"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Assign(ctx, conv, "1", "c_dead"); err != nil {
			t.Fatal(err)
		}
	}
	before := len(rec.got)
	if _, err := s.OrphanAssigned(ctx, "c_dead"); err != nil {
		t.Fatal(err)
	}
	notices := rec.got[before:]
	c.Len(notices, 2, "two conversations swept are two notices")
	c.Eq("conv-a", notices[0].ConversationID, "notices are ordered by conversation id")
	c.Eq("conv-b", notices[1].ConversationID)
	for i, conv := range []string{"conv-a", "conv-b"} {
		c.Eq(NoticeOrphaned, notices[i].Kind)
		c.Len(notices[i].Tasks, 1)
		c.Eq(conv, notices[i].Tasks[0].ConversationID, "a notice carries only its own conversation's rows")
		c.Eq(StatusOrphaned, notices[i].Tasks[0].Status)
		c.Eq("1", notices[i].Tasks[0].Handle)
	}
}

func TestNotifyingStaysSilentOnError(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	s, rec := notifyingMemory()

	if _, err := s.Add(ctx, "conv", "", []NewTask{{Content: "a"}}); err != nil {
		t.Fatal(err)
	}
	before := len(rec.got)
	_, err := s.Drop(ctx, "conv", "1", "")
	c.ErrorIs(err, ErrDropReason, "Drop without a reason")
	c.Len(rec.got[before:], 0, "a refused mutation must report nothing")
}

func TestNotifyingStaysSilentWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	s, rec := notifyingMemory()

	t.Run("empty add", func(t *testing.T) {
		c := assert.NewCollecting(t)
		before := len(rec.got)
		got, err := s.Add(ctx, "conv", "", nil)
		c.NoError(err)
		c.Len(got, 0)
		c.Len(rec.got[before:], 0, "an Add that added nothing must report nothing")
	})

	t.Run("orphan with no assignee", func(t *testing.T) {
		c := assert.NewCollecting(t)
		before := len(rec.got)
		got, err := s.OrphanAssigned(ctx, "")
		c.NoError(err)
		c.Len(got, 0)
		c.Len(rec.got[before:], 0, `OrphanAssigned("") must report nothing`)
	})

	t.Run("orphan for an assignee holding nothing", func(t *testing.T) {
		c := assert.NewCollecting(t)
		before := len(rec.got)
		got, err := s.OrphanAssigned(ctx, "c_nobody")
		c.NoError(err)
		c.Len(got, 0)
		c.Len(rec.got[before:], 0, "a sweep that changed nothing must report nothing")
	})
}

func TestNotifyingNilNotifyReturnsInner(t *testing.T) {
	inner := NewMemoryStore()
	if got := Notifying(inner, nil); got != inner {
		t.Fatalf("Notifying with a nil notify returned %#v; want the inner store unchanged", got)
	}
}
