// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// createExecutorSeam stubs the three package-var seams the missing-executor
// prompt resolves through — the interactivity check, the setup wizard, and the
// executor lister — so resolveCreateExecutor can be driven without a terminal,
// a daemon, or a real `rafiki executor setup`. install restores the production
// functions on cleanup.
type createExecutorSeam struct {
	interactive bool
	// results are returned by successive resolve calls; a call past the end
	// reuses the last element, and no results means always "".
	results []string

	setupCalls   int
	resolveCalls int
}

func (s *createExecutorSeam) install(t *testing.T) {
	t.Helper()
	origSetup, origInteractive, origResolve := createRunExecutorSetup, createIsInteractive, createResolveLaunchExecutor
	t.Cleanup(func() {
		createRunExecutorSetup, createIsInteractive, createResolveLaunchExecutor = origSetup, origInteractive, origResolve
	})

	createIsInteractive = func(*cobra.Command) bool { return s.interactive }
	createRunExecutorSetup = func(context.Context, *wizard, io.Writer) error {
		s.setupCalls++
		return nil
	}
	createResolveLaunchExecutor = func(context.Context, *cobra.Command, string, string) (string, error) {
		i := s.resolveCalls
		s.resolveCalls++
		if len(s.results) == 0 {
			return "", nil
		}
		if i >= len(s.results) {
			i = len(s.results) - 1
		}
		return s.results[i], nil
	}
}

// TestCreateMissingExecutorNonInteractiveNamesCommand pins the headless half of
// the prompt: with no one to answer, a claude spawn that no executor can launch
// must fail with an error naming `rafiki executor setup`, and must not run the
// interactive wizard.
func TestCreateMissingExecutorNonInteractiveNamesCommand(t *testing.T) {
	c := assert.NewCollecting(t)
	seam := &createExecutorSeam{interactive: false}
	seam.install(t)

	_, err := resolveCreateExecutor(context.Background(), newTestCreateCmd(),
		"p", protocol.KindClaude, "", "", io.Discard)
	c.Require().Error(err, "want a refusal naming the fix, got nil")
	c.StrContains(err.Error(), "rafiki executor setup", "err = %v, want it to name `rafiki executor setup`", err)
	c.Eq(0, seam.setupCalls, "the setup wizard must not run with no one to answer")
}

// TestCreateMissingExecutorInteractiveRunsSetupAndRetries pins the interactive
// half: when nothing can launch a claude child and the caller accepts the
// offer, the wizard runs exactly once and the executor is re-resolved, with the
// ref from that second resolve returned to the caller.
func TestCreateMissingExecutorInteractiveRunsSetupAndRetries(t *testing.T) {
	c := assert.NewCollecting(t)
	seam := &createExecutorSeam{interactive: true, results: []string{"", "greyshift"}}
	seam.install(t)

	ref, err := resolveCreateExecutor(context.Background(), newTestCreateCmd(),
		"p", protocol.KindClaude, "", "", io.Discard)
	c.Require().NoError(err, "resolveCreateExecutor")
	c.Eq(1, seam.setupCalls, "setup runs exactly once")
	c.Eq(2, seam.resolveCalls, "resolve happens once before the prompt and once after")
	c.Eq("greyshift", ref, "the ref from the second resolve is the one returned")
}

// TestCreateMissingExecutorFundiUnaffected pins the kind gate: a fundi spawn
// never consults the executor lister and never offers the setup wizard — it is
// served by the session-executor fallback in runCreate, not by this pre-flight.
func TestCreateMissingExecutorFundiUnaffected(t *testing.T) {
	c := assert.NewCollecting(t)
	seam := &createExecutorSeam{interactive: true, results: []string{"greyshift"}}
	seam.install(t)

	ref, err := resolveCreateExecutor(context.Background(), newTestCreateCmd(),
		"p", protocol.KindFundi, "", "", io.Discard)
	c.Require().NoError(err, "resolveCreateExecutor")
	c.Eq("", ref, "fundi needs no launch-executor resolution here")
	c.Eq(0, seam.resolveCalls, "the lister is never consulted for a fundi spawn")
	c.Eq(0, seam.setupCalls, "the setup wizard never runs for a fundi spawn")

	// A caller who named an executor has already answered the question too:
	// the ref passes straight through, untouched.
	ref, err = resolveCreateExecutor(context.Background(), newTestCreateCmd(),
		"p", protocol.KindClaude, "greyshift", "", io.Discard)
	c.Require().NoError(err, "resolveCreateExecutor with --executor")
	c.Eq("greyshift", ref, "a named --executor passes through")
	c.Eq(0, seam.resolveCalls, "the lister is not consulted when --executor was given")
}
