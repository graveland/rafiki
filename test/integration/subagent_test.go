package integration_test

import (
	"encoding/json"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestSubagentLineagePersistence verifies that spawns with parentChildId
// correctly record the lineage and that it persists through store reads.
func TestSubagentLineagePersistence(t *testing.T) {
	d := bootDaemon(t)

	// Spawn two top-level siblings.
	topA := d.spawnChild(t)
	topB := d.spawnChild(t)

	// Spawn a child under topA with ParentChildID set.
	raw := d.request(t, mustMarshal(t, map[string]interface{}{
		"type":          "ctrl_spawn",
		"id":            "sp2",
		"cwd":           "/tmp",
		"noSession":     true,
		"parentChildId": topA,
	}))
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("spawn under parent failed: %+v", r.Error)
	}
	var data protocol.SpawnResponseData
	mustUnmarshal(t, r.Data, &data)

	// Verify the child appears in the list with its parent.
	listRaw := d.request(t, `{"type":"ctrl_list","id":"list"}`)
	var listR protocol.Response
	mustUnmarshal(t, listRaw, &listR)
	if !listR.Success {
		t.Fatalf("ctrl_list failed: %+v", listR.Error)
	}
	var listData protocol.ListResponseData
	mustUnmarshal(t, listR.Data, &listData)

	found := make(map[string]bool)
	for _, s := range listData.Children {
		found[s.ChildID] = true
	}
	for _, want := range []string{topA, topB, data.ChildID} {
		if !found[want] {
			t.Errorf("child %s not found in list: %v", want, found)
		}
	}

	// Verify topA's child has the parent label set.
	statusRaw := d.request(t, mustMarshal(t, map[string]interface{}{
		"type":    "ctrl_get",
		"id":      "get",
		"childId": data.ChildID,
	}))
	var statusR protocol.Response
	mustUnmarshal(t, statusRaw, &statusR)
	if !statusR.Success {
		t.Fatalf("ctrl_get failed: %+v", statusR.Error)
	}
	var snap protocol.ChildSummary
	mustUnmarshal(t, statusR.Data, &snap)

	parent, hasParent := snap.Labels["rafiki/parent"]
	if !hasParent || parent != topA {
		t.Errorf("want rafiki/parent=%s, got %s (labels: %v)", topA, parent, snap.Labels)
	}

	// Kill topA and its children.
	killRaw := d.request(t, mustMarshal(t, map[string]interface{}{
		"type":    "ctrl_kill",
		"id":      "kill",
		"childId": topA,
	}))
	var killR protocol.Response
	mustUnmarshal(t, killRaw, &killR)
	if !killR.Success {
		t.Fatalf("ctrl_kill failed: %+v", killR.Error)
	}

	// Wait for the child to exit too.
	time.Sleep(500 * time.Millisecond)

	// Clean up sibling.
	_ = d.request(t, mustMarshal(t, map[string]interface{}{
		"type":    "ctrl_kill",
		"id":      "killB",
		"childId": topB,
	}))
}

func mustMarshal(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
