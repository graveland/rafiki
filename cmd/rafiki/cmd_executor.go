package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newExecutorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "executor",
		Aliases: []string{"exec", "ex"},
		Short:   "Run an executor, or manage the executor pool",
		Long: `Run an executor, or manage the pool of them.

  serve, serve-stdio    BE an executor on this machine, in the foreground.
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
		// serve/serve-stdio are the executor ITSELF, not operator verbs against
		// the daemon; see cmd_executor_serve.go. They sit under the same noun
		// because "be an executor" and "administer executors" are the same
		// subject, and `serve` does not collide with any verb above.
		newExecutorServeCmd(),
		newExecutorServeStdioCmd(),
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
		Use:   "list",
		Short: "List enrolled executors",
		RunE:  runExecutorList,
	}
	cmd.Flags().String("selector", "", "Label selector to filter by")
	cmd.Flags().Int("limit", 50, "Maximum number of executors to return")
	return cmd
}

func runExecutorList(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

	selector, _ := cmd.Flags().GetString("selector")
	limit, _ := cmd.Flags().GetInt("limit")

	req := protocol.ExecutorListRequest{
		Type:     protocol.TypeCtrlExecutorList,
		Selector: selector,
		Limit:    limit,
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_executor_list: %s", client.FormatError(resp))
	}

	// Response is {"executors": [...]}
	var wrapper struct {
		Executors []executors.Executor `json:"executors"`
	}
	if err := json.Unmarshal(resp.Data, &wrapper); err != nil {
		return fmt.Errorf("malformed response: %w", err)
	}

	mode, _ := outputOpts(cmd)
	if mode == outputTable {
		renderExecutorTable(wrapper.Executors)
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(wrapper)
	}
	return nil
}

func renderExecutorTable(execs []executors.Executor) {
	if len(execs) == 0 {
		fmt.Println("No enrolled executors.")
		return
	}
	fmt.Printf("%-14s %-20s %-10s %-30s %s\n", "ID", "NAME", "ENABLED", "LABELS", "ADMITS")
	fmt.Println(strings.Repeat("-", 100))
	for _, ex := range execs {
		shortID := ex.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		enabled := "yes"
		if !ex.Enabled {
			enabled = "no"
		}
		labelStr := executorFormatLabels(ex.Labels)
		fmt.Printf("%-14s %-20s %-10s %-30s %s\n", shortID, ex.DisplayName, enabled, labelStr, ex.Admits)
	}
}

func executorFormatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	var parts []string
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
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
		Use:   "label <executor-id> [k=v ...]",
		Short: "Set or remove labels on an executor",
		Long: `Set or remove labels on an executor's database row.

Labels take effect on the executor's next connection — no restart or
machine access is needed. The rafiki/ prefix is reserved.`,
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
		Args:  cobra.ExactArgs(1),
		RunE:  runExecutorDisable,
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
		Args:  cobra.ExactArgs(1),
		RunE:  runExecutorEnable,
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
