package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"connectrpc.com/connect"
	"github.com/charmbracelet/colorprofile"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

func newExecutorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "executor",
		Aliases: []string{"exec", "ex"},
		Short:   "Run an executor, or manage the executor pool",
		Long: `Run an executor, or manage the pool of them.

  serve                 BE an executor on this machine, in the foreground.
  service               run it as a per-user system service (launchd/systemd).
  enroll, create, list,
  label, disable,
  enable, name          administer the pool, via the daemon's control socket.

The administrative verbs output JSON.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newExecutorEnrollCmd(),
		newExecutorCreateCmd(),
		newExecutorListCmd(),
		newExecutorLabelCmd(),
		newExecutorDisableCmd(),
		newExecutorEnableCmd(),
		newExecutorDeleteCmd(),
		newExecutorNameCmd(),
		// serve is the executor ITSELF, not an operator verb against the
		// daemon; see cmd_executor_serve.go. It sits under the same noun
		// because "be an executor" and "administer executors" are the same
		// subject, and `serve` does not collide with any verb above.
		newExecutorServeCmd(),
		newExecutorServiceCmd(),
	)
	return cmd
}

// ─── enroll ────────────────────────────────────────────────────────────────────

// executorIsolationValues and executorWorkspaceModeValues are the closed
// sets the enroll and create verbs' --isolation/--workspace-mode flags accept.
// They exist so completion and the flags' help text cannot drift: a wrong
// closed list is worse than none, so these are the values the flags
// document, nothing inferred.
var (
	executorIsolationValues     = []string{"none", "container", "vm"}
	executorWorkspaceModeValues = []string{"ephemeral", "pinned"}
)

// bindExecutorEnumCompletions registers the closed-enum completions shared by
// enroll and create.
func bindExecutorEnumCompletions(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("isolation", cobra.FixedCompletions(
		executorIsolationValues, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("workspace-mode", cobra.FixedCompletions(
		executorWorkspaceModeValues, cobra.ShellCompDirectiveNoFileComp))
}

func newExecutorEnrollCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Mint a one-time enrollment token",
		Long: `Mint a one-time enrollment token for a new executor.

	The token is printed ONCE to stdout and will not be shown again.
	Pass it to the executor binary as --enroll-token.

	Labels bound to the token become trust labels on the executor row — the
	executor cannot claim them and an operator can revoke them with a row update.

	--name is the name of the machine the executor will RUN on, which is not
	necessarily this one: a token minted here is routinely carried elsewhere
	with --enroll-token. Run ` + "`rafiki executor name`" + ` on the target box to see
	what to pass.

	owner and machine are written by the daemon — owner from this connection,
	machine from --name — so neither can be given with --label.`,
		RunE: runExecutorEnroll,
	}
	// No backquotes in this usage string: cobra's UnquoteUsage takes the first
	// backquoted word as the flag's VALUE NAME, so a mention of the machine
	// label renders as `--name machine` instead of `--name string`.
	cmd.Flags().String("name", "", "Name of the machine this executor runs on; becomes its 'machine' trust label and must match 'rafiki executor name' on that box")
	cmd.Flags().StringArray("label", nil, "Label to bind to the token (repeatable, k=v)")
	cmd.Flags().StringArray("root", nil, "Root path the executor may access (repeatable)")
	cmd.Flags().String("isolation", "none", "Isolation level: none|container|vm")
	cmd.Flags().String("workspace-mode", "pinned", "Workspace provisioning: ephemeral|pinned")
	cmd.Flags().String("admits", "", "Executor-side admission selector over child labels")
	cmd.Flags().Duration("ttl", time.Hour, "Token lifetime (default 1h)")
	bindExecutorEnumCompletions(cmd)
	return cmd
}

func runExecutorEnroll(cmd *cobra.Command, _ []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}

	name, _ := cmd.Flags().GetString("name")
	labelPairs, _ := cmd.Flags().GetStringArray("label")
	labels, err := parseLabelPairs(labelPairs)
	if err != nil {
		return fmt.Errorf("--label: %w", err)
	}
	roots, _ := cmd.Flags().GetStringArray("root")
	isolation, _ := cmd.Flags().GetString("isolation")
	wm, _ := cmd.Flags().GetString("workspace-mode")
	admits, _ := cmd.Flags().GetString("admits")
	ttl, _ := cmd.Flags().GetDuration("ttl")

	resp, err := ep.control().EnrollExecutor(cmdCtx(cmd), connect.NewRequest(&rafikiv1.EnrollExecutorRequest{
		Name:          name,
		Labels:        labels,
		Roots:         roots,
		Isolation:     isolation,
		WorkspaceMode: wm,
		Admits:        admits,
		TtlSeconds:    int64(ttl.Seconds()),
	}))
	if err != nil {
		return fmt.Errorf("executor enroll: %s", formatConnectErr(err))
	}

	// Token to stdout ONLY; nothing else on stdout so it pipes.
	fmt.Fprintln(os.Stderr, "Token minted — this will not be shown again.")
	fmt.Println(resp.Msg.GetToken())
	return nil
}

// ─── list ──────────────────────────────────────────────────────────────────────

func newExecutorListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List enrolled executors",
		Long: `List enrolled executors.

