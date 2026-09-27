package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewAborting(t)
	reg, ctx := newTaskTools(t)
	out, err := reg.Execute(ctx, "task_add", json.RawMessage(
		`{"items":[{"content":"one","active_form":"doing one"},{"content":"two","active_form":"doing two"}]}`))
	c.NoError(err, "task_add")
	c.False(!strings.Contains(out, "1 ") || !strings.Contains(out, "2 "), "result must echo handles; got:\n%s", out)
	c.False(!strings.Contains(out, "one") || !strings.Contains(out, "two"), "result must echo the full list; got:\n%s", out)
}

func TestTaskUpdateTouchesOnlyNamedRows(t *testing.T) {
	c := assert.NewCollecting(t)
	reg, ctx := newTaskTools(t)
	if _, err := reg.Execute(ctx, "task_add", json.RawMessage(
		`{"items":[{"content":"one"},{"content":"two"}]}`)); err != nil {
		t.Fatal(err)
	}
	out, err := reg.Execute(ctx, "task_update", json.RawMessage(
		`{"changes":[{"handle":"1","status":"completed"}]}`))
	c.Require().NoError(err, "task_update")
	c.StrContains(out, "☑ one", "task 1 should be completed; got:\n")
	c.StrContains(out, "☐ two", "task 2 must be untouched; got:\n")
}

func TestTaskUpdateRejectsBadStatus(t *testing.T) {
	c := assert.NewCollecting(t)
	reg, ctx := newTaskTools(t)
	if _, err := reg.Execute(ctx, "task_add", json.RawMessage(`{"items":[{"content":"one"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, err := reg.Execute(ctx, "task_update", json.RawMessage(
		`{"changes":[{"handle":"1","status":"almost_done"}]}`))
	c.Require().Error(err, "an invalid status must be an error the model can see")
	c.StrContains(err.Error(), "almost_done", "error must name the offending value; got %v", err)
}

func TestTaskDropRequiresReason(t *testing.T) {
	reg, ctx := newTaskTools(t)
	if _, err := reg.Execute(ctx, "task_add", json.RawMessage(`{"items":[{"content":"one"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, err := reg.Execute(ctx, "task_drop", json.RawMessage(`{"handle":"1"}`))
	assert.NewAborting(t).Error(err, "task_drop without a reason must fail")
}

func TestTaskListEmptyIsNotAnError(t *testing.T) {
	c := assert.NewAborting(t)
	reg, ctx := newTaskTools(t)
	out, err := reg.Execute(ctx, "task_list", json.RawMessage(`{}`))
	c.NoError(err, "task_list on an empty ledger must succeed")
	c.StrContains(out, "0 task(s)", "got:\n")
}

// Isolation: two materialized registries must not share state. todo.go's
// comment records that this exact guarantee was once asserted by a test that
// could not fail, because the tool echoed its own input instead of reading
// stored state. Read through the store.
func TestTaskToolsAreIsolatedPerAgent(t *testing.T) {
	c := assert.NewAborting(t)
	regA, ctxA := newTaskTools(t)
	regB, ctxB := newTaskTools(t)
	if _, err := regA.Execute(ctxA, "task_add", json.RawMessage(`{"items":[{"content":"only-A"}]}`)); err != nil {
		t.Fatal(err)
	}
	out, err := regB.Execute(ctxB, "task_list", json.RawMessage(`{}`))
	c.NoError(err)
	c.NotStrContains(out, "only-A", "agent B can see agent A's tasks")
}
