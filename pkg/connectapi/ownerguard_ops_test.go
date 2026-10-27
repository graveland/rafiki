// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// This file pins the owner guard on the operator-side verbs (childops.go,
// events_extra.go, stream.go, tasks.go) and the predicate seam
// CloseAllExited travels through.

// opsRecordingTaskLister records the ledger request ListTasks forwards, so a
// refusal can be proven to have never reached it.
type opsRecordingTaskLister struct {
	calls int
	got   protocol.TaskListRequest
}

func (f *opsRecordingTaskLister) TaskList(_ context.Context, r protocol.TaskListRequest) ([]tasks.Task, error) {
	f.calls++
	f.got = r
	return nil, nil
}

// opsFixture is a Server whose owner guard owns exactly "c_mine" and
// conversation "conv-mine", with every backend these verbs reach recorded.
type opsFixture struct {
	s        *connectapi.Server
	guard    *fakeOwnerGuard
	ops      *fakeChildOps
	raw      *fakeRawChildIO
	tasks    *opsRecordingTaskLister
	children *verbsRecordingLister
}

func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	f := &opsFixture{
		guard: &fakeOwnerGuard{
			owned: map[string]bool{"c_mine": true},
			convs: map[string]bool{"conv-mine": true},
		},
		ops:      &fakeChildOps{},
		raw:      &fakeRawChildIO{},
		tasks:    &opsRecordingTaskLister{},
		children: &verbsRecordingLister{all: []protocol.ChildSummary{{ChildID: "c_mine"}, {ChildID: "c_foreign"}}},
	}
	f.s = connectapi.NewServer(nil)
	f.s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard { return f.guard })
	f.s.SetChildOps(f.ops)
	f.s.SetRawChildIO(f.raw)
	f.s.SetTaskLister(f.tasks)
	f.s.SetChildLister(f.children)
	return f
}

// TestOwnerGuardOpsRefusesForeignAndUnknownIdentically is the anti-probe pin
// for the operator verbs: foreign and unknown ids answer the same
// PermissionDenied text, and the ChildOps/RawChildIO backend is never reached.
func TestOwnerGuardOpsRefusesForeignAndUnknownIdentically(t *testing.T) {
	c := assert.NewAborting(t)

	verbs := []struct {
		name    string
		invoke  func(f *opsFixture, id string) error
		reached func(f *opsFixture) bool
	}{
		{"Resume", func(f *opsFixture, id string) error {
			_, err := f.s.Resume(context.Background(), connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: id}))
			return err
		}, func(f *opsFixture) bool { return f.ops.resumeChildID != "" }},
		{"SetLabels", func(f *opsFixture, id string) error {
			_, err := f.s.SetLabels(context.Background(), connect.NewRequest(&rafikiv1.SetLabelsRequest{
				ChildId: id, Set: map[string]string{"k": "v"},
			}))
			return err
		}, func(f *opsFixture) bool { return f.ops.labelsChildID != "" }},
		{"GetStreams", func(f *opsFixture, id string) error {
			_, err := f.s.GetStreams(context.Background(), connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: id}))
			return err
		}, func(f *opsFixture) bool { return f.raw.streamsChildID != "" }},
		{"SendFrame", func(f *opsFixture, id string) error {
			_, err := f.s.SendFrame(context.Background(), connect.NewRequest(&rafikiv1.SendFrameRequest{
				ChildId: id, FrameJson: `{"type":"x"}`,
			}))
			return err
		}, func(f *opsFixture) bool { return f.raw.sendChildID != "" }},
	}

	var firstText func(id string) string
	for _, verb := range verbs {
		for _, id := range []string{"c_foreign", "c_unknown"} {
			f := newOpsFixture(t)
			err := verb.invoke(f, id)
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s(%s) = %v, want PermissionDenied", verb.name, id, err)
			c.False(verb.reached(f), "%s(%s) reached its backend despite the owner refusal", verb.name, id)

			msg := err.Error()
			if firstText == nil {
				firstText = func(id string) string { return strings.ReplaceAll(msg, "c_foreign", id) }
				c.False(!strings.Contains(msg, "is not owned by you"), "%s refusal %q does not carry the owner text", verb.name, msg)
				continue
			}
			c.Eq(firstText(id), msg, "%s(%s) refusal text differs from the first refusal", verb.name, id)
		}
	}

	// The control group: an owned id reaches the backend.
	f := newOpsFixture(t)
	c.NoError(verbs[0].invoke(f, "c_mine"), "Resume(c_mine)")
	c.Eq("c_mine", f.ops.resumeChildID, "Resume(c_mine) never reached the backend")
}

