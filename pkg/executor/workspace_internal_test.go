package executor

import (
	"fmt"
	"sync"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
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

// The executor must not rewrite commands through rtk by accident.
//
// RTKMode is a string and rtkRewrite short-circuits only on the literal "off",
// so the ZERO value behaves like "auto". The registry was built with no RTK
// field at all, which meant every executor silently rewrote through rtk with
// nobody having chosen it — and a child configured for rtk off got it anyway,
// on a machine whose tooling it cannot see.
func TestTheExecutorsRTKModeIsAChoiceAndNotAZeroValue(t *testing.T) {
	if got := toolOptsFor(NewServer(Options{Root: "/x"}).opts, nil).RTK; got != tools.RTKAuto {
		t.Errorf("default RTK = %q; it must resolve to an explicit mode, not %q behaving like one",
			got, tools.RTKMode(""))
	}
	off := NewServer(Options{Root: "/x", RTK: tools.RTKOff}).opts
	if got := toolOptsFor(off, nil).RTK; got != tools.RTKOff {
		t.Errorf("RTK = %q; an operator who turned rtk off must actually get it off", got)
	}
}

// Oversized foreground results and background job output land in the same
// place, and it is a chosen place. OutputPolicy.SpillDir was unset, so foreground
// spills went to os.TempDir() by default rather than by decision — harmless
// until an operator needs them somewhere else.
func TestSpillDirIsResolvedAndShared(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Options{Root: "/x", SpillDir: dir})
	if got := toolOptsFor(s.opts, nil).OutputPolicy.SpillDir; got != dir {
		t.Errorf("foreground spill dir = %q, want %q", got, dir)
	}
	if s.jobs.spillDir != dir {
		t.Errorf("background job spill dir = %q, want %q", s.jobs.spillDir, dir)
	}
	if NewServer(Options{Root: "/x"}).opts.SpillDir == "" {
		t.Error("an unset spill dir must resolve to a real directory, not stay empty")
	}
}