The ID column shows the LAST twelve characters of each executor's id:
ids are UUIDv7s, whose leading bits are a timestamp — every executor
minted in the same window shares its front, and only the tail
distinguishes them.

That fragment is enough to act on a row:

  rafiki executor disable <fragment>

Any unique trailing fragment of four or more characters is accepted;
an ambiguous one names the rows it matches instead of picking one.`,
		RunE: runExecutorList,
	}
	cmd.Flags().String("selector", "", "Label selector to filter by")
	cmd.Flags().IntP("limit", "l", 50, "Maximum number of executors to return")
	return cmd
}

func runExecutorList(cmd *cobra.Command, _ []string) error {
	// The output mode is a user-input decision (-j and -J together is an
	// error), so resolve it before any round trip.
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	selector, _ := cmd.Flags().GetString("selector")
	limit, _ := cmd.Flags().GetInt("limit")

	rows, err := fetchExecutorRows(cmdCtx(cmd), ep.control(), selector, limit)
	if err != nil {
		return err
	}

	if mode == outputTable {
		return renderExecutorTable(os.Stdout, rows, useColor)
	}
	// JSON is the canonical protojson of the wire's ExecutorRow rows; the
	// framed full executors.Executor shape is retired with its transport.
	return emitProtoRows(os.Stdout, rows, mode)
}

// fetchExecutorRows lists the executor pool over the Connect executor-admin
// listing. kind is deliberately EMPTY: that selects the plain management
// listing — the durable table merged with the live pool, selector and
// limit-cap semantics included, eligibility unevaluated — which both `list`
// and `delete --all-*` want. A non-empty kind would ask the spawn-shaped
// eligibility question instead. The limit rides the request as given; the
// daemon applies the same default-50/clamp-500 contract its framed
// dispatcher carried.
func fetchExecutorRows(ctx context.Context, ctl rafikiv1connect.ControlClient, selector string, limit int) ([]*rafikiv1.ExecutorRow, error) {
	resp, err := ctl.ListExecutors(ctx, connect.NewRequest(&rafikiv1.ListExecutorsRequest{
		Selector: selector,
		Limit:    int32(limit),
	}))
	if err != nil {
		return nil, fmt.Errorf("executor list: %s", formatConnectErr(err))
	}
	return resp.Msg.GetRows(), nil
}

// Column indexes into an executor table row; StyleFunc styles by column.
const (
	colID = iota
	colName
	colStatus
	colLabels
	colAdmits
	colConnected
	colLastSeen
)

// shortExecutorID returns the display form of an executor id: its trailing
// twelve characters — the fragment the label/enable/disable verbs accept as an
// argument.
//
// The TAIL, not the head: executor ids are UUIDv7s whose leading bits are a
// millisecond timestamp, so rows minted close together share their front and
// only the end distinguishes them. Truncating from the front displayed twelve
// characters that were nearly identical across every recent row — and a form
// no verb accepted.
func shortExecutorID(id string) string {
	if len(id) <= executorShortIDLen {
		return id
	}
	return id[len(id)-executorShortIDLen:]
}

const executorShortIDLen = 12

// renderExecutorTable writes the executor pool as a table.
//
// When useColor is false every style is left empty, so piped and redirected
// output carries no ANSI escapes at all and stays byte-stable for scripts.
// When it is true, output goes through colorprofile so codes degrade to what
// the terminal actually supports (NO_COLOR never reaches here — colorEnabled
// already answered false).
func renderExecutorTable(w io.Writer, execs []*rafikiv1.ExecutorRow, useColor bool) error {
	if len(execs) == 0 {
		fmt.Fprintln(w, "No enrolled executors.")
		return nil
	}

	out := w
	if useColor {
		out = colorprofile.NewWriter(w, os.Environ())
	}

	rows := make([][]string, len(execs))
	for i, ex := range execs {
		lastSeen := "-"
		if ms := ex.GetLastSeenMs(); ms != 0 {
			lastSeen = humanize.Time(time.UnixMilli(ms))
		}
		connectedSince := "-"
		if ms := ex.GetConnectedAtMs(); ms != 0 {
			connectedSince = humanize.Time(time.UnixMilli(ms))
		}
		rows[i] = []string{
			shortExecutorID(ex.GetId()),
			defaultDash(ex.GetMachine()),
			executorStatus(ex),
			executorFormatLabels(ex.GetLabels(), 48),
			defaultDash(ex.GetAdmits()),
			connectedSince,
			lastSeen,
		}
	}

	t := table.New()
	t.Headers("ID", "MACHINE", "STATUS", "LABELS", "ADMITS", "CONNECTED", "LAST SEEN")
	t.Rows(rows...)
	t.StyleFunc(func(row, col int) lipgloss.Style {
		// Padding is layout, not color: it applies whether or not styling
		// does, so plain output keeps its column gutters too.
		s := lipgloss.NewStyle().Padding(0, 1)
		if !useColor {
			return s
		}
		if row == table.HeaderRow {
			return s.Bold(true)
		}
		if col != colStatus {
			return s
		}
		switch rows[row][colStatus] {
		case "live":
			return s.Foreground(lipgloss.Color("2")) // green: connected and enabled
		case "offline":
			return s.Foreground(lipgloss.Color("3")) // yellow: enabled but not connected
		case "disabled":
			return s.Foreground(lipgloss.Color("1")) // red: credential revoked
		}
		return s
	})

	_, err := fmt.Fprintln(out, t.Render())
	return err
}

// executorStatus collapses the row's two booleans into one operational word:
// whether the machine can take work right now (live), exists but is not
// talking to us (offline), or has been switched off (disabled). Connected is
// the daemon's live-pool view of the row, and connected_at_ms/last_seen_ms
// are the wire's 0 when there is no current connection or no sighting ever —
// exactly what renderExecutorTable dashes.
func executorStatus(ex *rafikiv1.ExecutorRow) string {
	switch {
	case !ex.GetEnabled():
		return "disabled"
	case ex.GetConnected():
		return "live"
	default:
		return "offline"
	}
}

// executorFormatLabels renders a label map as sorted k=v pairs joined with
// commas, truncated to max runes. Map iteration order is randomized in Go, so
// sorting is what keeps two invocations seconds apart identical; truncation is
// what keeps one noisy label set from widening the whole table past the
// terminal edge — JSON mode carries the full map.
func executorFormatLabels(labels map[string]string, max int) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	s := strings.Join(parts, ",")
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// ─── create ────────────────────────────────────────────────────────────────────

func newExecutorCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an executor and print its durable credential (stateless path)",
		Long: `Create an executor row and its durable credential in one step.

	The credential is printed ONCE and only its hash is stored. Give it to the
	executor as --credential or RAFIKI_EXECUTOR_CREDENTIAL; that executor writes
	nothing to disk, which is what a deployment with no durable local storage
	needs.

	Prefer ` + "`enroll`" + ` where the machine can keep a file. Enrollment hands the
	operator only a short-lived one-time token, so a leak expires by itself and a
	theft announces itself — the thief consumes the token and the real executor
	fails loudly. A credential from this command is long-lived and a theft of it
	is silent. Revoke with ` + "`rafiki executor disable`" + `, which takes effect on a
	live connection within one health interval.

	--name is the name of the machine the executor will RUN on, which is not
	necessarily this one — the credential is carried to it. Run ` + "`rafiki executor name`" + `
	on the target box to see what to pass. owner and machine are written by the
	daemon, so neither can be given with --label.`,
		RunE: runExecutorCreate,
	}
	// No backquotes in this usage string: cobra's UnquoteUsage takes the first
	// backquoted word as the flag's VALUE NAME, so a mention of the machine
	// label renders as `--name machine` instead of `--name string`.
	cmd.Flags().String("name", "", "Name of the machine this executor runs on; becomes its 'machine' trust label and must match 'rafiki executor name' on that box")
	cmd.Flags().StringArray("label", nil, "Label to bind to the executor (repeatable, k=v)")
	cmd.Flags().StringArray("root", nil, "Root path the executor may access (repeatable)")
	cmd.Flags().String("isolation", "none", "Isolation level: none|container|vm")
	cmd.Flags().String("workspace-mode", "pinned", "Workspace provisioning: ephemeral|pinned")
	cmd.Flags().String("admits", "", "Executor-side admission selector over child labels")
	bindExecutorEnumCompletions(cmd)
	return cmd
}

func runExecutorCreate(cmd *cobra.Command, _ []string) error {
	// The output mode is a user-input decision (-j and -J together is an
	// error), so resolve it before minting anything.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}

	name, _ := cmd.Flags().GetString("name")
	labelPairs, _ := cmd.Flags().GetStringArray("label")
	labels, err := parseLabelPairs(labelPairs)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	roots, _ := cmd.Flags().GetStringArray("root")
	isolation, _ := cmd.Flags().GetString("isolation")
	workspaceMode, _ := cmd.Flags().GetString("workspace-mode")
	admits, _ := cmd.Flags().GetString("admits")

	resp, err := ep.control().CreateExecutor(cmdCtx(cmd), connect.NewRequest(&rafikiv1.CreateExecutorRequest{
		Name:          name,
		Labels:        labels,
		Roots:         roots,
		Isolation:     isolation,
		WorkspaceMode: workspaceMode,
		Admits:        admits,
	}))
	if err != nil {
		return fmt.Errorf("executor create: %s", formatConnectErr(err))
	}
	// The credential echo stays JSON in every mode — this verb renders no
	// table: the response's canonical protojson, pretty by default and one
	// compact line under -J.
	return emitProto(os.Stdout, resp.Msg, mode)
}

// ─── label ─────────────────────────────────────────────────────────────────────

func newExecutorLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "label <executor-id> [k=v ...]",
		Aliases: []string{"lab"},
		Short:   "Set or remove labels on an executor",
		Long: `Set or remove labels on an executor's database row.

