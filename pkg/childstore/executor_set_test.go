package childstore_test

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
)

func TestResolveSetSpec(t *testing.T) {
	tests := []struct {
		name         string
		set          []string
		legacy       string
		wantAll      bool
		wantIDs      []string
		wantIDsNil   bool
		wantSelector string
	}{
		{name: "nil set and no selector is all", wantAll: true, wantIDsNil: true},
		{name: "nil set keeps legacy selector", legacy: "machine=a", wantIDsNil: true, wantSelector: "machine=a"},
		{name: "stored all beats legacy selector", set: []string{"all"}, legacy: "machine=a", wantAll: true, wantIDsNil: true},
		{name: "ids", set: []string{"id1", "id2"}, wantIDs: []string{"id1", "id2"}},
		{name: "empty set addresses nothing", set: []string{}, wantIDs: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			all, ids, sel := childstore.ResolveSetSpec(tt.set, tt.legacy)
			c.Eq(tt.wantAll, all, "all")
			c.Eq(tt.wantSelector, sel, "selector")
			if tt.wantIDsNil {
				c.True(ids == nil, "ids must be nil, got %v", ids)
				return
			}
			c.True(ids != nil, "ids must be non-nil")
			c.EqDeep(tt.wantIDs, ids, "ids")
		})
	}
}

func TestSetExecutorSetKeepsNilAndEmptyDistinct(t *testing.T) {
	c := assert.NewAborting(t)
	s := childstore.New()
	s.Insert(newSess("c_1", "foo", "/x"))

	c.NoError(s.SetExecutorSet("c_1", nil))
	snap, ok := s.Get("c_1")
	c.True(ok, "missing")
	c.True(snap.ExecutorSet == nil, "nil set must stay nil, got %#v", snap.ExecutorSet)

	c.NoError(s.SetExecutorSet("c_1", []string{}))
	snap, _ = s.Get("c_1")
	c.True(snap.ExecutorSet != nil && len(snap.ExecutorSet) == 0, "empty set must stay empty non-nil, got %#v", snap.ExecutorSet)
}

// The set survives every hop a child's state takes (live session, snapshot,
// persisted record, rebuilt session), nil and empty staying distinct, and no
// hop aliases the caller's slice: a later edit to the caller's backing array
// must not rewrite a stored set.
func TestExecutorSetSurvivesSessionSnapshotRecordHops(t *testing.T) {
	shapes := []struct {
		name string
		set  []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"all", []string{childstore.ExecutorSetAll}},
		{"ids", []string{"x_1", "x_2"}},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			caller := append([]string(nil), tc.set...)
			if tc.set == nil {
				caller = nil
			} else if len(tc.set) == 0 {
				caller = []string{}
			}
			sess := newSess("c_1", "foo", "/x")
			sess.ExecutorSet = caller

			snap := sess.Snapshot()
			rec := childstore.RecordFromSnapshot(snap)
			back := childstore.SessionFromRecord(rec)
			final := back.Snapshot()

			for hop, got := range map[string][]string{"snapshot": snap.ExecutorSet, "record": rec.ExecutorSet, "rebuilt": final.ExecutorSet} {
				c.EqDeep(tc.set, got, hop)
				c.Eq(tc.set == nil, got == nil, "%s: nil-ness changed", hop)
			}

			if len(caller) == 0 {
				return
			}
			caller[0] = "mutated"
			c.EqDeep(tc.set, snap.ExecutorSet, "snapshot aliases the caller's slice")
			snap.ExecutorSet[0] = "mutated-snap"
			c.EqDeep(tc.set, rec.ExecutorSet, "record aliases the snapshot's slice")
			rec.ExecutorSet[0] = "mutated-rec"
			c.EqDeep(tc.set, final.ExecutorSet, "rebuilt session aliases the record's slice")
		})
	}
}
