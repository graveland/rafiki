// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

type fakeLister struct {
	all        []protocol.ChildSummary
	gotStatus  []string
	lookupMiss bool
}

func (f *fakeLister) ListChildren(statuses []string) []protocol.ChildSummary {
	f.gotStatus = statuses
	return f.all
}

func (f *fakeLister) GetChild(childID string) (protocol.ChildSummary, bool) {
	if f.lookupMiss {
		return protocol.ChildSummary{}, false
	}
	for _, c := range f.all {
		if c.ChildID == childID {
			return c, true
		}
	}
	return protocol.ChildSummary{}, false
}

func sampleChildren() []protocol.ChildSummary {
	pid := 4242
	return []protocol.ChildSummary{{
		ChildID: "c_1", Name: "scout", Kind: "fundi", Status: "idle",
		Model: "claude-opus-5", Cwd: "/tmp", PID: &pid,
		StartedAt: 100, LastActivity: 200, SessionID: "conv-uuid",
		Labels: map[string]string{"rafiki/parent": "c_0"}, ContextWindow: 200000,
	}}
}

func TestListChildrenMapsFields(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: sampleChildren()})

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(resp.Msg.GetChildren()) != 1 {
		t.Fatalf("children = %d, want 1", len(resp.Msg.GetChildren()))
	}
	c := resp.Msg.GetChildren()[0]
	if c.GetChildId() != "c_1" || c.GetName() != "scout" || c.GetKind() != "fundi" {
		t.Errorf("identity fields wrong: %+v", c)
	}
	if c.GetStatus() != "idle" || c.GetModel() != "claude-opus-5" || c.GetCwd() != "/tmp" {
		t.Errorf("state fields wrong: %+v", c)
	}
	if c.GetPid() != 4242 {
		t.Errorf("Pid = %d, want 4242", c.GetPid())
	}
	if c.GetStartedAt() != 100 || c.GetLastActivity() != 200 {
		t.Errorf("timestamps wrong: %+v", c)
	}
	if c.GetSessionId() != "conv-uuid" || c.GetContextWindow() != 200000 {
		t.Errorf("session/window wrong: %+v", c)
	}
	if c.GetLabels()["rafiki/parent"] != "c_0" {
		t.Errorf("labels wrong: %+v", c.GetLabels())
	}
}

// TestListChildrenNilPidStaysNil proves the optional field survives: an exited
// child has no pid, and 0 is a legal pid value.
func TestListChildrenCarriesLatestOrdinal(t *testing.T) {
	ctx := context.Background()
	elog := eventlog.NewMemory()
	_, _ = elog.Append(ctx, "c_1", statusEvent("c_1", "idle"))
	_, _ = elog.Append(ctx, "c_1", statusEvent("c_1", "running"))

	s := connectapi.NewServer(nil)
	s.SetEventLog(elog)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{
		{ChildID: "c_1"},
		{ChildID: "c_empty"},
	}})

	resp, err := s.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	children := resp.Msg.GetChildren()
	if len(children) != 2 {
		t.Fatalf("got %d children, want 2", len(children))
	}
	var c1, cEmpty *rafikiv1.ChildSummary
	for _, c := range children {
		if c.GetChildId() == "c_1" {
			c1 = c
		} else if c.GetChildId() == "c_empty" {
			cEmpty = c
		}
	}
	if c1 == nil || c1.LatestOrdinal == nil || *c1.LatestOrdinal != 1 {
		t.Fatalf("c_1 LatestOrdinal = %v, want 1", c1.LatestOrdinal)
	}
	if cEmpty == nil || cEmpty.LatestOrdinal != nil {
		t.Fatalf("c_empty LatestOrdinal = %v, want nil", cEmpty.LatestOrdinal)
	}
}
func TestListChildrenNilPidStaysNil(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: []protocol.ChildSummary{{ChildID: "c_1"}}})

	resp, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if resp.Msg.GetChildren()[0].Pid != nil {
		t.Error("Pid must stay nil when the source PID is nil")
	}
}

func TestListChildrenForwardsStatusFilter(t *testing.T) {
	f := &fakeLister{all: sampleChildren()}
	s := connectapi.NewServer(nil)
	s.SetChildLister(f)

	if _, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{Statuses: []string{"idle", "streaming"}})); err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(f.gotStatus) != 2 || f.gotStatus[0] != "idle" || f.gotStatus[1] != "streaming" {
		t.Errorf("forwarded statuses = %v, want [idle streaming]", f.gotStatus)
	}
}

func TestListChildrenWithoutListerFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.ListChildren(context.Background(),
		connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestGetChildReturnsOne(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{all: sampleChildren()})

	resp, err := s.GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_1"}))
	if err != nil {
		t.Fatalf("GetChild: %v", err)
	}
	if resp.Msg.GetChild().GetChildId() != "c_1" {
		t.Errorf("ChildId = %q, want c_1", resp.Msg.GetChild().GetChildId())
	}
}

func TestGetChildUnknownIsNotFound(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{lookupMiss: true})

	_, err := s.GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: "c_nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestGetChildRejectsEmptyID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildLister(&fakeLister{})
	_, err := s.GetChild(context.Background(),
		connect.NewRequest(&rafikiv1.GetChildRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}
