package main

import (
	"fmt"
	"net"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/sandboxrelay"
)

// newExecutorBridgeCmd is the foothold container's entry point: it serves a
// unix socket inside a mounted volume and splices every accepted connection to
// a TCP dial of the launcher's relay. The launcher cannot reach into a VM's
// network namespace, so the container dials OUT to the host and the volume
// carries the socket back in — this verb is the half that runs inside the
// container.
//
// It is HIDDEN and registered by the launcher's setup, never typed by a user:
// the container's argv is fixed by the foothold image, and the verb is useless
// on a host that has no volume-mounted socket to bridge.
func newExecutorBridgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "bridge",
		Hidden: true,
		Args:   cobra.NoArgs,
		Short:  "Bridge a unix socket in a volume to a launcher's TCP relay (run by a foothold container)",
		RunE:   runExecutorBridge,
	}
	cmd.Flags().String("listen", "", "Absolute directory holding the unix socket to serve (daemon.sock)")
	cmd.Flags().String("dial", "", "Launcher relay to splice each connection to, as host:port")
	_ = cmd.MarkFlagRequired("listen")
	_ = cmd.MarkFlagRequired("dial")
	return cmd
}

func runExecutorBridge(cmd *cobra.Command, _ []string) error {
	// A usage error here is a bad argv from the launcher, not something a user
	// typed; printing the whole help on every failed start buries the one line
	// that names the mismatched flag.
	cmd.SilenceUsage = true

	listen, _ := cmd.Flags().GetString("listen")
	dial, _ := cmd.Flags().GetString("dial")

	if !filepath.IsAbs(listen) {
		return fmt.Errorf("--listen %q must be an absolute path", listen)
	}
	if _, port, err := net.SplitHostPort(dial); err != nil || port == "" {
		return fmt.Errorf("--dial %q must be host:port", dial)
	}

	return sandboxrelay.Serve(cmdCtx(cmd), listen, sandboxrelay.TCPDial(dial))
}
