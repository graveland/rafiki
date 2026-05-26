package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
)

// mustDial connects to the daemon's UDS using the --socket flag value
// (default ~/.pi/run/controller.sock). Exits with code 2 on failure so
// connection errors are distinguishable from user-input errors (exit 1).
func mustDial(cmd *cobra.Command) *client.Client {
	socket, _ := cmd.Flags().GetString("socket")
	c, err := client.Dial(socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: connect:", err)
		os.Exit(2)
	}
	return c
}

// cmdCtx returns the cobra command's context (populated with cancellation
// on SIGINT etc by cobra.Command.ExecuteContext). Falls back to
// context.Background() if no context was set.
func cmdCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// outputOpts reads --output and --color from the command flags and returns
// the resolved output mode and whether color should be emitted.
func outputOpts(cmd *cobra.Command) (outputMode, bool) {
	outFlag, _ := cmd.Flags().GetString("output")
	colorFlag, _ := cmd.Flags().GetString("color")
	tty := isStdoutTTY()
	return resolveOutputMode(outFlag, tty), colorEnabled(colorFlag, tty)
}

// setActive is a stub until Task 10 implements the active file.
func setActive(childID string) error { return nil }
