package tasks_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/tasks/tasktest"
)

// The memory store's half of the shared contract. The Postgres half runs the
// same suite from pkg/tasksdb — the suite moved to pkg/tasks/tasktest so both
// can import it once the two implementations stopped sharing a package.
//
// A green run here says less than it looks: this store is atomic under its own
// mutex, so it passes concurrency cases that Postgres, under READ COMMITTED,
// does not. See the caveat on tasktest.RunConformance.
func TestMemoryStoreConformance(t *testing.T) {
	tasktest.RunConformance(t, func(*testing.T) (tasks.Store, string) {
		return tasks.NewMemoryStore(), "conv-1"
	})
}
