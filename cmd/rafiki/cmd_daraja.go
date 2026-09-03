// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/daraja"
)

// newDarajaCmd builds `rafiki daraja`, the per-child process host.
//
// It is a subcommand rather than a binary for the same reason the executor is:
// this repo ships exactly two artifacts, one client and one server, and
// cmd/rafiki-executor was deleted to keep it that way.
func newDarajaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daraja",
		Short: "Host a single child process for a remote rafikid",
	}
	cmd.AddCommand(newDarajaServeCmd())
	return cmd
}

func newDarajaServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run one child process and serve its stdio over a socket",
		Long: "Hosts exactly one child process and relays its stdio. daraja dies with\n" +
			"its child and the child dies with daraja: there is no state to keep on\n" +
			"either side of that pair.\n\n" +
			"The child is claude, and its command line is built from the typed flags\n" +
			"below — the same builder the executor's Launch and daraja's Restart use.",
		RunE: runDarajaServe,
	}
	cmd.Flags().String("socket", "", "unix socket to listen on (required)")
	cmd.Flags().String("binary", "", "child binary to run (required)")
	cmd.Flags().String("cwd", "", "working directory for the child")
	cmd.Flags().String("model", "", "model id for the child")
	cmd.Flags().String("resume-session", "", "claude session id to continue")
	cmd.Flags().String("permission-mode", "", "claude permission mode")
	return cmd
}

func runDarajaServe(cmd *cobra.Command, args []string) error {
	// The host rebuilds argv from the spec it holds, so raw child arguments no
	// longer cross this boundary — accepting and silently dropping them would
	// mislead a caller that still expects the old passthrough.
	if len(args) > 0 {
		return errors.New("daraja no longer takes raw child arguments; use --model, --resume-session and --permission-mode")
	}
	socket, _ := cmd.Flags().GetString("socket")
	binary, _ := cmd.Flags().GetString("binary")
	cwd, _ := cmd.Flags().GetString("cwd")
	model, _ := cmd.Flags().GetString("model")
	resumeSession, _ := cmd.Flags().GetString("resume-session")
	permissionMode, _ := cmd.Flags().GetString("permission-mode")
	if socket == "" {
		return errors.New("--socket is required")
	}
	if binary == "" {
		return errors.New("--binary is required")
	}

	host := daraja.NewHost(daraja.HostOptions{
		Binary: binary,
		Spec: daraja.ChildSpec{
			Kind:           daraja.KindClaude,
			Model:          model,
			ResumeSession:  resumeSession,
			PermissionMode: permissionMode,
		},
		Cwd: cwd,
	})
	if err := host.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}

	// A stale socket from a previous daraja would make Listen fail. Removing it
	// is safe precisely because a daraja never outlives its child: a live one
	// holding this path cannot exist without the child that owns this id.
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socket, err)
	}
	defer os.Remove(socket)

	srv := daraja.NewServer(host)
	path, handler := srv.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, handler)

	// Unencrypted HTTP/2: connect-go refuses BIDI streaming below HTTP/2, and
	// Relay is bidi. Both ends of a unix socket are ours, so there is nothing
	// for TLS to prove.
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: mux, Protocols: protos}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case <-srv.ShutdownRequested():
	case <-sigCh:
		_, _, _ = host.Shutdown(0)
	case err := <-errCh:
		_, _, _ = host.Shutdown(0)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return httpSrv.Close()
}
