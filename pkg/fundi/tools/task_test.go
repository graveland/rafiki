package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/tasks"
)

func newTaskTools(t *testing.T) (*Registry, context.Context) {
	t.Helper()
	reg := DefaultBlueprint.MaterializeAll(ToolOpts{
		Tasks:   tasks.NewMemoryStore(),
		ChildID: "c_self",
		Cwd:     t.TempDir(),
	})
	ctx := context.WithValue(context.Background(), ConversationIDKey{}, "conv-1")
	return reg, ctx
}

func TestTaskAddReturnsHandles(t *testing.T) {
	reg, ctx := newTaskTools(t)
	out, err := reg.Execute(ctx, "task_add", json.RawMessage(
		`{"items":[{"content":"one","active_form":"doing one"},{"content":"two","active_form":"doing two"}]}`))
	if err != nil {
		t.Fatalf("task_add: %v", err)
	}
	if !strings.Contains(out, "1 ") || !strings.Contains(out, "2 ") {
		t.Fatalf("result must echo handles; got:\n%s", out)
	}
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Fatalf("result must echo the full list; got:\n%s", out)
	}
}

func TestTaskUpdateTouchesOnlyNamedRows(t *testing.T) {
	reg, ctx := newTaskTools(t)
	if _, err := reg.Execute(ctx, "task_add", json.RawMessage(
		`{"items":[{"content":"one"},{"content":"two"}]}`)); err != nil {
		t.Fatal(err)
	}
	out, err := reg.Execute(ctx, "task_update", json.RawMessage(
		`{"changes":[{"handle":"1","status":"completed"}]}`))
	if err != nil {
		t.Fatalf("task_update: %v", err)
	}
	if !strings.Contains(out, "☑ one") {
		t.Errorf("task 1 should be completed; got:\n%s", out)
	}
	if !strings.Contains(out, "☐ two") {
		t.Errorf("task 2 must be untouched; got:\n%s", out)
	}
}

func TestTaskUpdateRejectsBadStatus(t *testing.T) {
	reg, ctx := newTaskTools(t)
	if _, err := reg.Execute(ctx, "task_add", json.RawMessage(`{"items":[{"content":"one"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, err := reg.Execute(ctx, "task_update", json.RawMessage(
		`{"changes":[{"handle":"1","status":"almost_done"}]}`))
	if err == nil {
		t.Fatal("an invalid status must be an error the model can see")
	}
	if !strings.Contains(err.Error(), "almost_done") {
		t.Errorf("error must name the offending value; got %v", err)
	}
}

func TestTaskDropRequiresReason(t *testing.T) {
	reg, ctx := newTaskTools(t)
	if _, err := reg.Execute(ctx, "task_add", json.RawMessage(`{"items":[{"content":"one"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Execute(ctx, "task_drop", json.RawMessage(`{"handle":"1"}`)); err == nil {
		t.Fatal("task_drop without a reason must fail")
	}
}

func TestTaskListEmptyIsNotAnError(t *testing.T) {
	reg, ctx := newTaskTools(t)
	out, err := reg.Execute(ctx, "task_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("task_list on an empty ledger must succeed: %v", err)
	}
	if !strings.Contains(out, "0 task(s)") {
		t.Fatalf("got:\n%s", out)
	}
}

// Isolation: two materialized registries must not share state. todo.go's
// comment records that this exact guarantee was once asserted by a test that
// could not fail, because the tool echoed its own input instead of reading
// stored state. Read through the store.
func TestTaskToolsAreIsolatedPerAgent(t *testing.T) {
	regA, ctxA := newTaskTools(t)
	regB, ctxB := newTaskTools(t)
	if _, err := regA.Execute(ctxA, "task_add", json.RawMessage(`{"items":[{"content":"only-A"}]}`)); err != nil {
		t.Fatal(err)
	}
	out, err := regB.Execute(ctxB, "task_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "only-A") {
		t.Fatal("agent B can see agent A's tasks")
	}
}
