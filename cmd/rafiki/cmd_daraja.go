// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childsock"
	"go.graveland.dev/rafiki/pkg/daraja"
	darajapb "go.graveland.dev/rafiki/pkg/darajapb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
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
			"the first successful hello, which is held in memory only.\n\n" +
			"Trailing positional arguments after a bare \"--\" are passed to the\n" +
			"child verbatim — the operator escape hatch for flags daraja does\n" +
			"not map itself.",
		Args: cobra.ArbitraryArgs,
		RunE: runDarajaServe,
	}
	cmd.Flags().String("connect", "", "rafi kID address to connect to (host:port)")
	cmd.Flags().String("connect-socket", "", "rafi kID Unix socket path")
	cmd.Flags().String("child-id", "", "child ID for authentication")
	cmd.Flags().String("binary", "", "child binary to run (required for claude; the script's interpreter for kind=script)")
	cmd.Flags().String("cwd", "", "working directory for the child")
	cmd.Flags().String("kind", "claude", "child protocol to host: claude or script")
	cmd.Flags().String("model", "", "model to pass to the child")
	cmd.Flags().String("resume", "", "session id to resume")
	cmd.Flags().String("permission-mode", "", "child permission mode")
	cmd.Flags().String("append-system-prompt", "", "system prompt to append to the child's own")
	cmd.Flags().String("proxy-url", "", "rafiki proxy URL to point the child at (empty: talk to Anthropic directly)")
	cmd.Flags().Bool("passthrough", false, "omit ANTHROPIC_AUTH_TOKEN so the child's own Claude subscription bills instead of rafiki's proxy token")
	cmd.Flags().Int("auto-compact-window", 0, "override Claude Code's assumed context window for a proxied model (0: leave its default)")
	cmd.Flags().Bool("record-requests", false, "ask the proxy to record this conversation's raw HTTP traffic")
	cmd.Flags().String("pin-cert", "", "SHA-256 fingerprint of the daemon's leaf certificate (TLS target only)")
	cmd.Flags().String("server-name", "", "TLS server name (SNI) when it differs from --connect's host")
	return cmd
}

func runDarajaServe(cmd *cobra.Command, args []string) error {
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
	pinCert := mustGetString(cmd, "pin-cert")
	serverName := mustGetString(cmd, "server-name")

	// A script child has its own construction path entirely: the interpreter
	// is --binary, the resolved script argv arrives positionally, the proxy
	// machinery does not apply, and the per-child Connect socket is served
	// HERE so the script can talk to its daemon.
	if kind == daraja.KindScript {
		return runDarajaScriptServe(childID, binary, cwd, args, pinCert, serverName,
			connect, connectSocket)
	}

	model := mustGetString(cmd, "model")
	resume := mustGetString(cmd, "resume")
	permMode := mustGetString(cmd, "permission-mode")
	appendPrompt := mustGetString(cmd, "append-system-prompt")
	proxyURL := mustGetString(cmd, "proxy-url")
	passthrough, _ := cmd.Flags().GetBool("passthrough")
	autoCompact, _ := cmd.Flags().GetInt("auto-compact-window")
	recordRequests, _ := cmd.Flags().GetBool("record-requests")

	// The proxy token travels by environment, never argv, for the same
	// reason RAFIKI_DARAJA_TICKET does: ps is world-readable on this machine.
	// The per-child MCP secret rides the same route: AdminService.Launch put
	// it in this process's environment (from the Launch RPC's mcp_token — the
	// same authenticated channel proxy_token arrives over), and it becomes the
	// child's RAFIKI_MCP_TOKEN below, where the injected --mcp-config's
	// ${RAFIKI_MCP_TOKEN} placeholder expands. Empty means no per-child secret
	// was minted; ClaudeEnv then falls back to the proxy bearer.
	proxyToken := os.Getenv("RAFIKI_DARAJA_PROXY_TOKEN")
	mcpToken := os.Getenv("RAFIKI_MCP_TOKEN")

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
	// proxyenv.ClaudeEnv's own doc comment.
	env, values := proxyenv.ClaudeEnv(os.Environ(), proxyenv.ClaudeOptions{
		URL:               proxyURL,
		Token:             proxyToken,
		MCPToken:          mcpToken,
		PassthroughAuth:   passthrough,
		Model:             model,
		AutoCompactWindow: autoCompact,
		Headers:           headers,
	})
	// values.MCPConfig and values.ModelArgs are already empty when proxyURL
	// == "" (proxyenv.ClaudeEnv's own early return), so ChildSpec.argv()'s
	// plain --model is used unproxied. values.MCPConfig is the BARE inline
	// JSON — the shape HostOptions.MCPConfig and claudeargv.Params.MCPConfig
	// both expect, with Build prepending the flag — so the rendered child argv
	// carries exactly one --mcp-config element, never a doubled prefix.

	host := daraja.NewHost(daraja.HostOptions{
		Binary:      binary,
		Cwd:         cwd,
		Env:         env,
		EnvOverride: true,
		MCPConfig:   values.MCPConfig,
		ModelArgs:   values.ModelArgs,
		Spec: daraja.ChildSpec{
			Kind:               kind,
			Model:              model,
			ResumeSession:      resume,
			PermissionMode:     permMode,
			AppendSystemPrompt: appendPrompt,
			// Positional args (everything after the "--" the executor puts
			// before the spec's ExtraArgs) are the operator escape hatch and
			// are handed to the child verbatim, appended last by
			// claudeargv.Build so they can override anything above them.
			ExtraArgs: args,
		},
	})
	if err := host.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}

	srv := daraja.NewServer(host)
	mux := http.NewServeMux()
	mux.Handle(srv.Routes())

	opts := daraja.ConnectOptions{
		ChildID:    childID,
		Handler:    mux,
		PID:        os.Getpid(),
		PinCert:    pinCert,
		ServerName: serverName,
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

	return runDarajaConnectLoop(host, srv, opts)
}

