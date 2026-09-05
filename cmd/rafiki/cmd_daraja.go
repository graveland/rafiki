// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/daraja"
	darajapb "go.graveland.dev/rafiki/pkg/darajapb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/proxyenv"
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
	cmd.AddCommand(newDarajaLaunchCmd())
	return cmd
}

func newDarajaServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run one child process and serve its stdio over a reverse-dialled connection",
		Long: "Hosts exactly one child process and relays its stdio. daraja dials\n" +
			"rafikid and then serves DarajaService on the connection it dialled —\n" +
			"the same inversion the executor uses, so a laptop behind NAT can reach\n" +
			"an operator's daemon. daraja dies with its child and the child dies\n" +
			"with daraja: there is no state to keep on either side of that pair.\n\n" +
			"The ticket arrives from the environment (RAFIKI_DARAJA_TICKET), never\n" +
			"from argv: 1b-i's Launch builds a command line anyone on the machine\n" +
			"can read with ps. The ticket is replaced by a reconnect credential on\n" +
			"the first successful hello, which is held in memory only.",
		Args: cobra.NoArgs,
		RunE: runDarajaServe,
	}
	cmd.Flags().String("connect", "", "rafi kID address to connect to (host:port)")
	cmd.Flags().String("connect-socket", "", "rafi kID Unix socket path")
	cmd.Flags().String("child-id", "", "child ID for authentication")
	cmd.Flags().String("binary", "", "child binary to run (required)")
	cmd.Flags().String("cwd", "", "working directory for the child")
	cmd.Flags().String("kind", "claude", "child protocol to host")
	cmd.Flags().String("model", "", "model to pass to the child")
	cmd.Flags().String("resume", "", "session id to resume")
	cmd.Flags().String("permission-mode", "", "child permission mode")
	cmd.Flags().String("proxy-url", "", "rafiki proxy URL to point the child at (empty: talk to Anthropic directly)")
	cmd.Flags().Bool("passthrough", false, "omit ANTHROPIC_AUTH_TOKEN so the child's own Claude subscription bills instead of rafiki's proxy token")
	cmd.Flags().Int("auto-compact-window", 0, "override Claude Code's assumed context window for a proxied model (0: leave its default)")
	cmd.Flags().Bool("record-requests", false, "ask the proxy to record this conversation's raw HTTP traffic")
	return cmd
}

func runDarajaServe(cmd *cobra.Command, _ []string) error {
	connect, connectSocket, err := resolveDarajaConnectFlags(
		mustGetString(cmd, "connect"),
		mustGetString(cmd, "connect-socket"))
	if err != nil {
		return err
	}
	childID := mustGetString(cmd, "child-id")
	if childID == "" {
		return errors.New("--child-id is required")
	}
	binary := mustGetString(cmd, "binary")
	if binary == "" {
		return errors.New("--binary is required")
	}
	cwd := mustGetString(cmd, "cwd")
	kind := mustGetString(cmd, "kind")
	model := mustGetString(cmd, "model")
	resume := mustGetString(cmd, "resume")
	permMode := mustGetString(cmd, "permission-mode")
	proxyURL := mustGetString(cmd, "proxy-url")
	passthrough, _ := cmd.Flags().GetBool("passthrough")
	autoCompact, _ := cmd.Flags().GetInt("auto-compact-window")
	recordRequests, _ := cmd.Flags().GetBool("record-requests")

	// The proxy token travels by environment, never argv, for the same
	// reason RAFIKI_DARAJA_TICKET does: ps is world-readable on this machine.
	proxyToken := os.Getenv("RAFIKI_DARAJA_PROXY_TOKEN")

	headers := map[string]string{
		"X-Rafiki-Session": childID,
		"X-Rafiki-Source":  "claude",
	}
	if recordRequests {
		headers["X-Rafiki-Record-Requests"] = "1"
	}

	// daraja builds a COMPLETE environment here, once, for its whole process
	// lifetime — this is what makes passthrough billing possible at all: the
	// local-subprocess daemon path can only ever APPEND to its own inherited
	// env (proxyChildEnv), which can never unset ANTHROPIC_API_KEY. See
	// proxyenv.Claude's own doc comment.
	env, proxyModelArgs := proxyenv.Claude(os.Environ(), proxyenv.ClaudeOptions{
		URL:               proxyURL,
		Token:             proxyToken,
		PassthroughAuth:   passthrough,
		Model:             model,
		AutoCompactWindow: autoCompact,
		Headers:           headers,
	})
	// proxyModelArgs is already nil when proxyURL == "" (proxyenv.Claude's own
	// early return), so ChildSpec.argv()'s plain --model is used unproxied.

	host := daraja.NewHost(daraja.HostOptions{
		Binary:         binary,
		Cwd:            cwd,
		Env:            env,
		EnvOverride:    true,
		ProxyModelArgs: proxyModelArgs,
		Spec: daraja.ChildSpec{
			Kind:           kind,
			Model:          model,
			ResumeSession:  resume,
			PermissionMode: permMode,
		},
	})
	if err := host.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}

	srv := daraja.NewServer(host)
	mux := http.NewServeMux()
	mux.Handle(srv.Routes())

	opts := daraja.ConnectOptions{
		ChildID: childID,
		Handler: mux,
		PID:     os.Getpid(),
	}
	if connect != "" {
		opts.Addr = connect
	}
	if connectSocket != "" {
		opts.SocketPath = connectSocket
	}
	// Ticket comes from the environment, never argv: argv is world-readable
	// in ps, and 1b-i's AdminService.Launch already builds an argv that
	// anyone on the machine can read.
	opts.Ticket = os.Getenv("RAFIKI_DARAJA_TICKET")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- daraja.Connect(context.Background(), opts) }()

	select {
	case <-srv.ShutdownRequested():
	case <-host.Done():
		// The host gave up on respawning (RespawnStopsAtTheLimit): a daraja
		// whose child cannot be kept alive has nothing left to host. Exiting
		// here is what makes that promise true.
	case <-sigCh:
		_, _, _ = host.Shutdown(0)
	case err := <-errCh:
		_, _, _ = host.Shutdown(0)
		// Log the reason darja exited so operators can diagnose whether
		// it was authentication failure, connection loss, or a terminal
		// rejection from rafikid.
		if err != nil {
			fmt.Fprintf(os.Stderr, "daraja: %v\n", err)
		}
		if !errors.Is(err, daraja.ErrRejected) && err != nil {
			return err
		}
	}
	return nil
}