Labels take effect on the executor's next connection — no restart or
machine access is needed. The rafiki/ prefix is reserved.

<executor-id> may be the full row id or any unique trailing fragment of
four or more characters, such as the short form shown by 'executor list'.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runExecutorLabel,
	}
	cmd.Flags().StringArray("remove", nil, "Remove a label key (repeatable)")
	return cmd
}

func runExecutorLabel(cmd *cobra.Command, args []string) error {
	// The output mode is a user-input decision (-j and -J together is an
	// error), so resolve it before touching the row.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}

	executorID := args[0]
	kvPairs := args[1:]
	removeKeys, _ := cmd.Flags().GetStringArray("remove")

	if len(kvPairs) == 0 && len(removeKeys) == 0 {
		return fmt.Errorf("at least one k=v argument or --remove flag is required")
	}

	set, err := parseLabelPairs(kvPairs)
	if err != nil {
		return fmt.Errorf("label: %w", err)
	}

	resp, err := ep.control().LabelExecutor(cmdCtx(cmd), connect.NewRequest(&rafikiv1.LabelExecutorRequest{
		ExecutorId: executorID,
		Set:        set,
		Remove:     removeKeys,
	}))
	if err != nil {
		return fmt.Errorf("executor label: %s", formatConnectErr(err))
	}

	// The echo is the response's updated row as canonical protojson, in every
	// mode — this verb renders no table.
	ex := resp.Msg.GetExecutor()
	if ex == nil {
		return fmt.Errorf("executor label: daemon returned no executor row")
	}
	return emitProto(os.Stdout, ex, mode)
}

