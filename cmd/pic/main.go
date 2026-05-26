package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/version"
)

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		// Cobra's RunE error path: print to stderr, exit 1.
		// Connection errors get exit 2 via direct os.Exit in subcommands.
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "pic",
		Short:         "Control the pi-controller daemon",
		Long:          "pic is the command-line client for the pi-controller daemon.\nIt speaks the JSONL protocol over the daemon's UDS socket.",
		Version:       version.String(),
		SilenceUsage:  true, // don't print usage on RunE errors
		SilenceErrors: true, // main() prints errors itself
	}

	root.PersistentFlags().String("socket", "", "controller socket path (default ~/.pi/run/controller.sock)")
	root.PersistentFlags().String("output", "auto", "output format for list/tail: auto|json|table (other commands always emit JSON)")
	root.PersistentFlags().String("color", "auto", "color output: auto|always|never")

	_ = root.RegisterFlagCompletionFunc("output", cobra.FixedCompletions(
		[]string{"auto", "json", "table"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = root.RegisterFlagCompletionFunc("color", cobra.FixedCompletions(
		[]string{"auto", "always", "never"},
		cobra.ShellCompDirectiveNoFileComp,
	))

	root.AddCommand(
		newListCmd(),
		newGetCmd(),
		newStatusCmd(),
		newCreateCmd(),
		newAttachCmd(),
		newResumeCmd(),
		newKillCmd(),
		newForgetCmd(),
		newRecentCmd(),
		newSearchCmd(),
		newSendCmd(),
		newTailCmd(),
		newLogsCmd(),
		newLabelCmd(),
		newServiceCmd(),
		newInstallExtensionCmd(),
		newCompletionCmd(),
	)

	return root
}
