// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	tea "charm.land/bubbletea/v2"

	"github.com/multigres/testkit/assert"
)

// taskLedgerClient is the ControlClient fetchTasks reaches. Only ListTasks is
// implemented: any other call lands on the embedded nil interface and panics,
// which is the loud answer to an RPC a test did not set up.
type taskLedgerClient struct {
	rafikiv1connect.ControlClient

	mu   sync.Mutex
	reqs []*rafikiv1.ListTasksRequest
	rows []*rafikiv1.TaskRow
}

func (f *taskLedgerClient) ListTasks(_ context.Context, req *connect.Request[rafikiv1.ListTasksRequest]) (*connect.Response[rafikiv1.ListTasksResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req.Msg)
	return connect.NewResponse(&rafikiv1.ListTasksResponse{Tasks: f.rows}), nil
}

// listTasksCalls returns the ListTasks requests the cockpit issued, in order.
func (f *taskLedgerClient) listTasksCalls() []*rafikiv1.ListTasksRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*rafikiv1.ListTasksRequest(nil), f.reqs...)
}

// tasksChangedFor is the durable event the daemon publishes after a ledger
// mutation commits. Its child_id names the ledger's OWNER, which is the child
// the box belongs to.
func tasksChangedFor(id string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_TasksChanged{TasksChanged: &rafikiv1.TasksChanged{
			Kind:  "added",
			Tasks: []*rafikiv1.TaskChange{{Handle: "1", Content: "write the test", Status: "pending"}},
		}}}
}

// taskCycle drives one focus-stream update the way the program loop does: the
// events go onto the cockpit's REAL focus channel, waitForEvent collapses them
// into one eventMsg, Update folds it and re-arms, and every command Update
// returned is run. It returns that message plus everything the commands
// produced.
//
// The channel is closed before the commands run, which is what frees the
// re-armed waitForEvent: that waiter blocks until the next focus event and a
// test has none -- and tea.Batch collapses to the waiter ALONE whenever
// syncOSC has nothing to emit, so it would be handed to the caller directly
// rather than inside a BatchMsg. One call per cockpit, and nothing pushed
// afterwards.
func taskCycle(t *testing.T, c *Cockpit, evs ...*rafikiv1.Event) (eventMsg, []tea.Msg) {
	t.Helper()
	ck := assert.NewAborting(t)

	for _, ev := range evs {
		c.focusCh <- ev
	}
	msg := waitForEvent(c.focusCh)()
	em, ok := msg.(eventMsg)
	ck.Require().True(ok, "waitForEvent() = %T, want eventMsg", msg)

	_, cmd := c.Update(em)
	close(c.focusCh)
	if cmd == nil {
		return em, nil
	}
	produced := cmd()
	if produced == nil {
		return em, nil
	}
	cmds, ok := produced.(tea.BatchMsg)
	if !ok {
		return em, []tea.Msg{produced}
	}

	replies := make(chan tea.Msg, len(cmds))
	var wg sync.WaitGroup
	for _, sub := range cmds {
		if sub == nil {
			continue
		}
		wg.Add(1)
		go func(sub tea.Cmd) {
			defer wg.Done()
			replies <- sub()
		}(sub)
	}
	go func() { wg.Wait(); close(replies) }()

	var out []tea.Msg
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, open := <-replies:
			if !open {
				return em, out
			}
			out = append(out, m)
		case <-deadline:
			t.Fatal("a command Update returned never finished")
			return em, out
		}
	}
}

// tasksLoadedFrom picks the ledger reply out of a cycle's messages.
func tasksLoadedFrom(msgs []tea.Msg) (tasksLoadedMsg, bool) {
	for _, m := range msgs {
		if tl, ok := m.(tasksLoadedMsg); ok {
			return tl, true
		}
	}
	return tasksLoadedMsg{}, false
}

