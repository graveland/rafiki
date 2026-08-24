package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
)

func newExecutorNameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "name [<name>]",
		Short: "Show or set this machine's executor name",
		Long: `Show or set the name this machine's executor is known by.

	The name is written into the executor row as the ` + "`machine`" + ` trust label when
	an operator mints a token with ` + "`rafiki executor enroll --name <name>`" + `, and read
	back here by an interactive client asking whether a durable executor already
	shares this filesystem. The two must match — that is the whole mechanism.

	A container or pod should export ` + paths.ExecutorName + ` instead (from the
	downward API); the environment wins over the file.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runExecutorName,
	}
}

func runExecutorName(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		if err := paths.SetMachineName(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Executor name set to %s (%s).\n",
			args[0], paths.MachineNameFile())
		fmt.Fprintf(cmd.OutOrStdout(),
			"Mint this machine's executor with:\n  rafiki executor enroll --name %s\n", args[0])
		return nil
	}

	name, source, err := paths.MachineName()
	if err != nil {
		return err
	}
	if source == "env" {
		fmt.Fprintf(cmd.OutOrStdout(), "%s (from %s)\n", name, paths.ExecutorName)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s (from %s)\n", name, source)
	return nil
}
