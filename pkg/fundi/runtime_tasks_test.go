package fundi

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// buildRuntimeWithTasks builds a runtime from fake options carrying store and
// returns the engine whose tools the model calls.
func buildRuntimeWithTasks(t *testing.T, store tasks.Store) *Engine {
	t.Helper()
	c := assert.NewAborting(t)
	opts := fakeRuntimeOptions(t, t.TempDir())
	opts.Tasks = store
	eng, shutdown, err := BuildRuntime(context.Background(), NewFrontend(strings.NewReader(""), io.Discard, nil), opts)
	c.NoError(err, "BuildRuntime")
	t.Cleanup(shutdown)
	return eng
}

// addTaskThrough runs task_add the way a turn does: through the built tool
// registry, which injects the engine's own conversation id.
func addTaskThrough(t *testing.T, eng *Engine, content string) {
	t.Helper()
	c := assert.NewAborting(t)
	input, err := json.Marshal(map[string]any{
		"items": []map[string]any{{"content": content, "active_form": "doing " + content}},
	})
	c.NoError(err, "marshal task_add input")
	_, err = eng.tools.Execute(context.Background(), "task_add", input)
	c.NoError(err, "task_add through the built registry")
}

// everyRow reads a store across conversations: the engine's conversation id is
// its own, so filtering by one would test the wrong thing.
func everyRow(t *testing.T, store tasks.Store) []tasks.Task {
	t.Helper()
	rows, err := store.List(context.Background(), tasks.ListFilter{})
	assert.NewAborting(t).NoError(err, "List")
	return rows
}

// TestRuntimeTasksOverride pins RuntimeOptions.Tasks as the ledger the built
// tools receive: a supplied store takes the row, and a runtime built with no
// Tasks keeps a ledger of its own rather than reaching into the supplied one.
func TestRuntimeTasksOverride(t *testing.T) {
	c := assert.NewAborting(t)
	supplied := tasks.NewMemoryStore()

	addTaskThrough(t, buildRuntimeWithTasks(t, supplied), "wire the ledger")

	rows := everyRow(t, supplied)
	c.Require().Len(rows, 1, "the supplied store did not receive the row; the built tools got another ledger")
	c.Eq("wire the ledger", rows[0].Content, "content stored in the supplied ledger")

	addTaskThrough(t, buildRuntimeWithTasks(t, nil), "stay in the default ledger")

	rows = everyRow(t, supplied)
	c.Len(rows, 1, "a runtime with no Tasks wrote into a store it was never given")
}