// The daemon publishes tasks_changed on the ledger owner's bus after the
// mutation commits, so that event -- and nothing the cockpit infers from tool
// calls -- is what refills the box.
func TestTasksChangedEventRefreshesTheBox(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)})
	// startFocus is what records the subscription owner in production; the
	// channel and the waiter below are the real ones.
	c.focusChild = "c_1"
	client := &taskLedgerClient{rows: []*rafikiv1.TaskRow{
		{Handle: "1", Content: "write the test", Status: "pending"},
	}}
	c.client = client

	_, msgs := taskCycle(t, c, tasksChangedFor("c_1", 3))

	calls := client.listTasksCalls()
	ck.Require().Len(calls, 1, "ListTasks calls after a tasks_changed event")
	ck.Eq("c_1", calls[0].GetOwnerChildId(), "ListTasks asked for the ledger owned by")
	ck.Eq("", calls[0].GetConversationId(), "ListTasks must not address the ledger by conversation")

	loaded, ok := tasksLoadedFrom(msgs)
	ck.Require().True(ok, "the commands Update returned produced no tasksLoadedMsg: %#v", msgs)
	ck.Eq("c_1", loaded.childID, "tasksLoadedMsg is keyed by")
	c.Update(loaded)
	ck.Len(c.tasks["c_1"], 1, "rows in the box")

	// The event carries no transcript content, so the session must take its
	// ordinal and add nothing: TasksChanged is a durable event like any other.
	s := c.sessions["c_1"]
	ck.Require().NotNil(s, "session for c_1")
	ck.Len(s.Blocks, 0, "blocks appended by a tasks_changed event")
	ck.Eq(int32(3), s.Cursor, "the session cursor after a tasks_changed event")
}

func TestTasksChangedForAnotherChildDoesNotRefreshTheViewedBox(t *testing.T) {
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{
		summaryFor("c_1", "one", 0), summaryFor("c_2", "two", 0),
	})
	c.focusChild = "c_1"
	client := &taskLedgerClient{}
	c.client = client

	taskCycle(t, c, tasksChangedFor("c_2", 4))

	assert.NewCollecting(t).Len(client.listTasksCalls(), 0,
		"ListTasks calls for a ledger the pane is not showing")
}

// The box is addressed by the child that owns the ledger, so a ListChildren row
// with no session id still fills it. A child that has not run a turn yet has
// none, and keying the fetch off SessionID left its box permanently empty.
func TestFetchTasksDoesNotNeedASessionID(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)})
	client := &taskLedgerClient{rows: []*rafikiv1.TaskRow{
		{Handle: "1", Content: "write the test", Status: "pending"},
	}}
	c.client = client

	row, ok := c.rail.Get("c_1")
	ck.Require().True(ok, "rail row for c_1")
	ck.Eq("", row.SessionID, "setup: the row's session id")

	cmd := c.fetchTasks("c_1")
	ck.Require().NotNil(cmd, "fetchTasks issued no command for a rail row with no session id")

	msg := cmd()
	loaded, ok := msg.(tasksLoadedMsg)
	ck.Require().True(ok, "fetchTasks() = %T, want tasksLoadedMsg", msg)
	ck.Eq("c_1", loaded.childID, "tasksLoadedMsg is keyed by")
	ck.Len(loaded.rows, 1, "rows returned")

	calls := client.listTasksCalls()
	ck.Require().Len(calls, 1, "ListTasks calls")
	ck.Eq("c_1", calls[0].GetOwnerChildId(), "ListTasks asked for the ledger owned by")
}

// A burst of mutations arrives as one eventMsg, and the fetch it asks for is
// one ListTasks rather than one per event.
func TestTasksChangedBurstCoalescesToOneFetch(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	c.rail.Seed([]*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)})
	c.focusChild = "c_1"
	client := &taskLedgerClient{}
	c.client = client

	em, _ := taskCycle(t, c,
		tasksChangedFor("c_1", 1), tasksChangedFor("c_1", 2), tasksChangedFor("c_1", 3))

	ck.Len(em.evs, 3, "events drained into one eventMsg")
	ck.Len(client.listTasksCalls(), 1, "ListTasks calls for a three-event burst")
}
