// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/repos"
	"go.graveland.dev/rafiki/pkg/table"
)

// newRepoCmd is the `repo` group: managing repos — the `(owner, name, url,
// ref)` registrations whose discovered scripts/packages children address as
// `repo=<name>` on the pymodule tools, plus the opt-in refresh schedule and
// the skills the checkout ships.
// Registration is a rare, setup-shaped operation (design §2), so it lives on
// the operator CLI over Connect, never on the MCP/fundi tool surface.
func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage repos (add/list/refresh/set/remove)",
	}
	cmd.AddCommand(newRepoAddCmd(), newRepoListCmd(), newRepoRefreshCmd(), newRepoSetCmd(), newRepoRemoveCmd())
	return cmd
}

// repoIntervalFlag parses a --refresh value. "off" (or an empty value) is on
// demand — the default, and the only way to clear a schedule from the CLI —
// and anything else is a duration at or above the one-minute floor, refused
// here so the operator gets the rule without a round trip.
func repoIntervalFlag(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "off" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("--refresh %q: %w (or \"off\" for on demand)", value, err)
	}
	if d < repos.MinRefreshInterval {
		return 0, repos.ErrIntervalTooShort
	}
	return d, nil
}

// newRepoAddCmd registers a repo and reports the FIRST refresh's
// discovery. The registration itself goes through AddRepo —
// which the daemon answers only after its synchronous first refresh
// succeeded, so a bad clone fails the add outright — and the script/package
// counts come from a follow-up RefreshRepo, the one wire surface that carries
// the discovered inventory. The commit, the unchanged flag, the skill count
// and the skills half's errors come from the registration's own refresh.
func newRepoAddCmd() *cobra.Command {
	var ref, refresh string
	cmd := &cobra.Command{
		Use:   "add <name> <url>",
		Short: "Register a repo and run its first refresh",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, url := args[0], args[1]
			// Fast, local rejection, mirroring `python put`'s name pre-check:
			// the daemon re-checks both rules, but a name that can never be a
			// `repo` value should fail before the round trip.
			if err := repos.ValidateName(name); err != nil {
				return err
			}
			interval, err := repoIntervalFlag(refresh)
			if err != nil {
				return err
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			addReq := &rafikiv1.AddRepoRequest{Name: name, Url: url, Ref: ref}
			if interval > 0 {
				addReq.RefreshInterval = durationpb.New(interval)
			}
			added, err := ep.control().AddRepo(cmdCtx(cmd), connect.NewRequest(addReq))
			if err != nil {
				return err
			}
			row := added.Msg.GetRow()
			inv, err := ep.control().RefreshRepo(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RefreshRepoRequest{Name: row.GetName()}))
			if err != nil {
				return err
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			if err := emitRepoAdded(os.Stdout, added.Msg, inv.Msg, mode); err != nil {
				return err
			}
			// A skill error did not fail the registration — only a namespace
			// collision does — but the command still exits non-zero: the repo
			// is registered and its scripts work, while the skills it ships
			// did not land, and a silent success would hide that.
			if n := len(added.Msg.GetSkillErrors()); n > 0 {
				return fmt.Errorf("%d skill error(s)", n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "main", "git ref to track (branch, tag or commit)")
	cmd.Flags().StringVar(&refresh, "refresh", "", "refresh interval (e.g. 15m), or \"off\" for on demand (the default)")
	return cmd
}

// newRepoSetCmd changes a registration in place. Both halves are optional and
// independent: --ref repoints, --refresh schedules or clears the schedule, and
// naming neither is a usage error rather than a no-op round trip.
func newRepoSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change a repo's ref and/or its refresh interval",
		Long: `Change a repo's ref and/or its refresh interval.

Both halves are optional and independent: --ref repoints the source from now
on, --refresh arms (or, with "off", clears) its periodic refresh. Naming
neither is an error. Neither triggers a refresh — ` + "`rafiki repo refresh <name>`" + `
does that now, and the schedule does it at the next fire.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := repos.ValidateName(name); err != nil {
				return err
			}
			req := &rafikiv1.SetRepoRequest{Name: name}
			if cmd.Flags().Changed("ref") {
				ref, err := cmd.Flags().GetString("ref")
				if err != nil {
					return err
				}
				req.Ref = &ref
			}
			if cmd.Flags().Changed("refresh") {
				raw, err := cmd.Flags().GetString("refresh")
				if err != nil {
					return err
				}
				interval, err := repoIntervalFlag(raw)
				if err != nil {
					return err
				}
				req.RefreshInterval = durationpb.New(interval)
			}
			if req.Ref == nil && req.RefreshInterval == nil {
				return errors.New("nothing to change: pass --ref and/or --refresh")
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().SetRepo(cmdCtx(cmd), connect.NewRequest(req))
			if err != nil {
				return err
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitRepoSet(os.Stdout, resp.Msg.GetRow(), mode)
		},
	}
	cmd.Flags().String("ref", "", "git ref to track from now on")
	cmd.Flags().String("refresh", "", "refresh interval (e.g. 15m), or \"off\" for on demand")
	cmd.ValidArgsFunction = completeRepoNames
	return cmd
}

func newRepoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your registered repos",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListRepos(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListReposRequest{}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitRepoList(os.Stdout, resp.Msg.GetRows(), mode, useColor)
		},
	}
}

func newRepoRefreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh [name...]",
		Short: "Re-pull repos on every eligible executor (all of them when no name is given)",
		Long: `Re-pull repos on every eligible executor.

With no name, every repo the caller has registered is refreshed, in list
order; naming repos refreshes just those. Each source is its own Connect
call, so a source that fails fails only its own line — the others still
refresh — and the command exits non-zero if any did. A skill that failed to
load is reported as a skill error on stderr and also exits non-zero: the
scripts refreshed, but the skills the repo ships did not.`,
		Args: cobra.ArbitraryArgs,
		RunE: runRepoRefresh,
	}
	cmd.ValidArgsFunction = completeRepoNames
	return cmd
}

// runRepoRefresh refreshes the named sources, or every registered source
// when none is named.
func runRepoRefresh(cmd *cobra.Command, args []string) error {
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
		listed, err := ep.control().ListRepos(ctx,
			connect.NewRequest(&rafikiv1.ListReposRequest{}))
		if err != nil {
			return err
		}
		for _, row := range listed.Msg.GetRows() {
			names = append(names, row.GetName())
		}
	}
	var (
		refreshed []*rafikiv1.RefreshRepoResponse
		failures  int
		skillErrs int
	)
	for _, name := range names {
		resp, err := ep.control().RefreshRepo(ctx,
			connect.NewRequest(&rafikiv1.RefreshRepoRequest{Name: name}))
		if err != nil {
			// Per-source, to stderr, exactly as `close`/`stop` report a failed
			// target: the other sources still refresh.
			fmt.Fprintf(os.Stderr, "error: refresh %s: %v\n", name, err)
			failures++
			continue
		}
		skillErrs += emitRepoSkillErrors(resp.Msg.GetSkillErrors())
		refreshed = append(refreshed, resp.Msg)
	}
	if err := emitRepoRefresh(os.Stdout, refreshed, mode); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d source(s) failed", failures)
	}
	if skillErrs > 0 {
		return fmt.Errorf("%d skill error(s)", skillErrs)
	}
	return nil
}

// completeRepoNames completes the <name> arguments of
// `repo refresh`, which takes any number of them (none means every
// source). It never exits, never prints, and cannot block past
// completionDeadline: every failure degrades to "no candidates". A name already
// given is still offered — cobra has no notion of "already used", and the same
// shape completeChildren offers every child at every position.
func completeRepoNames(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmdCtx(cmd), completionDeadline)
	defer cancel()
	resp, err := ep.control().ListRepos(ctx,
		connect.NewRequest(&rafikiv1.ListReposRequest{}))
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

func newRepoRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Short:   "Remove a repo registration",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			_, err = ep.control().RemoveRepo(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RemoveRepoRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			fmt.Printf("removed %s\n", args[0])
			return nil
		},
	}
}

// emitRepoRefresh writes a refresh run's result in the resolved mode: one
// summary line per source in table mode, the canonical protojson of the
// responses as one {"rows": […]} envelope in -j, one compact response per
// line in -J. Every response carries its own name, so a multi-source run's
// rows attribute themselves. A run with nothing to refresh says so in table
// mode and emits an empty row set in JSON.
func emitRepoRefresh(w io.Writer, resps []*rafikiv1.RefreshRepoResponse, mode outputMode) error {
	if mode == outputJSON || mode == outputJSONL {
		return emitProtoRows(w, resps, mode)
	}
	if len(resps) == 0 {
		_, err := fmt.Fprintln(w, "no repos to refresh")
		return err
	}
	for _, r := range resps {
		if err := emitRepoSummary(w, r, mode); err != nil {
			return err
		}
	}
	return nil
}

// emitRepoAdded writes `repo add`'s summary: the registration's own refresh
// answer (commit, unchanged, skill count, skill errors) with the discovered
// script/package counts the follow-up refresh reported.
func emitRepoAdded(w io.Writer, added *rafikiv1.AddRepoResponse, inv *rafikiv1.RefreshRepoResponse, mode outputMode) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, added)
	case outputJSONL:
		return writeJSONL(w, []any{added})
	}
	row := added.GetRow()
	fmt.Fprintln(w, repoRefreshLine(row.GetName(), added.GetCommit(), added.GetUnchanged(),
		len(inv.GetScripts()), len(inv.GetPackages()), int(added.GetSkills())))
	emitRepoVenvFailure(w, inv)
	emitRepoSkillErrors(added.GetSkillErrors())
	return nil
}

// emitRepoSummary writes one refresh's summary line, plus the venv failure
// when the repo's shared venv did not build. A failed build does NOT fail the
// command — the refresh itself succeeded, and per the design a broken build
// only fails the runs that actually need the venv — so the failure is
// printed, visibly, on stdout. The skills half's errors go to stderr (see
// emitRepoSkillErrors) because they are what the command exits non-zero for.
func emitRepoSummary(w io.Writer, resp *rafikiv1.RefreshRepoResponse, mode outputMode) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, resp)
	case outputJSONL:
		// One compact record per line, the same -J contract every peer
		// emitter follows — never the indented -j shape.
		return writeJSONL(w, []any{resp})
	default:
		fmt.Fprintln(w, repoRefreshLine(resp.GetName(), resp.GetCommit(), resp.GetUnchanged(),
			len(resp.GetScripts()), len(resp.GetPackages()), int(resp.GetSkills())))
		emitRepoVenvFailure(w, resp)
		return nil
	}
}

// emitRepoVenvFailure prints the venv build failure when there is one.
func emitRepoVenvFailure(w io.Writer, resp *rafikiv1.RefreshRepoResponse) {
	if !resp.GetVenvReady() && resp.GetVenvError() != "" {
		fmt.Fprintf(w, "venv build failed: %s\n", resp.GetVenvError())
	}
}

// emitRepoSkillErrors prints one `skill error: <text>` line per skill the
// repo's refresh could not load, and returns how many there were. A skill
// error is not the refresh's error — the scripts half succeeded — but it is
// never silent either: a repo whose skills never load would otherwise look
// exactly like a repo that ships none.
func emitRepoSkillErrors(errs []string) int {
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "skill error: %s\n", e)
	}
	return len(errs)
}

// repoRefreshLine is the one-line summary `add` and `refresh` print:
// `refreshed <name> at <commit[:12]>[ (unchanged)]: N script(s), N package(s),
// N skill(s)`. The commit is shortened because a 40-character SHA pushes the
// counts off a terminal; twelve characters are unambiguous within one repo's
// history. "(unchanged)" says the ref had not moved since the last refresh.
func repoRefreshLine(name, commit string, unchanged bool, scripts, packages, skills int) string {
	line := fmt.Sprintf("refreshed %s at %s", name, defaultDash(shortCommit(commit)))
	if unchanged {
		line += " (unchanged)"
	}
	return fmt.Sprintf("%s: %d script(s), %d package(s), %d skill(s)", line, scripts, packages, skills)
}

// shortCommit truncates a commit to the twelve characters the summaries show.
func shortCommit(commit string) string {
	const short = 12
	if len(commit) > short {
		return commit[:short]
	}
	return commit
}

// emitRepoSet writes `repo set`'s confirmation: the updated registration, as
// one line in table mode or the canonical protojson of the row in JSON.
func emitRepoSet(w io.Writer, row *rafikiv1.RepoRow, mode outputMode) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, row)
	case outputJSONL:
		return writeJSONL(w, []any{row})
	default:
		fmt.Fprintf(w, "set %s: ref %s, refresh %s\n",
			row.GetName(), defaultDash(row.GetRef()), repoRefreshCell(row))
		return nil
	}
}

