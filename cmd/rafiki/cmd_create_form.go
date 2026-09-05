// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui"
)

// shapingFlags are the flags that DECIDE what child gets spawned. Passing any
// of them is a statement of intent, so create honours it rather than asking
// again in a form.
//
// Deliberately not every flag: --kill-on-exit says what happens when the TUI
// quits, not what to spawn, so it does not suppress the form. The NAME is not
// here either -- it is a positional, and len(args) covers it.
var shapingFlags = []string{
	"model", "kind", "cwd", "preset", "detached",
	"executor-selector", "session", "fork", "append-system-prompt", "thinking",
}

// wantsCreateForm decides whether `rafiki create` should ask.
//
// The rule is "nothing to go on", not "no arguments": bare create is the fast
// path to an agent and passing --model or a name already says what you want,
// so only a genuinely empty invocation opens the form. -i forces it either
// way, which is how you get the form WITH a prefill.
//
// A form cannot render into a pipe, so a non-TTY always spawns directly --
// otherwise `rafiki create` in a script would hang on a screen nobody sees.
// isTTY is a parameter rather than a call to isStdinTTY, so the rule itself is
// testable: `go test` has no terminal, so an internal check would make every
// case that expects a form SKIP -- and a suite that silently skips looks
// exactly like one that passes.
func wantsCreateForm(cmd *cobra.Command, args []string, isTTY bool) bool {
	if !isTTY {
		return false
	}
	if forced, _ := cmd.Flags().GetBool("interactive"); forced {
		return true
	}
	if len(args) > 0 {
		return false
	}
	for _, name := range shapingFlags {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			return false
		}
	}
	return true
}

// runCreateForm opens the cockpit on its create form, prefilled from the
// request `create` had already assembled -- so the defaults, the preset and
// any environment fallbacks are all visible and editable rather than silently
// applied.
//
// The form spawns through the Connect plane and lands on the new child, which
// is exactly what create does after a flag-driven spawn -- including standing
// up a local session executor first. That step used to exist only on the
// flag-driven branch: every child created from a bare `rafiki create` (the
// default, TTY, no-shaping-flags case) went out with no executor selector at
// all, silently losing the whole workspace tool tier (read/write/edit/bash/
// etc.) with no error and no "unbound" label to explain why. c and
// noLocalExecutor let this path apply the same policy runCreate's flag branch
// does, rather than skipping it.
func runCreateForm(cmd *cobra.Command, c *client.Client, req protocol.SpawnRequest, noLocalExecutor bool) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ring, restoreLogging, err := installTUILogging(500)
	if err != nil {
		return err
	}
	defer restoreLogging()

	// Same policy as runCreate's flag-driven branch: offer this machine as the
	// workspace unless the caller already named an executor or opted out. One
	// session executor covers every spawn issued from this cockpit for its
	// whole lifetime (below), matching runCreate's undetached case -- the TUI
	// only returns when the user quits, so a deferred stop here never outlives
	// the children it was serving.
	executorSelector := req.ExecutorSelector
	if executorSelector == "" && !noLocalExecutor {
		selector, stop, err := startSessionExecutor(cmdCtx(cmd), c, req.Cwd, mustProfile(cmd))
		if err != nil {
			return fmt.Errorf("this machine could not join as a workspace: %w", err)
		}
		executorSelector = selector
		defer stop()
	}

	m := tui.NewCockpit(tui.Options{
		HTTPClient:       ep.httpClient,
		BaseURL:          ep.baseURL,
		OpenCreate:       true,
		ExecutorSelector: executorSelector,
		CreateDefaults: tui.SpawnDefaults{
			Name: req.Name,
			Kind: req.Kind,
			// req.Model already carries the whole precedence chain, including
			// the remembered model -- see buildSpawnRequest.
			Model: req.Model,
			Cwd:   req.Cwd,
		},
	})
	if _, runErr := tea.NewProgram(m).Run(); runErr != nil {
		ring.Dump()
		return fmt.Errorf("tui: %w", runErr)
	}
	// The child set changed if anything was created, so a cached TAB answer is
	// stale either way.
	dropChildCompletionCache(cmd)
	return nil
}
