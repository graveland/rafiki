package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
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
  enable                administer the pool, via the daemon's control socket.

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

func newExecutorEnrollCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Mint a one-time enrollment token",
		Long: `Mint a one-time enrollment token for a new executor.

	The token is printed ONCE to stdout and will not be shown again.
	Pass it to the executor binary as --enroll-token.

	Labels bound to the token become trust labels on the executor row — the
	executor cannot claim them and an operator can revoke them with a row update.`,
		RunE: runExecutorEnroll,
	}
	cmd.Flags().StringArray("label", nil, "Label to bind to the token (repeatable, k=v)")
	cmd.Flags().StringArray("root", nil, "Root path the executor may access (repeatable)")
	cmd.Flags().String("isolation", "none", "Isolation level: none|container|vm")
	cmd.Flags().String("workspace-mode", "pinned", "Workspace provisioning: ephemeral|pinned")
	cmd.Flags().String("admits", "", "Executor-side admission selector over child labels")
	cmd.Flags().Duration("ttl", time.Hour, "Token lifetime (default 1h)")
	return cmd
}

func runExecutorEnroll(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

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

	req := protocol.ExecutorEnrollRequest{
		Type:          protocol.TypeCtrlExecutorEnroll,
		Labels:        labels,
		Roots:         roots,
		Isolation:     isolation,
		WorkspaceMode: wm,
		Admits:        admits,
		TTLSeconds:    int64(ttl.Seconds()),
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_executor_enroll: %s", client.FormatError(resp))
	}

	var data protocol.ExecutorEnrollResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return fmt.Errorf("malformed response: %w", err)
	}

	// Token to stdout ONLY; nothing else on stdout so it pipes.
	fmt.Fprintln(os.Stderr, "Token minted — this will not be shown again.")
	fmt.Println(data.Token)
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
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

	selector, _ := cmd.Flags().GetString("selector")
	limit, _ := cmd.Flags().GetInt("limit")

	execs, err := fetchExecutors(ctx, c, selector, limit)
	if err != nil {
		return err
	}

	mode, useColor := outputOpts(cmd)
	if mode == outputTable {
		return renderExecutorTable(os.Stdout, execs, useColor)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Executors []executors.Executor `json:"executors"`
	}{execs})
}

// fetchExecutors issues ctrl_executor_list and decodes its {"executors": [...]}
// wrapper. Shared by `list` and `delete --all-*`, which both need the raw rows.
func fetchExecutors(ctx context.Context, c *client.Client, selector string, limit int) ([]executors.Executor, error) {
	req := protocol.ExecutorListRequest{
		Type:     protocol.TypeCtrlExecutorList,
		Selector: selector,
		Limit:    limit,
	}
	resp, err := c.Request(ctx, req)
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("ctrl_executor_list: %s", client.FormatError(resp))
	}
	var wrapper struct {
		Executors []executors.Executor `json:"executors"`
	}
	if err := json.Unmarshal(resp.Data, &wrapper); err != nil {
		return nil, fmt.Errorf("malformed response: %w", err)
	}
	return wrapper.Executors, nil
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

// applyMachineLabel stamps machine=<id> onto an enrollment token's labels
// unless the operator named one explicitly.
//
// It is a TRUST label, written at mint time and living in the row, not a
// self-reported fact: it participates in selection, deciding which executor an
// interactive client on this box binds its children to. An executor that could
// assert its own machine could attract another operator's work.
func applyMachineLabel(labels map[string]string) error {
	if labels == nil {
		return fmt.Errorf("applyMachineLabel: nil label map")
	}
	if labels["machine"] != "" {
		return nil
	}
	name, _, err := paths.MachineName()
	if err != nil {
		return fmt.Errorf("resolve this executor name: %w", err)
	}
	labels["machine"] = name
	return nil
}

