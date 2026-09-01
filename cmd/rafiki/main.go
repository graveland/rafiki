package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/version"
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
		Use:           "rafiki",
		Short:         "Control the rafiki daemon",
		Long:          "rafiki is the command-line client for the rafikid daemon.\nIt speaks the JSONL protocol over the daemon's UDS socket.",
		Version:       version.String(),
		SilenceUsage:  true, // don't print usage on RunE errors
		SilenceErrors: true, // main() prints errors itself
	}

	// These three are persistent, so every subcommand inherits them; the
	// shorthands (-o/-c/-s) ride along too. -s in particular is the socket the
	// client actually talks to, so it earns the shortest form.
	root.PersistentFlags().StringP("socket", "s", "", "controller socket path (default: $RAFIKI_SOCKET, else the XDG runtime path)")
	root.PersistentFlags().StringP("output", "o", "auto", "output format for list/tail/conversations: auto|json|table (other commands always emit JSON)")
	root.PersistentFlags().StringP("color", "c", "auto", "color output: auto|always|never")

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
		newHistoryCmd(),
		newAttachCmd(),
		newCreateCmd(),
		newResumeCmd(),
		newKillCmd(),
		newCloseCmd(),
		newRecentCmd(),
		newSearchCmd(),
		newTasksCmd(),
		newConversationsCmd(),
		newSendCmd(),
		newTailCmd(),
		newLogsCmd(),
		newLabelCmd(),
		newModelsCmd(),
		newPresetsCmd(),
		newServiceCmd(),
		newCompletionCmd(),
		newClaudeCmd(),
		newExecutorCmd(),
		newDarajaCmd(),
		newUserCmd(),
	)

	return root
}
