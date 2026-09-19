// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/table"
)

// newPythonRepoCmd is the `python repo` group: managing git-backed pymodule
// sources — the `(owner, name, url, ref)` registrations whose discovered
// scripts/packages children address as `repo=<name>` on the pymodule tools.
// Registration is a rare, setup-shaped operation (design §2), so it lives on
// the operator CLI over Connect, never on the MCP/fundi tool surface.
func newPythonRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage git-backed pymodule sources (add/list/refresh/remove)",
	}
	cmd.AddCommand(newPythonRepoAddCmd(), newPythonRepoListCmd(), newPythonRepoRefreshCmd(), newPythonRepoRemoveCmd())
	return cmd
}

// newPythonRepoAddCmd registers a source and reports the FIRST refresh's
// discovery. The registration itself goes through AddPymoduleGitSource —
// which the daemon answers only after its synchronous first refresh
// succeeded, so a bad clone fails the add outright — and the summary comes
// from a follow-up RefreshPymoduleGitSource, the one wire surface that
// carries the discovered inventory.
func newPythonRepoAddCmd() *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "add <name> <url>",
		Short: "Register a git-backed pymodule source and run its first refresh",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, url := args[0], args[1]
			// Fast, local rejection, mirroring `python put`'s name pre-check:
			// the daemon re-checks both rules, but a name that can never be a
			// `repo` value should fail before the round trip.
			if err := gitpymodules.ValidateName(name); err != nil {
				return err
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			added, err := ep.control().AddPymoduleGitSource(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.AddPymoduleGitSourceRequest{Name: name, Url: url, Ref: ref}))
			if err != nil {
				return err
			}
			row := added.Msg.GetRow()
			inv, err := ep.control().RefreshPymoduleGitSource(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RefreshPymoduleGitSourceRequest{Name: row.GetName()}))
			if err != nil {
				return err
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitGitSourceSummary(os.Stdout, fmt.Sprintf("registered %s (%s @ %s)", row.GetName(), row.GetUrl(), row.GetRef()), inv.Msg, mode)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "main", "git ref to track (branch, tag or commit)")
	return cmd
}

func newPythonRepoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your registered git-backed pymodule sources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListPymoduleGitSources(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListPymoduleGitSourcesRequest{}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitGitSourceList(os.Stdout, resp.Msg.GetRows(), mode, useColor)
		},
	}
}

func newPythonRepoRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh <name>",
		Short: "Re-pull one git-backed source on every eligible executor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().RefreshPymoduleGitSource(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RefreshPymoduleGitSourceRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitGitSourceSummary(os.Stdout, fmt.Sprintf("refreshed %s", args[0]), resp.Msg, mode)
		},
	}
}

func newPythonRepoRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Short:   "Remove a git-backed pymodule source registration",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			_, err = ep.control().RemovePymoduleGitSource(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RemovePymoduleGitSourceRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			fmt.Printf("removed %s\n", args[0])
			return nil
		},
	}
}

// emitGitSourceList writes list's output in the resolved mode, the same
// three-way contract every other list verb renders: a table, a {"rows": …}
// envelope, or one bare row per line.
func emitGitSourceList(w io.Writer, rows []*rafikiv1.GitSourceRow, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, map[string]any{"rows": rows})
	case outputJSONL:
		return writeJSONL(w, gitSourceJSONLRows(rows))
	default:
		return renderGitSourceList(w, rows, useColor)
	}
}

// renderGitSourceList renders the inventory table: NAME, URL, REF.
func renderGitSourceList(w io.Writer, rows []*rafikiv1.GitSourceRow, useColor bool) error {
	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "NAME", "URL", "REF")...)
	for _, r := range rows {
		tb.Row(r.GetName(), defaultDash(r.GetUrl()), defaultDash(r.GetRef()))
	}
	return tb.Render()
}

func gitSourceJSONLRows(rows []*rafikiv1.GitSourceRow) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

// emitGitSourceSummary writes the "what did discovery find" line both add and
// refresh print after the daemon's fan-out: the discovered scripts/packages
// counts, plus the venv error when the repo's one shared venv failed to
// build. A failed build does NOT fail the command — the refresh itself
// succeeded, and per the design a broken build only fails the runs that
// actually need the venv — so the failure is printed, visibly, on stdout.
func emitGitSourceSummary(w io.Writer, header string, resp *rafikiv1.RefreshPymoduleGitSourceResponse, mode outputMode) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, resp)
	case outputJSONL:
		// One compact record per line, the same -J contract every peer
		// emitter follows — never the indented -j shape.
		return writeJSONL(w, []any{resp})
	default:
		fmt.Fprintf(w, "%s: %d script(s), %d package(s)\n", header, len(resp.GetScripts()), len(resp.GetPackages()))
		if !resp.GetVenvReady() && resp.GetVenvError() != "" {
			fmt.Fprintf(w, "venv build failed: %s\n", resp.GetVenvError())
		}
		return nil
	}
}