// renderExecutorTable writes the executor pool as a table.
//
// When useColor is false every style is left empty, so piped and redirected
// output carries no ANSI escapes at all and stays byte-stable for scripts.
// When it is true, output goes through colorprofile so codes degrade to what
// the terminal actually supports (NO_COLOR never reaches here — colorEnabled
// already answered false).
func renderExecutorTable(w io.Writer, execs []executors.Executor, useColor bool) error {
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
		if !ex.LastSeenAt.IsZero() {
			lastSeen = humanize.Time(ex.LastSeenAt)
		}
		connectedSince := "-"
		if ex.ConnectedAt != nil && !ex.ConnectedAt.IsZero() {
			connectedSince = humanize.Time(*ex.ConnectedAt)
		}
		rows[i] = []string{
			shortExecutorID(ex.ID),
			defaultDash(ex.DisplayName),
			executorStatus(ex),
			executorFormatLabels(ex.Labels, 48),
			defaultDash(ex.Admits),
			connectedSince,
			lastSeen,
		}
	}

	t := table.New()
	t.Headers("ID", "NAME", "STATUS", "LABELS", "ADMITS", "CONNECTED", "LAST SEEN")
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
// talking to us (offline), or has been switched off (disabled). Connected is a
// view field from the daemon's live pool, not the row — see ExecutorList.
func executorStatus(ex executors.Executor) string {
	switch {
	case !ex.Enabled:
		return "disabled"
	case ex.Connected:
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
	live connection within one health interval.`,
		RunE: runExecutorCreate,
	}
	cmd.Flags().StringArray("label", nil, "Label to bind to the executor (repeatable, k=v)")
	cmd.Flags().StringArray("root", nil, "Root path the executor may access (repeatable)")
	cmd.Flags().String("isolation", "none", "Isolation level: none|container")
	cmd.Flags().String("workspace-mode", "pinned", "Workspace provisioning: ephemeral|pinned")
	cmd.Flags().String("admits", "", "Executor-side admission selector over child labels")
	return cmd
}

func runExecutorCreate(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	labelPairs, _ := cmd.Flags().GetStringArray("label")
	labels, err := parseLabelPairs(labelPairs)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	roots, _ := cmd.Flags().GetStringArray("root")
	isolation, _ := cmd.Flags().GetString("isolation")
	workspaceMode, _ := cmd.Flags().GetString("workspace-mode")
	admits, _ := cmd.Flags().GetString("admits")

	resp, err := c.Request(cmdCtx(cmd), protocol.ExecutorCreateRequest{
		Type:          protocol.TypeCtrlExecutorCreate,
		Labels:        labels,
		Roots:         roots,
		Isolation:     isolation,
		WorkspaceMode: workspaceMode,
		Admits:        admits,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_executor_create: %s", client.FormatError(resp))
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
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
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

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

	req := protocol.ExecutorLabelRequest{
		Type:       protocol.TypeCtrlExecutorLabel,
		ExecutorID: executorID,
		Set:        set,
		Remove:     removeKeys,
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_executor_label: %s", client.FormatError(resp))
	}

	// Response is the full executor object.
	var ex executors.Executor
	if err := json.Unmarshal(resp.Data, &ex); err != nil {
		// Fallback: print raw JSON.
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(resp.Data))
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(ex)
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
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

	req := protocol.ExecutorDisableRequest{
		Type:       protocol.TypeCtrlExecutorDisable,
		ExecutorID: args[0],
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_executor_disable: %s", client.FormatError(resp))
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
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

	req := protocol.ExecutorEnableRequest{
		Type:       protocol.TypeCtrlExecutorEnable,
		ExecutorID: args[0],
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_executor_enable: %s", client.FormatError(resp))
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
// offline within one health interval anyway).
func filterExecutorsForDelete(execs []executors.Executor, allDisabled, allOffline bool) []executors.Executor {
	var out []executors.Executor
	for _, e := range execs {
		if (allDisabled && !e.Enabled) || (allOffline && !e.Connected) {
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

	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

	if !bulk {
		return deleteOneExecutor(ctx, c, args[0])
	}

	// 500 is the ceiling ctrl_executor_list enforces; there is no "no limit".
	all, err := fetchExecutors(ctx, c, "", 500)
	if err != nil {
		return err
	}
	matches := filterExecutorsForDelete(all, allDisabled, allOffline)
	if len(matches) == 0 {
		fmt.Println("No executors match.")
		return nil
	}

	_, useColor := outputOpts(cmd)
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
		if err := deleteOneExecutor(ctx, c, e.ID); err != nil {
			fmt.Fprintf(os.Stderr, "executor %s: %v\n", shortExecutorID(e.ID), err)
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

func deleteOneExecutor(ctx context.Context, c *client.Client, executorID string) error {
	req := protocol.ExecutorDeleteRequest{
		Type:       protocol.TypeCtrlExecutorDelete,
		ExecutorID: executorID,
	}
	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_executor_delete: %s", client.FormatError(resp))
	}
	fmt.Printf("Executor %s deleted.\n", executorID)
	return nil
}
