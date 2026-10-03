// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

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
	cmd := &cobra.Command{
		Use:   "refresh [name...]",
		Short: "Re-pull git-backed sources on every eligible executor (all of them when no name is given)",
		Long: `Re-pull git-backed pymodule sources on every eligible executor.

With no name, every source the caller has registered is refreshed, in list
order; naming sources refreshes just those. Each source is its own Connect
call, so a source that fails fails only its own line — the others still
refresh — and the command exits non-zero if any did.`,
		Args: cobra.ArbitraryArgs,
		RunE: runPythonRepoRefresh,
	}
	cmd.ValidArgsFunction = completePymoduleGitSourceNames
	return cmd
}

// runPythonRepoRefresh refreshes the named sources, or every registered source
// when none is named.
func runPythonRepoRefresh(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	// Resolve the mode before the first refresh: -j and -J together is a
	// user-input error and must not re-pull anything first (the rule close
	// applies to --all-exited).
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ctx := cmdCtx(cmd)
	names := args
	if len(names) == 0 {
		listed, err := ep.control().ListPymoduleGitSources(ctx,
			connect.NewRequest(&rafikiv1.ListPymoduleGitSourcesRequest{}))
		if err != nil {
			return err
		}
		for _, row := range listed.Msg.GetRows() {
			names = append(names, row.GetName())
		}
	}
	var (
		refreshed []*rafikiv1.RefreshPymoduleGitSourceResponse
		failures  int
	)
	for _, name := range names {
		resp, err := ep.control().RefreshPymoduleGitSource(ctx,
			connect.NewRequest(&rafikiv1.RefreshPymoduleGitSourceRequest{Name: name}))
		if err != nil {
			// Per-source, to stderr, exactly as `close`/`stop` report a failed
			// target: the other sources still refresh.
			fmt.Fprintf(os.Stderr, "error: refresh %s: %v\n", name, err)
			failures++
			continue
		}
		refreshed = append(refreshed, resp.Msg)
	}
	if err := emitGitSourceRefresh(os.Stdout, refreshed, mode); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d source(s) failed", failures)
	}
	return nil
}

// completePymoduleGitSourceNames completes the <name> arguments of
// `python repo refresh`, which takes any number of them (none means every
// source). It never exits, never prints, and cannot block past
// completionDeadline: every failure degrades to "no candidates". A name already
// given is still offered — cobra has no notion of "already used", and the same
// shape completeChildren offers every child at every position.
func completePymoduleGitSourceNames(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmdCtx(cmd), completionDeadline)
	defer cancel()
	resp, err := ep.control().ListPymoduleGitSources(ctx,
		connect.NewRequest(&rafikiv1.ListPymoduleGitSourcesRequest{}))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(resp.Msg.GetRows()))
	for _, row := range resp.Msg.GetRows() {
		if strings.HasPrefix(row.GetName(), toComplete) {
			names = append(names, row.GetName())
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// emitGitSourceRefresh writes a refresh run's result in the resolved mode: one
// `refreshed <name>: …` line per source in table mode, the canonical protojson
// of the responses as one {"rows": […]} envelope in -j, one compact response
// per line in -J. Every response carries its own name, so a multi-source run's
// rows attribute themselves. A run with nothing to refresh says so in table
// mode and emits an empty row set in JSON.
func emitGitSourceRefresh(w io.Writer, resps []*rafikiv1.RefreshPymoduleGitSourceResponse, mode outputMode) error {
	if mode == outputJSON || mode == outputJSONL {
		return emitProtoRows(w, resps, mode)
	}
	if len(resps) == 0 {
		_, err := fmt.Fprintln(w, "no git sources to refresh")
		return err
	}
	for _, r := range resps {
		if err := emitGitSourceSummary(w, fmt.Sprintf("refreshed %s", r.GetName()), r, mode); err != nil {
			return err
		}
	}
	return nil
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
