package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Spawn a new pi child and attach a local TUI to it",
		Long: `Spawn a new pi child via the controller, then open the pi TUI driving it.

By default, quitting the TUI (Ctrl+D, /quit) detaches — the session keeps
running in the daemon. Use --kill-on-exit for native pi exit semantics
(quitting terminates the session).

With --detached, pic create just spawns the child and exits without
attaching. The child runs in the background; reattach later with
'pic attach <name>'.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runCreate,
	}
	addSpawnFlags(cmd)
	cmd.Flags().Bool("detached", false, "Spawn without attaching (equivalent to `pic spawn`)")
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits")
	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	req, err := buildSpawnRequest(cmd, args)
	if err != nil {
		return err
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_spawn: %s", client.FormatError(resp))
	}

	var data protocol.SpawnResponseData
	_ = json.Unmarshal(resp.Data, &data)
	if err := setActive(data.ChildID); err != nil {
		// Best effort — log to stderr but don't fail.
		fmt.Fprintln(os.Stderr, "warning: could not update active marker:", err)
	}

	detached, _ := cmd.Flags().GetBool("detached")
	if detached {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	return execPicAttach(data.ChildID, killOnExit)
}
