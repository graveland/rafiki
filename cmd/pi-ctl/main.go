package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.1.0"

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
		Use:           "pi-ctl",
		Short:         "Control the pi-controller daemon",
		Long:          "pi-ctl is the command-line client for the pi-controller daemon.\nIt speaks the JSONL protocol over the daemon's UDS socket.",
		Version:       version,
		SilenceUsage:  true, // don't print usage on RunE errors
		SilenceErrors: true, // main() prints errors itself
	}

	root.PersistentFlags().String("socket", "", "controller socket path (default ~/.pi/run/controller.sock)")
	root.PersistentFlags().String("output", "auto", "output format: auto|json|table")
	root.PersistentFlags().String("color", "auto", "color output: auto|always|never")

	// Subcommands wired up in later tasks; nothing yet.

	return root
}
