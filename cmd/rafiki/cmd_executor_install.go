package main

import (
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
)

func newExecutorInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <target>",
		Short: "Install a rafiki executor on an ssh target",
		Long: `Install and start a rafiki executor on a machine reachable over ssh.

<target> is an ssh destination — "host" or "user@host". The ssh itself is run
by one of your own executors (--via), with THAT machine's ssh config, so
ProxyJump, User, keys and Include all come from there. The new executor runs
exactly this daemon's version, because the daemon is the epoch authority.

--via defaults to this machine's executor name (see ` + "`rafiki executor name`" + `); when
this machine has no name the flag is omitted and the daemon picks the single
native launcher you own.

The new executor's machine name defaults to the destination's host part.
Reinstalling the same name reuses that executor — the same row, the same id,
so anything that pinned ` + "`executor: <name>`" + ` keeps working. An install
proceeds only onto a target that is empty for its namespace or that carries
this same executor's own install: there is no --force, and clearing a foreign
install is one command you run deliberately.

If ssh refuses (host key, auth), the error carries ssh's own message — fix it
on the --via machine and retry.

--mode picks the transport. auto (the default) probes the target for a route to
the daemon and starts a process that dials it directly when there is one,
otherwise it relays through the launcher's ssh tunnel. direct requires the
dialling form and is refused when the target cannot reach the daemon. tunnel
always relays through the launcher.

The installed executor is restarted automatically on its next use, so a machine
that reboots does not need re-installing.`,
		Args: cobra.ExactArgs(1),
		RunE: runExecutorInstall,
	}
	cmd.Flags().String("via", "", "Launcher executor that runs the ssh (machine name or id); defaults to this machine's executor name")
	cmd.Flags().String("name", "", "Machine name for the new executor; defaults to the destination's host part")
	cmd.Flags().String("root", "", "Directory the new executor serves; defaults to its home directory")
	cmd.Flags().StringArray("label", nil, "Label to set on the new executor (repeatable, k=v)")
	cmd.Flags().String("admits", "", "Executor-side admission selector over child labels")
	cmd.Flags().String("mode", "auto", "Install transport: auto|direct|tunnel")
	// Completion lists the modes without enforcing them: a wrong value is the
	// daemon's refusal to give, so it is sent rather than swallowed here.
	_ = cmd.RegisterFlagCompletionFunc("mode", cobra.FixedCompletions(
		executorInstallModeValues, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func runExecutorInstall(cmd *cobra.Command, args []string) error {
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

	labelPairs, _ := cmd.Flags().GetStringArray("label")
	labels, err := parseLabelPairs(labelPairs)
	if err != nil {
		return fmt.Errorf("--label: %w", err)
	}
	via, _ := cmd.Flags().GetString("via")
	if via == "" {
		via = thisMachineExecutorName(cmd)
	}
	name, _ := cmd.Flags().GetString("name")
	root, _ := cmd.Flags().GetString("root")
	admits, _ := cmd.Flags().GetString("admits")
	// installMode, not `mode`: the output mode already owns that name in this
	// function, and the two are unrelated user decisions.
	installMode, _ := cmd.Flags().GetString("mode")

	resp, err := ep.control().InstallExecutor(cmdCtx(cmd), connect.NewRequest(&rafikiv1.InstallExecutorRequest{
		Target: args[0],
		Via:    via,
		Name:   name,
		Root:   root,
		Labels: labels,
		Admits: admits,
		Mode:   installMode,
	}))
	if err != nil {
		return connectVerbErr(err, ep.describe)
	}
	row := resp.Msg.GetExecutor()
	if row == nil {
		return fmt.Errorf("executor install: daemon returned no executor row")
	}

	if mode == outputTable {
		return renderExecutorTable(os.Stdout, []*rafikiv1.ExecutorRow{row}, useColor)
	}
	// JSON is the row's canonical protojson — the same shape `executor list`
	// emits per row, so a script reads one message whether it listed or
	// installed.
	return emitProto(os.Stdout, row, mode)
}

// thisMachineExecutorName is --via's default: the name this machine's executor
// is enrolled under, which is the launcher whose ssh config and keys an
// operator set up for it. It is read leniently and an unreadable name is ""
// (omitted from the request), because the daemon then picks the caller's single
// native launcher — a name this machine never had must not refuse the command.
//
// The name follows the INSTANCE, so a declared -P/RAFIKI_PROFILE selects it
// (applyExecutorInstance, the lenient form: nothing is written).
func thisMachineExecutorName(cmd *cobra.Command) string {
	applyExecutorInstance(cmd)
	name, _, err := paths.MachineName()
	if err != nil {
		return ""
	}
	return name
}

func newExecutorUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <name|id>",
		Short: "Remove an executor installed over ssh",
		Long: `Remove an executor that was installed over ssh.

The ssh session ends, the target's install marker is removed and the executor
row is deleted. The target keeps the shared binary cache, so installing there
again pushes nothing.

<name|id> is the executor's machine name or its row id (any unique trailing
fragment of four or more characters works). An executor that was ENROLLED or
CREATED rather than installed is refused here — use ` + "`rafiki executor delete`" + `
for that, which is the same end state without an ssh target to clean up.`,
		Args: cobra.ExactArgs(1),
		RunE: runExecutorUninstall,
	}
}

func runExecutorUninstall(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	if _, err := ep.control().UninstallExecutor(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.UninstallExecutorRequest{Executor: args[0]})); err != nil {
		return connectVerbErr(err, ep.describe)
	}
	fmt.Printf("Executor %s uninstalled.\n", args[0])
	return nil
}
