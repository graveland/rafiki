package tools

import "testing"

// testReadTool returns a materialized read tool for tests.
func testReadTool(t *testing.T, tr *FileTracker, cwd string) *readTool {
	t.Helper()
	rt, err := (&ReadBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return rt.(*readTool)
}

// testEditTool returns a materialized edit tool for tests.
func testEditTool(t *testing.T, tr *FileTracker, cwd string) *editTool {
	t.Helper()
	et, err := (&EditBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return et.(*editTool)
}

// testWriteTool returns a materialized write tool for tests.
func testWriteTool(t *testing.T, tr *FileTracker, cwd string) *writeTool {
	t.Helper()
	wt, err := (&WriteBlueprint{}).Materialize(ToolOpts{FileTracker: tr, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return wt.(*writeTool)
}