// emitRepoList writes list's output in the resolved mode, the same
// three-way contract every other list verb renders: a table, a {"rows": …}
// envelope, or one bare row per line.
func emitRepoList(w io.Writer, rows []*rafikiv1.RepoRow, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, map[string]any{"rows": rows})
	case outputJSONL:
		return writeJSONL(w, repoJSONLRows(rows))
	default:
		return renderRepoList(w, rows, useColor)
	}
}

// renderRepoList renders the inventory table: NAME, URL, REF, COMMIT,
// REFRESH, SKILLS, SCRIPTS. COMMIT is the last resolved commit (twelve
// characters), REFRESH "-" when the source is on demand, and both counts are
// the daemon's own (skills stored, scripts last discovered).
func renderRepoList(w io.Writer, rows []*rafikiv1.RepoRow, useColor bool) error {
	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "NAME", "URL", "REF", "COMMIT", "REFRESH", "SKILLS", "SCRIPTS")...)
	for _, r := range rows {
		tb.Row(r.GetName(), defaultDash(r.GetUrl()), defaultDash(r.GetRef()),
			defaultDash(shortCommit(r.GetCommit())), repoRefreshCell(r),
			fmt.Sprint(r.GetSkills()), fmt.Sprint(r.GetScripts()))
	}
	return tb.Render()
}

// repoRefreshCell renders a row's schedule: the interval, or "-" when it is on
// demand (never on by default, for any repo).
func repoRefreshCell(row *rafikiv1.RepoRow) string {
	d := row.GetRefreshInterval()
	if d == nil || d.AsDuration() <= 0 {
		return "-"
	}
	return d.AsDuration().String()
}

func repoJSONLRows(rows []*rafikiv1.RepoRow) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}
