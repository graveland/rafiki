// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/quotafmt"
)

// runClaudeLimits serves `rafiki claude --limits`: print the caller's
// captured Anthropic subscription rate-limit status and exit, launching no
// session. Goes over the Connect control plane like every other read-only
// verb (rafiki attach, rafiki history) — not the proxy's own credential,
// which is a different mechanism entirely (see resolveClaudeToken).
func runClaudeLimits(cmd *cobra.Command) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().GetRateLimitStatus(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.GetRateLimitStatusRequest{}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			fmt.Fprintln(cmd.OutOrStdout(),
				"No Anthropic subscription usage has been captured yet. "+
					"It's recorded the first time a `rafiki claude` session bills your\n"+
					"subscription (the default for Anthropic models; see --passthrough-auth).")
			return nil
		}
		return diagnoseConnectError(err, ep.describe)
	}
	renderRateLimitStatus(cmd.OutOrStdout(), resp.Msg)
	return nil
}

// renderRateLimitStatus writes st as plain text. Separated from the command
// so it can be tested without a server.
func renderRateLimitStatus(w io.Writer, st *rafikiv1.GetRateLimitStatusResponse) {
	updated := time.Unix(st.GetUpdatedAt(), 0)
	fmt.Fprintf(w, "Anthropic subscription rate limits (org %s, updated %s ago)\n",
		orDash(st.GetOrganizationId()), time.Since(updated).Round(time.Second))
	renderRateLimitWindow(w, "5h ", st.GetFiveH())
	renderRateLimitWindow(w, "7d ", st.GetSevenD())
	fmt.Fprintf(w, "  overall: %s\n", orDash(st.GetOverallStatus()))
}

// renderRateLimitWindow formats one window.
func renderRateLimitWindow(w io.Writer, label string, win *rafikiv1.RateLimitWindow) {
	util := quotafmt.Utilization(win.Utilization)
	reset := "—"
	if win.ResetAt != nil {
		reset = time.Unix(win.GetResetAt(), 0).Format(time.RFC3339)
	}
	fmt.Fprintf(w, "  %s: %s used, resets %s, status %s\n", label, util, reset, orDash(win.GetStatus()))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