// opsStreamServer mounts a Control service with the given guard source, so a
// StreamEvents refusal can be observed on the wire and an All-subject stream
// can be drained.
func opsStreamServer(t *testing.T, guard connectapi.OwnerGuard, src connectapi.EventSource) rafikiv1connect.ControlClient {
	t.Helper()
	s := connectapi.NewServer(nil)
	if guard != nil {
		s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard { return guard })
	}
	s.SetLineage(&fakeLineage{depth: map[string]map[string]int{}, labels: map[string]map[string]string{}})
	s.SetEventLog(eventlog.NewMemory())
	if src != nil {
		s.SetEventSource(src)
	}
	path, h := s.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
}

// TestOwnerGuardStreamEventsScopesSubjects pins StreamEvents' two halves: a
// child or subtree subject naming a foreign id is refused before any replay,
// while the All subject streams the caller's own children only — a foreign
// child's event never reaches the wire.
func TestOwnerGuardStreamEventsScopesSubjects(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	guard := &fakeOwnerGuard{owned: map[string]bool{"c_mine": true}, convs: map[string]bool{}}
	client := opsStreamServer(t, guard, &fakeSource{
		ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10),
	})

	for _, subj := range []*rafikiv1.EventSubject{
		{Scope: &rafikiv1.EventSubject_Child{Child: "c_foreign"}},
		{Scope: &rafikiv1.EventSubject_Subtree{Subtree: "c_foreign"}},
	} {
		// A server-streaming refusal arrives on the stream, not the call.
		stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{Subject: subj}))
		c.NoError(err, "StreamEvents(%T) call", subj.Scope)
		c.False(stream.Receive(), "StreamEvents(%T) delivered an event despite the owner refusal", subj.Scope)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(stream.Err()),
			"StreamEvents(%T) = %v, want PermissionDenied before any replay", subj.Scope, stream.Err())
	}

	// The All subject is the caller's own fleet view: a foreign child's event
	// is dropped by the send-side wrapper, an owned child's passes through.
	src := &fakeSource{ch: make(chan *rafikiv1.Event, 10), allCh: make(chan *rafikiv1.Event, 10)}
	client = opsStreamServer(t, guard, src)
	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{}},
	}))
	c.NoError(err, "StreamEvents(All)")

	src.allCh <- statusEvent("c_foreign", "idle")
	src.allCh <- statusEvent("c_mine", "idle")

	c.True(stream.Receive(), "StreamEvents(All) delivered nothing: %v", stream.Err())
	c.Eq("c_mine", stream.Msg().GetChildId(), "first delivered event is a foreign child's")
}

// TestOwnerGuardListTasks pins ListTasks' ledger boundary: a foreign
// owner_child_id is refused by the guard, an unowned or empty conversation id
// is refused with the handler's own sentence, and an owned conversation
// reaches the lister.
func TestOwnerGuardListTasks(t *testing.T) {
	const refusedText = "list the task ledger of one conversation or child you own; every conversation is the operator's view"

	for _, tc := range []struct {
		name    string
		req     *rafikiv1.ListTasksRequest
		reached bool
	}{
		{"foreign owner_child_id", &rafikiv1.ListTasksRequest{OwnerChildId: "c_foreign"}, false},
		{"unknown owner_child_id", &rafikiv1.ListTasksRequest{OwnerChildId: "c_unknown"}, false},
		{"unowned conversation", &rafikiv1.ListTasksRequest{ConversationId: "conv-theirs"}, false},
		{"no target at all", &rafikiv1.ListTasksRequest{}, false},
		{"owned conversation", &rafikiv1.ListTasksRequest{ConversationId: "conv-mine"}, true},
		{"owned owner_child_id", &rafikiv1.ListTasksRequest{OwnerChildId: "c_mine"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			f := newOpsFixture(t)
			_, err := f.s.ListTasks(context.Background(), connect.NewRequest(tc.req))
			if tc.reached {
				c.NoError(err, "ListTasks(%s) = %v, want the lister reached", tc.name, err)
				c.Eq(1, f.tasks.calls, "ListTasks(%s) never reached the lister", tc.name)
				return
			}
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "ListTasks(%s) = %v, want PermissionDenied", tc.name, err)
			c.Eq(0, f.tasks.calls, "ListTasks(%s) reached the lister despite the refusal", tc.name)
			if tc.req.GetOwnerChildId() == "" {
				c.ErrorContains(err, refusedText, "ListTasks(%s) refusal text", tc.name)
			}
		})
	}
}

