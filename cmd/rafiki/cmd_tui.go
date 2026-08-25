// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/tui"
)

func newTUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tui <childId>",
		Short: "Open the rafiki TUI for a fundi child",
		Long: `Open a terminal UI session for an existing fundi child.

The TUI streams the conversation live over the Connect control plane.
It renders markdown, tool calls, and thinking blocks. Use Enter to send
a prompt; Ctrl+C or Ctrl+D to quit.

The child keeps running after you quit — reattach any time.`,
		Args: cobra.ExactArgs(1),
		RunE: runTUI,
	}
	return cmd
}

func runTUI(cmd *cobra.Command, args []string) error {
	base := paths.Get(paths.URL)
	if base == "" {
		base = defaultControlURL
	}

	httpClient := http.DefaultClient

	// Resolve the auth token.
	tok := paths.TokenFromEnv()

	// Wrap the default transport to inject Authorization on every request,
	// since bubbletea's model sends requests from background goroutines
	// rather than from the cobra command context.
	if tok != "" {
		transport := http.DefaultTransport
		httpClient = &http.Client{
			Transport: &tokenTransport{
				base: transport,
				auth: "Bearer " + tok,
			},
		}
	}

	m := tui.NewModel(tui.Options{
		HTTPClient: httpClient,
		BaseURL:    base,
		ChildID:    args[0],
	})

	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// tokenTransport injects an Authorization header on every request.
type tokenTransport struct {
	base http.RoundTripper
	auth string
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", t.auth)
	return t.base.RoundTrip(r)
}
