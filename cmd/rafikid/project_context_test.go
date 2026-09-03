package main

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
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
	got, err := fetchProjectContext(context.Background(), fakeProjectContextFetcher{content: "EXECUTOR_MARKER"})
	if err != nil {
		t.Fatalf("fetchProjectContext: %v", err)
	}
	if got != "EXECUTOR_MARKER" {
		t.Errorf("got %q, want EXECUTOR_MARKER", got)
	}

	if _, err := fetchProjectContext(context.Background(), fakeProjectContextFetcher{err: errors.New("boom")}); err == nil {
		t.Error("a fetch error was swallowed")
	}

	// A client that is not a projectContextFetcher is not an error: the empty
	// string is still passed down as a non-nil pointer, which is what keeps the
	// daemon from falling back to its own cwd.
	got, err = fetchProjectContext(context.Background(), struct{}{})
	if err != nil || got != "" {
		t.Errorf("non-fetcher: got (%q, %v), want (\"\", nil)", got, err)
	}
	got, err = fetchProjectContext(context.Background(), nil)
	if err != nil || got != "" {
		t.Errorf("nil: got (%q, %v), want (\"\", nil)", got, err)
	}
}

// TestAgentRuntimeOptionsNoExecutorLeavesProjectContextNil pins the negative
// path: a child with no executor must load context files exactly as it does
// today. That is the path every existing user is on, and the one this refactor
// is most likely to break silently — a child that quietly loses its CLAUDE.md
// looks like a model getting worse, not like a bug.
func TestAgentRuntimeOptionsNoExecutorLeavesProjectContextNil(t *testing.T) {
	c := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:  protocol.KindFundi,
		Cwd:   t.TempDir(),
		Model: "anthropic/claude-sonnet-4-5",
	}
	ro, err := c.agentRuntimeOptions(req, "c_noexec", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.Executor != nil {
		t.Error("Executor should be nil for a child with no executor")
	}
	if ro.ProjectContext != nil {
		t.Errorf("ProjectContext = %v, want nil for a child with no executor", *ro.ProjectContext)
	}
}