// TestOwnerGuardStatusCountsOwnedOnly pins the filtered Status: the child
// counts cover the caller's own children only, while the daemon vitals pass
// through untouched.
func TestOwnerGuardStatusCountsOwnedOnly(t *testing.T) {
	c := assert.NewAborting(t)
	f := newOpsFixture(t)
	f.ops.statusOut = &rafikiv1.StatusResponse{
		Version:     "v1.2.3",
		MemoryBytes: 4096,
		Socket:      "/run/rafiki.sock",
		Children:    &rafikiv1.StatusResponse_ChildCounts{Live: 9, Exited: 9},
	}
	f.children.all = []protocol.ChildSummary{
		{ChildID: "c_mine", Status: string(protocol.StatusIdle)},
		{ChildID: "c_mine2", Status: string(protocol.StatusExited)},
		{ChildID: "c_foreign", Status: string(protocol.StatusIdle)},
	}
	f.guard.owned["c_mine2"] = true

	resp, err := f.s.Status(context.Background(), connect.NewRequest(&rafikiv1.StatusRequest{}))
	c.NoError(err, "Status")
	counts := resp.Msg.GetChildren()
	c.Eq(int32(1), counts.GetLive(), "live count")
	c.Eq(int32(1), counts.GetExited(), "exited count")
	c.Eq("v1.2.3", resp.Msg.GetVersion(), "daemon vitals must pass through")
	c.Eq(int64(4096), resp.Msg.GetMemoryBytes(), "daemon vitals must pass through")
}

// TestOwnerGuardSearchFiltersHits pins the filtered Search: foreign hits are
// dropped, total_hits is recomputed, and the owned hits survive intact.
func TestOwnerGuardSearchFiltersHits(t *testing.T) {
	c := assert.NewAborting(t)
	f := newOpsFixture(t)
	f.ops.searchOut = &rafikiv1.SearchResponse{
		Hits: []*rafikiv1.SearchResponse_SearchHit{
			{ChildId: "c_mine", Snippet: "mine"},
			{ChildId: "c_foreign", Snippet: "theirs"},
		},
		TotalHits: 2,
		Scanned:   17,
	}
	resp, err := f.s.Search(context.Background(), connect.NewRequest(&rafikiv1.SearchRequest{Query: "q"}))
	c.NoError(err, "Search")
	c.Require().Len(resp.Msg.GetHits(), 1, "hits = %v, want the owned one only", resp.Msg.GetHits())
	c.Eq("c_mine", resp.Msg.GetHits()[0].GetChildId(), "surviving hit")
	c.Eq(int32(1), resp.Msg.GetTotalHits(), "total_hits recomputed")
	c.Eq(int32(17), resp.Msg.GetScanned(), "scanned stays a daemon-wide diagnostic")
}

// TestOwnerGuardCloseAllExitedPassesPredicate pins the predicate seam: with a
// guard wired the ChildOps backend receives a non-nil predicate answering the
// guard's own ownership, and with a nil-resolving source it receives nil
// (full authority) — filtering the ANSWER instead would report children
// closed that were never touched.
func TestOwnerGuardCloseAllExitedPassesPredicate(t *testing.T) {
	c := assert.NewAborting(t)

	f := newOpsFixture(t)
	_, err := f.s.CloseAllExited(context.Background(), connect.NewRequest(&rafikiv1.CloseAllExitedRequest{}))
	c.NoError(err, "CloseAllExited")
	c.True(f.ops.exitedOwns != nil, "CloseAllExited passed no predicate with a guard wired")
	c.True(f.ops.exitedOwns("c_mine"), "predicate refuses an owned child")
	c.False(f.ops.exitedOwns("c_foreign"), "predicate admits a foreign child")

	s := connectapi.NewServer(nil)
	ops := &fakeChildOps{}
	s.SetChildOps(ops)
	s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard { return nil })
	_, err = s.CloseAllExited(context.Background(), connect.NewRequest(&rafikiv1.CloseAllExitedRequest{}))
	c.NoError(err, "CloseAllExited (unrestricted)")
	c.Nil(ops.exitedOwns, "a nil-resolving source must pass a nil predicate (full authority)")
}

// TestOwnerGuardOpsSendFrameReachesRawIO is the control group for the raw-I/O
// verbs: an owned id reaches the backend with the frame intact.
func TestOwnerGuardOpsSendFrameReachesRawIO(t *testing.T) {
	c := assert.NewAborting(t)
	f := newOpsFixture(t)
	_, err := f.s.SendFrame(context.Background(), connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId: "c_mine", FrameJson: `{"type":"x"}`,
	}))
	c.NoError(err, "SendFrame(c_mine)")
	c.Eq("c_mine", f.raw.sendChildID, "SendFrame(c_mine) never reached the backend")
	var frame map[string]json.RawMessage
	c.NoError(json.Unmarshal(f.raw.sendFrame, &frame), "forwarded frame is not JSON")
}