// runDarajaConnectLoop is the tail both hosted kinds share: reverse-dial the
// daemon and serve until the daemon asks for a shutdown, the host gives up
// (claude's respawn limit — or a script's exit, which for a script child is
// the normal end), a signal arrives, or the connection fails terminally.
func runDarajaConnectLoop(host *daraja.Host, srv *daraja.Server, opts daraja.ConnectOptions) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- daraja.Connect(context.Background(), opts) }()

	select {
	case <-srv.ShutdownRequested():
	case <-host.Done():
		// The host gave up on respawning (RespawnStopsAtTheLimit): a daraja
		// whose child cannot be kept alive has nothing left to host. Exiting
		// here is what makes that promise true — and for a script child it is
		// also the NORMAL end: the script exited, its Exited event was
		// relayed (the server drains the queue on done), and daraja dies with
		// it.
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

// runDarajaScriptServe hosts one script child: it resolves the daemon's face
// target from the same address it reverse-dials, serves the child's per-child
// Connect socket (pkg/childsock) with the child secret from the launch
// payload, and runs the interpreter on the resolved argv.
//
// The launch payload's credentials never touch argv: the ticket and the child
// secret arrive by environment (AdminService.Launch set them), and the
// environment they travel in is scrubbed out of the child's own env by
// darajaScriptEnv — the script gets a socket path (RAFIKI_CHILD_CONNECT),
// never a token.
func runDarajaScriptServe(childID, interpreter, cwd string, argv []string, pinCert, serverName, connect, connectSocket string) error {
	// Held in memory only, like the reconnect credential: it lives in this
	// process's environment and in the childsock proxy's closure, and it dies
	// with the child. A script child without one could not authenticate a
	// single request it sends — refuse rather than start one that 401s
	// forever.
	secret := os.Getenv("RAFIKI_CHILD_SECRET")
	if secret == "" {
		return errors.New("RAFIKI_CHILD_SECRET is not set: a script child cannot be hosted without its per-child credential")
	}

	// The face target is the address this process already reverse-dials. A
	// --connect executor reaches the daemon's TLS control listener, which
	// serves the face on "/"; its certificate is verified the way this
	// executor verifies it — by pinned fingerprint when one was configured,
	// never by silently trusting whatever the network presents. A
	// --connect-socket executor is on the daemon's own machine: the face for
	// Connect verbs is the daemon's Connect control socket (a sibling of the
	// executor socket it dialled), and no TLS applies to a unix socket.
	var target *url.URL
	var rt http.RoundTripper
	switch {
	case connect != "":
		target = &url.URL{Scheme: "https", Host: connect}
		rt = daraja.TLSTransport(serverName, pinCert)
	default:
		faceSocket := paths.ConnectSocketPath()
		target = &url.URL{Scheme: "http", Host: "connect.rafiki.invalid"}
		rt = childsock.UnixTransport(faceSocket)
	}

	// The per-child socket lives OUTSIDE any workspace the executor's file
	// tools can reach by a workspace-relative path: under rafiki's own cache
	// dir, in a 0700 per-child directory (childsock creates it). The child's
	// id is in the path, so only this child's scripts can even guess its
	// sibling sockets without listing the directory.
	dir := filepath.Join(paths.CacheDir(), "script-sockets", childID)
	sockCtx, sockCancel := context.WithCancel(context.Background())
	defer sockCancel()
	sock, err := childsock.ServeTransport(sockCtx, dir, target, secret, rt)
	if err != nil {
		return fmt.Errorf("script child: per-child socket: %w", err)
	}

	host := daraja.NewHost(daraja.HostOptions{
		Binary: interpreter,
		Cwd:    cwd,
		// A COMPLETE environment, like the claude path: the executor's own
		// environment (its env files applied, its operator-set functional
		// variables intact) minus every RAFIKI_*/ANTHROPIC_*/OPENROUTER_*
		// variable, plus the one channel the child is given. PYTHONPATH and
		// the forwarded environment arrived in this process's environment from
		// AdminService.Launch and ride the inherited part; the strip does not
		// touch them (PYTHONPATH is functional, and a forwarded name that
		// carried a credential prefix was already refused twice before it got
		// here).
		Env:         darajaScriptEnv(os.Environ(), childsock.SocketPath(dir)),
		EnvOverride: true,
		Spec: daraja.ChildSpec{
			Kind: daraja.KindScript,
			// The resolved argv — script path then the spec's own args — as
			// positionals after the executor's "--". The respawn loop is off
			// for this kind: the host exits with the script.
			ExtraArgs: argv,
		},
	})
	if err := host.Start(); err != nil {
		_ = sock.Close()
		return fmt.Errorf("start script child: %w", err)
	}
	defer func() { _ = sock.Close() }()

	srv := daraja.NewServer(host)
	mux := http.NewServeMux()
	mux.Handle(srv.Routes())

	opts := daraja.ConnectOptions{
		ChildID:    childID,
		Handler:    mux,
		PID:        os.Getpid(),
		PinCert:    pinCert,
		ServerName: serverName,
		Addr:       connect,
		SocketPath: connectSocket,
		Ticket:     os.Getenv("RAFIKI_DARAJA_TICKET"),
	}
	return runDarajaConnectLoop(host, srv, opts)
}

// darajaScriptEnv builds a script child's COMPLETE environment from the
// daraja process's own: everything the EXECUTOR set for its launches survives
// (PATH, HOME, the executor's env-file values, PYTHONPATH and the forwarded
// environment that arrived with the launch payload) minus every
// RAFIKI_*/ANTHROPIC_*/OPENROUTER_* variable — the same strip rule the
// daemon applies to its own script children and to launch payloads
// (pkg/child.ScriptEnvStripPrefixes), applied a third time here because a
// credential that survives any one plane is out. Then the one channel the
// child is given: RAFIKI_CHILD_CONNECT, the per-child socket PATH. The child
// never sees a token of any kind.
func darajaScriptEnv(environ []string, socketPath string) []string {
	env := make([]string, 0, len(environ)+1)
	for _, e := range environ {
		if child.ScriptEnvStripped(e) {
			continue
		}
		env = append(env, e)
	}
	return append(env, "RAFIKI_CHILD_CONNECT="+socketPath)
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
