package childstore_test

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestSession_Snapshot_CopiesFields(t *testing.T) {
	c := assert.NewAborting(t)
	s := &childstore.Session{
		ChildID: "c_1", Name: "x",
		Status: protocol.StatusIdle, StartedAt: time.Unix(100, 0),
	}
	snap := s.Snapshot()

	// Mutate the original; snapshot must not change.
	s.Name = "y"
	s.Status = protocol.StatusStreaming
	c.Eq("x", snap.Name, "snapshot Name aliased: got")
	c.Eq(protocol.StatusIdle, snap.Status, "snapshot Status aliased: got")
}

func TestSession_Snapshot_CopiesExitCode(t *testing.T) {
	c := assert.NewAborting(t)
	code := 1
	s := &childstore.Session{ChildID: "c1", ExitCode: &code}
	snap := s.Snapshot()

	// Mutate original; snapshot's ExitCode must not change.
	*s.ExitCode = 2
	c.NotNil(snap.ExitCode, "snapshot ExitCode is nil")
	c.Eq(1, *snap.ExitCode, "ExitCode aliased: got")

	// nil ExitCode → nil in snapshot.
	s2 := &childstore.Session{ChildID: "c2"}
	snap2 := s2.Snapshot()
	if snap2.ExitCode != nil {
		t.Fatalf("nil ExitCode should stay nil, got pointer to %d", *snap2.ExitCode)
	}
}

func TestSession_Snapshot_CopiesRecordRequests(t *testing.T) {
	c := assert.NewAborting(t)
	s := &childstore.Session{ChildID: "c_1", RecordRequests: true}
	snap := s.Snapshot()
	c.True(snap.RecordRequests, "snapshot did not carry RecordRequests=true")

	s2 := &childstore.Session{ChildID: "c_2", RecordRequests: false}
	snap2 := s2.Snapshot()
	c.False(snap2.RecordRequests, "snapshot should not set RecordRequests when the session did not")
}

func TestSession_Snapshot_CopiesSlices(t *testing.T) {
	s := &childstore.Session{
		ChildID:    "c_1",
		Extensions: []string{"a", "b"},
	}
	snap := s.Snapshot()
	s.Extensions[0] = "MUTATED"
	assert.NewAborting(t).Eq("a", snap.Extensions[0], "slice aliased: %v", snap.Extensions)
}
