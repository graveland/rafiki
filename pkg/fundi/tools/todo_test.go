package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func testTodoTool(t *testing.T) *todoTool {
	t.Helper()
	tt, err := (&TodoBlueprint{}).Materialize(ToolOpts{})
	if err != nil {
		t.Fatal(err)
	}
	return tt.(*todoTool)
}

func TestTodoRoundTrip(t *testing.T) {
	tt := testTodoTool(t)
	res, err := tt.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "Run the tests", "status": "in_progress", "active_form": "Running the tests"},
			{"content": "Write the code", "status": "pending", "active_form": "Writing the code"},
			{"content": "Commit", "status": "pending", "active_form": "Committing"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "Run the tests") || !strings.Contains(out, "Write the code") || !strings.Contains(out, "Commit") {
		t.Fatalf("expected all three todos in output, got %q", out)
	}
	if !strings.Contains(out, "in_progress") || !strings.Contains(out, "pending") {
		t.Fatalf("expected statuses in output, got %q", out)
	}
	if !strings.Contains(out, "1") {
		t.Fatalf("expected count in output, got %q", out)
	}
}

func TestTodoWholeListReplacement(t *testing.T) {
	tt := testTodoTool(t)

	// First call sets three items.
	_, err := tt.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "A", "status": "pending", "active_form": "Doing A"},
			{"content": "B", "status": "pending", "active_form": "Doing B"},
			{"content": "C", "status": "pending", "active_form": "Doing C"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	// Second call with only one item replaces the entire list.
	res, err := tt.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "X", "status": "completed", "active_form": "Done X"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "X") {
		t.Fatalf("expected X in output, got %q", out)
	}
	if strings.Contains(out, "A") || strings.Contains(out, "B") || strings.Contains(out, "C") {
		t.Fatalf("replaced list should NOT contain old items, got %q", out)
	}
}

func TestTodoInvalidStatusRejected(t *testing.T) {
	tt := testTodoTool(t)
	_, err := tt.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "Oops", "status": "nonsense", "active_form": "Oopsing"}
		]
	}`))
	if err == nil {
		t.Fatal("expected error for invalid status")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Fatalf("error should mention status, got %v", err)
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("error should mention the invalid value, got %v", err)
	}
}

func TestTodoIsolation(t *testing.T) {
	tt1 := testTodoTool(t)
	tt2 := testTodoTool(t)

	_, err := tt1.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "From tool 1", "status": "pending", "active_form": "Doing tool 1"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	_, err = tt2.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "From tool 2", "status": "in_progress", "active_form": "Doing tool 2"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	// Read back from tool 1 — should NOT see tool 2's items.
	res, err := tt1.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "From tool 1", "status": "completed", "active_form": "Done tool 1"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if strings.Contains(out, "From tool 2") {
		t.Fatalf("tool 1 should not see tool 2's items, got %q", out)
	}
	if !strings.Contains(out, "From tool 1") {
		t.Fatalf("tool 1 should see its own items, got %q", out)
	}

	// Read from tool 2 — should still see its original item.
	res2, err := tt2.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "From tool 2", "status": "in_progress", "active_form": "Doing tool 2"},
			{"content": "Extra", "status": "pending", "active_form": "Extra"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out2 := res2.Text
	if !strings.Contains(out2, "From tool 2") {
		t.Fatalf("tool 2 should still see its original item, got %q", out2)
	}
	if !strings.Contains(out2, "[1 pending, 1 in_progress, 0 completed]") {
		t.Fatalf("tool 2's item should remain in_progress (not changed by tool 1), got %q", out2)
	}
}

func TestTodoConcurrent(t *testing.T) {
	tt := testTodoTool(t)

	// Populate.
	_, err := tt.Execute(context.Background(), ToolInput(`{
		"todos": [
			{"content": "Item 0", "status": "pending", "active_form": ""}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = tt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{
				"todos": [
					{"content": "Item %d", "status": "pending", "active_form": ""}
				]
			}`, i)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
}