// ─── disable / enable ──────────────────────────────────────────────────────────

func newExecutorDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <executor-id>",
		Short: "Disable an executor — its credential stops working",
		Long: `Disable an executor — its credential stops working.

Takes effect on a live connection within one health interval.
<executor-id> may be the full row id or any unique trailing fragment of
four or more characters, such as the short form shown by 'executor list'.`,
		Args: cobra.ExactArgs(1),
		RunE: runExecutorDisable,
	}
}

func runExecutorDisable(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	_, err = ep.control().DisableExecutor(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.DisableExecutorRequest{ExecutorId: args[0]}))
	if err != nil {
		return fmt.Errorf("executor disable: %s", formatConnectErr(err))
	}
	fmt.Printf("Executor %s disabled.\n", args[0])
	return nil
}

func newExecutorEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <executor-id>",
		Short: "Re-enable a disabled executor",
		Long: `Re-enable a disabled executor.

<executor-id> may be the full row id or any unique trailing fragment of
four or more characters, such as the short form shown by 'executor list'.`,
		Args: cobra.ExactArgs(1),
		RunE: runExecutorEnable,
	}
}

func runExecutorEnable(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	_, err = ep.control().EnableExecutor(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.EnableExecutorRequest{ExecutorId: args[0]}))
	if err != nil {
		return fmt.Errorf("executor enable: %s", formatConnectErr(err))
	}
	fmt.Printf("Executor %s enabled.\n", args[0])
	return nil
}

// ─── delete ──────────────────────────────────────────────────────────────────

func newExecutorDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete [executor-id]",
		Aliases: []string{"del"},
		Short:   "Permanently remove an executor row",
		Long: `Permanently remove an executor row. Unlike 'disable', this cannot be undone
— there is no tombstone for executors.

<executor-id> may be the full row id or any unique trailing fragment of
four or more characters, such as the short form shown by 'executor list'.

--all-disabled and --all-offline delete every matching row instead of one:
--all-disabled selects rows with Enabled=false; --all-offline selects rows
with no live connection right now (Connected=false), regardless of Enabled.
Passing both deletes the UNION of the two sets, not their intersection. These
list what they are about to delete and ask for confirmation unless -y/--yes
is given, and are mutually exclusive with an <executor-id> argument.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runExecutorDelete,
	}
	cmd.Flags().Bool("all-disabled", false, "Delete every disabled executor")
	cmd.Flags().Bool("all-offline", false, "Delete every executor with no live connection")
	cmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

// filterExecutorsForDelete selects the rows --all-disabled/--all-offline
// target, as the UNION of whichever criteria are on — not their intersection,
// since intersecting would make --all-disabled and --all-offline together
// behave almost exactly like --all-disabled alone (a disabled row goes
// offline within one health interval anyway). Only Enabled/Connected are
// consulted: the two flags need nothing else the row carries.
func filterExecutorsForDelete(execs []*rafikiv1.ExecutorRow, allDisabled, allOffline bool) []*rafikiv1.ExecutorRow {
	var out []*rafikiv1.ExecutorRow
	for _, e := range execs {
		if (allDisabled && !e.GetEnabled()) || (allOffline && !e.GetConnected()) {
			out = append(out, e)
		}
	}
	return out
}

func runExecutorDelete(cmd *cobra.Command, args []string) error {
	allDisabled, _ := cmd.Flags().GetBool("all-disabled")
	allOffline, _ := cmd.Flags().GetBool("all-offline")
	yes, _ := cmd.Flags().GetBool("yes")
	bulk := allDisabled || allOffline

	if bulk && len(args) > 0 {
		return fmt.Errorf("--all-disabled/--all-offline cannot be combined with an executor id")
	}
	if !bulk && len(args) != 1 {
		return fmt.Errorf("requires an <executor-id>, or --all-disabled/--all-offline")
	}

	ctx := cmdCtx(cmd)
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctl := ep.control()

	if !bulk {
		return deleteOneExecutor(ctx, ctl, args[0])
	}

	// 500 is the ceiling the executor-admin listing enforces (the same
	// default-50/clamp-500 contract the framed dispatcher carried); there is
	// no "no limit".
	all, err := fetchExecutorRows(ctx, ctl, "", 500)
	if err != nil {
		return err
	}
	matches := filterExecutorsForDelete(all, allDisabled, allOffline)
	if len(matches) == 0 {
		fmt.Println("No executors match.")
		return nil
	}

	_, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	if err := renderExecutorTable(os.Stdout, matches, useColor); err != nil {
		return err
	}
	if !yes {
		confirmed, err := confirmBulkDelete(len(matches))
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("Aborted.")
			return nil
		}
	}

	var failed int
	for _, e := range matches {
		if err := deleteOneExecutor(ctx, ctl, e.GetId()); err != nil {
			fmt.Fprintf(os.Stderr, "executor %s: %s\n", shortExecutorID(e.GetId()), formatConnectErr(err))
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d deletes failed", failed, len(matches))
	}
	return nil
}

// confirmBulkDelete prompts before an irreversible bulk delete. Non-TTY stdin
// refuses rather than silently proceeding OR silently doing nothing — a script
// without -y gets a clear error instead of a surprise either way.
func confirmBulkDelete(n int) (bool, error) {
	if !isStdinTTY() {
		return false, fmt.Errorf("refusing to delete %d executors without --yes (no interactive terminal)", n)
	}
	fmt.Printf("\nDelete %d executors? [y/N]: ", n)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	ans := strings.TrimSpace(strings.ToLower(line))
	confirmed, warned := parseKillAnswer(ans)
	if warned {
		fmt.Println("(treating as no)")
	}
	return confirmed, nil
}

// deleteOneExecutor removes one row. executorID may be the full row id or any
// unique trailing fragment of four or more characters — the daemon resolves
// it, exactly as the framed verb did.
func deleteOneExecutor(ctx context.Context, ctl rafikiv1connect.ControlClient, executorID string) error {
	if _, err := ctl.DeleteExecutor(ctx,
		connect.NewRequest(&rafikiv1.DeleteExecutorRequest{ExecutorId: executorID})); err != nil {
		return fmt.Errorf("executor delete: %s", formatConnectErr(err))
	}
	fmt.Printf("Executor %s deleted.\n", executorID)
	return nil
}