// resolveDarajaConnectFlags applies the same mutual-exclusion validation as
// executor's resolveExecutorConnectFlags. Unlike the executor, daraja does NOT
// derive defaults from RAFIKI_URL — a per-child process knows exactly where
// its raﬁkid is (it was launched by the daemon on that specific machine).
func resolveDarajaConnectFlags(connect, connectSocket string) (string, string, error) {
	modes := 0
	for _, set := range []bool{connect != "", connectSocket != ""} {
		if set {
			modes++
		}
	}
	if modes > 1 {
		return "", "", fmt.Errorf("--connect and --connect-socket are mutually exclusive")
	}
	if modes == 0 {
		return "", "", fmt.Errorf("one of --connect or --connect-socket is required")
	}
	return connect, connectSocket, nil
}

func mustGetString(cmd *cobra.Command, name string) string {
	s, _ := cmd.Flags().GetString(name)
	return s
}

// newDarajaLaunchCmd builds `rafiki daraja launch`. It resolves an executor,
// calls DarajaLaunch on the daemon's Connect control plane (through
// newConnectEndpoint, which honours the resolved profile for remote
// daemons), and waits for the daraja to reverse-dial back before returning.
func newDarajaLaunchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Launch a daraja-hosted child through a remote or local daemon",
		Long: "Launches a claude child via daraja on a matching executor:\n" +
			"  1. Resolves an executor that admits the selector and supports launching claude.\n" +
			"  2. Calls AdminService.Launch on the executor with a one-shot ticket.\n" +
			"  3. Waits for the daraja's reverse dial into the daemon's pool.\n" +
			"  4. Returns the child id once connected.\n\n" +
			"Through newConnectEndpoint — honours the resolved profile for remote daemons.\n" +
			"A launch that matches no executor is refused with a per-candidate diagnostic.",
		Args: cobra.NoArgs,
		RunE: runDarajaLaunch,
	}
	cmd.Flags().String("executor", "", "label selector over executor labels")
	cmd.Flags().String("cwd", "", "working directory for the hosted child")
	cmd.Flags().String("model", "", "model id for the claude child")
	cmd.Flags().String("resume", "", "session id to resume (optional)")
	return cmd
}

func runDarajaLaunch(cmd *cobra.Command, _ []string) error {
	endpoint, err := newConnectEndpoint(cmd)
	if err != nil {
		return fmt.Errorf("resolve endpoint: %w", err)
	}
	client := endpoint.control()

	cwd := mustGetString(cmd, "cwd")
	if cwd == "" {
		return errors.New("--cwd is required")
	}
	model := mustGetString(cmd, "model")
	if model == "" {
		return errors.New("--model is required")
	}
	resume := mustGetString(cmd, "resume")
	selector := mustGetString(cmd, "executor")

	req := &rafikiv1.DarajaLaunchRequest{
		ExecutorSelector: selector,
		Cwd:              cwd,
		Spec: &darajapb.ChildSpec{
			Kind: darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{
				Model:         model,
				ResumeSession: resume,
			},
		},
	}

	resp, err := client.DarajaLaunch(context.Background(), connect.NewRequest(req))
	if err != nil {
		return diagnoseConnectError(err, endpoint.describe)
	}

	fmt.Fprintf(os.Stdout, "child_id=%s pid=%d pgid=%d connected_at=%d\n",
		resp.Msg.GetChildId(), resp.Msg.GetPid(), resp.Msg.GetPgid(),
		resp.Msg.GetConnectedUnixMs())
	return nil
}
