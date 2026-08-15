package executor

import (
	"fmt"
	"sync"
	"testing"
)

// Concurrent map read+write is a Go FATAL ERROR, not a recoverable panic: it
// kills the executor process and every child running on it. Provision, Release
// and Execute are each one goroutine per Connect request, and Execute reads the
// registry BEFORE it acquires s.sem — so the concurrency bound does not even
// serialise the reads.
func TestWorkspaceRegistryIsConcurrencySafe(t *testing.T) {
	r := newWorkspaceRegistry()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("ws-%d", i)
			r.put(&workspace{id: id})
			r.get(id)
			r.remove(id)
		}()
	}
	wg.Wait()
}
