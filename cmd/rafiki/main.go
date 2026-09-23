package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/version"
)

func main() {
	// The one context.Background() for the whole process, wrapped in the one
	// signal.NotifyContext — every command reaches it via cmdCtx(cmd), rather
	// than a subcommand that needs cancellation building its own local
	// signal.NotifyContext(context.Background(), …) pairing. `Execute()` (no
	// context) used to leave cmd.Context() defaulting to a plain
	// context.Background() with no signal wired to it at all, despite
	// cmdCtx's doc comment claiming otherwise.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	root := newRootCmd()
	if err := root.ExecuteContext(ctx); err != nil {
		// Cobra's RunE error path: print to stderr, exit 1.
		// Connection errors get exit 2 via direct os.Exit in subcommands.
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// rootArgs accepts zero arguments, for RunE to run as a bare `rafiki attach`,
// and otherwise reproduces cobra's own "unknown command" error (normally
// produced by Find(), but only when Args is nil) so a mistyped subcommand
// still reads as a typo rather than an attempt to attach to a child literally
// named "lsit".
func rootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	// SuggestionsFor reads this directly rather than defaulting it the way
	// cobra's own (unexported) findSuggestions does -- skip the assignment and
	// every near-miss silently gets zero suggestions instead of the "Did you
	// mean" list `rafiki lsit` used to produce.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n"
		for _, s := range suggestions {
			msg += "\t" + s + "\n"
		}
	}
	return errors.New(msg)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "rafiki",
		Short: "Control the rafiki daemon",
		Long: "rafiki is the command-line client for the rafikid daemon.\n" +
			"It speaks the JSONL protocol over the daemon's UDS socket.\n\n" +
			"With no subcommand it behaves like `rafiki attach`: the cockpit, rail-first.",
		Version:       version.String(),
		SilenceUsage:  true, // don't print usage on RunE errors
		SilenceErrors: true, // main() prints errors itself
		// A bare `rafiki` is `rafiki attach` with nothing to focus -- the same
		// rail-first cockpit `rafiki attach` opens with no argument. Args has to
		// be set explicitly for RunE to ever see zero args: Find()'s own
		// "unknown command" check (legacyArgs) only fires when Args is nil, so
		// leaving it nil to also cover the zero-arg case would swallow that
		// check along with it. rootArgs keeps the check for everything but the
		// zero-arg case, so `rafiki bogus` still fails exactly as it did before.
		Args: rootArgs,
		RunE: runAttach,
	}

	// Persistent, so every subcommand inherits them; the shorthands ride along
	// too. -P names the daemon: --socket is gone, because a socket path with no
	// credential beside it is exactly the split this replaced.
	root.PersistentFlags().StringP("output", "o", "auto", "output mode: auto (table) | json | jsonl — table is the default on TTY and pipe alike; -j/-J are shorthands")
	root.PersistentFlags().BoolP("json", "j", false, "shorthand for --output json (pretty JSON)")
	root.PersistentFlags().BoolP("jsonl", "J", false, "shorthand for --output jsonl (one compact record per line)")
	root.PersistentFlags().StringP("color", "c", "auto", "color output: auto|always|never")
	root.PersistentFlags().StringP("profile", "P", "", "profile naming the daemon to use (default: $RAFIKI_PROFILE, else the current-profile file)")

	_ = root.RegisterFlagCompletionFunc("output", cobra.FixedCompletions(
		[]string{"auto", "json", "jsonl", "table"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = root.RegisterFlagCompletionFunc("color", cobra.FixedCompletions(
		[]string{"auto", "always", "never"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = root.RegisterFlagCompletionFunc("profile", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeProfileNames(toComplete), cobra.ShellCompDirectiveNoFileComp
	})

	root.AddCommand(
		newListCmd(),
		newGetCmd(),
		newStatusCmd(),
		newHistoryCmd(),
		newAttachCmd(),
		newWatchCmd(),
		newCreateCmd(),
		newResumeCmd(),
		newStopCmd(),
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
		newPresetCmd(),
		newServiceCmd(),
		newCompletionCmd(),
		newClaudeCmd(),
		newExecutorCmd(),
		newDarajaCmd(),
		newUserCmd(),
		newConfigCmd(),
		newSkillsCmd(),
		newPythonCmd(),
		newProfileCmd(),
		newBudgetCmd(),
	)

	return root
}
