package main

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeProjectContextFetcher implements the narrow projectContextFetcher
// interface the daemon's fetch consults.
type fakeProjectContextFetcher struct {
	content string
	err     error
}

func (f fakeProjectContextFetcher) ProjectContext(context.Context) (string, error) {
	return f.content, f.err
}

func TestFetchProjectContext(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := fetchProjectContext(context.Background(), fakeProjectContextFetcher{content: "EXECUTOR_MARKER"})
	c.Require().NoError(err, "fetchProjectContext")
	c.Eq("EXECUTOR_MARKER", got, "got")

	if _, err := fetchProjectContext(context.Background(), fakeProjectContextFetcher{err: errors.New("boom")}); err == nil {
		t.Error("a fetch error was swallowed")
	}

	// A client that is not a projectContextFetcher is not an error: the empty
	// string is still passed down as a non-nil pointer, which is what keeps the
	// daemon from falling back to its own cwd.
	got, err = fetchProjectContext(context.Background(), struct{}{})
	c.False(err != nil || got != "", "non-fetcher: got (%q, %v), want (\"\", nil)", got, err)
	got, err = fetchProjectContext(context.Background(), nil)
	c.False(err != nil || got != "", "nil: got (%q, %v), want (\"\", nil)", got, err)
}

// TestAgentRuntimeOptionsNoExecutorLeavesProjectContextNil pins the negative
// path: a child with no executor must load context files exactly as it does
// today. That is the path every existing user is on, and the one this refactor
// is most likely to break silently — a child that quietly loses its CLAUDE.md
// looks like a model getting worse, not like a bug.
func TestAgentRuntimeOptionsNoExecutorLeavesProjectContextNil(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}
	ro, err := c.agentRuntimeOptions(req, "c_noexec", false, "", "")
	ck.Require().NoError(err, "agentRuntimeOptions")
	ck.Nil(ro.Executor, "Executor should be nil for a child with no executor")
	if ro.ProjectContext != nil {
		t.Errorf("ProjectContext = %v, want nil for a child with no executor", *ro.ProjectContext)
	}
}
