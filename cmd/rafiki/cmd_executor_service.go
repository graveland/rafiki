package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/version"
)

// `rafiki executor service …` manages the EXECUTOR as a per-user service, the
// same way `rafiki service` manages the daemon. It is for pets — a laptop, a
// VM, a dev box — where the machine is long-lived, keeps a credential file, and
// should reconnect on boot without anyone typing anything.
//
// It reuses the daemon's serviceBackend with a different unit identity
// (dev.graveland.rafiki-executor / rafiki-executor.service). Two identities
// rather than one unit with a mode: the executor commonly runs on machines that
// host no daemon at all, and a shared identity would make `stop` ambiguous on
// the machines that run both.
//
// The unit runs `rafiki executor serve …`, so unlike the daemon's it needs
// ARGUMENTS baked in — which is what serviceSpec.Args is for.

func newExecutorServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "service",
		Aliases: []string{"svc"},
		Short:   "Manage this machine's executor as a system service",
		Long: `Install, start, stop and inspect the executor as a per-user system service.

On macOS this uses launchd (launchctl); on Linux, systemd --user. The unit is
separate from the daemon's, so a machine can run either or both.

Install needs to know where to connect and how to authenticate:

  rafiki executor service install --connect rafiki.example.com:8443 \
      --enroll-token "$TOKEN" --root ~/src

The token is used ONCE, at first connect; the credential it is exchanged for is
written to the credential file and used for every reconnect thereafter. For a
machine that cannot keep a file, use --credential instead (see
` + "`rafiki executor create`" + `).`,
	}
	cmd.AddCommand(
		newExecutorServiceInstallCmd(),
		newExecutorServiceUninstallCmd(),
		newExecutorServiceSimpleCmd("start", "Start the executor service", func(b serviceBackend) error { return b.Start() }),
		newExecutorServiceSimpleCmd("stop", "Stop the executor service", func(b serviceBackend) error { return b.Stop() }),
		newExecutorServiceSimpleCmd("restart", "Restart the executor service", func(b serviceBackend) error { return b.Restart() }),
		newExecutorServiceStatusCmd(),
		newExecutorServiceLogsCmd(),
		newExecutorServiceTailCmd(),
	)
	return cmd
}

func newExecutorServiceInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the executor service",
		Args:  cobra.NoArgs,
		RunE:  runExecutorServiceInstall,
	}
	cmd.Flags().String("connect", "", "daemon executor endpoint to dial (host:port; default: derived from $RAFIKI_URL)")
	cmd.Flags().String("connect-socket", "", "rafikid executor socket on this machine (unix path)")
	cmd.Flags().String("root", "", "working directory root the executor serves (default: current directory)")
	cmd.Flags().String("enroll-token", "", "one-time enrollment token, for the first connect")
	cmd.Flags().String("credential", "", "durable credential, for a machine that keeps no file")
	cmd.Flags().String("credential-file", "", "where the credential is stored (default: <data dir>/executor.cred)")
	cmd.Flags().String("pin-cert", "", "SHA-256 fingerprint of the daemon's leaf certificate")
	cmd.Flags().String("server-name", "", "TLS server name (SNI), when it differs from the --connect host")
	cmd.Flags().Int("concurrency", 6, "maximum concurrent tool calls")
	cmd.Flags().String("rtk", "", "rewrite known commands through rtk: auto|on|off")
	cmd.Flags().String("spill-dir", "", "where oversized results and background job output are written")
	cmd.Flags().Int64("job-output-budget-mb", 0, "megabytes of background-job output retained per workspace")
	cmd.Flags().StringArray("proxy", nil, "LLM endpoint this executor will forward to, name=base_url (repeatable) — the main reason to run an executor as a service")
	cmd.Flags().String("binary", "", "path to the rafiki binary (default: this one)")
	cmd.Flags().String("path-env", "", "PATH value for the service environment (default: auto-detect)")
	return cmd
}

func runExecutorServiceInstall(cmd *cobra.Command, _ []string) error {
	connect, _ := cmd.Flags().GetString("connect")
	connectSocket, _ := cmd.Flags().GetString("connect-socket")
	connect, connectSocket, err := resolveExecutorConnectFlags(connect, connectSocket)
	if err != nil {
		return err
	}
	enrollToken, _ := cmd.Flags().GetString("enroll-token")
	credential, _ := cmd.Flags().GetString("credential")
	credentialFile, _ := cmd.Flags().GetString("credential-file")

	if credentialFile == "" && credential == "" {
		credentialFile = filepath.Join(paths.DataDir(), "executor.cred")
	}
	// Refuse an install that cannot possibly authenticate rather than one that
	// installs cleanly and then fails in a log file nobody is watching.
	if enrollToken == "" && credential == "" && !credFileExists(credentialFile) {
		return fmt.Errorf("nothing to authenticate with: pass --enroll-token for a first install, "+
			"or --credential for a machine that keeps no file (no credential file at %s)", credentialFile)
	}

	root, _ := cmd.Flags().GetString("root")
	resolvedRoot, err := resolveRoot(root)
	if err != nil {
		return err
	}

	binary, _ := cmd.Flags().GetString("binary")
	if binary == "" {
		binary, err = os.Executable()
		if err != nil {
			return fmt.Errorf("cannot determine this binary's path; pass --binary: %w", err)
		}
	}
	if binary, err = filepath.Abs(binary); err != nil {
		return err
	}

	args := []string{"executor", "serve", "--root", resolvedRoot}
	if connectSocket != "" {
		args = append(args, "--connect-socket", connectSocket)
	} else {
		args = append(args, "--connect", connect)
	}

	// The enrollment token is deliberately NOT baked into the unit. It is
	// one-time and short-lived, so a unit carrying it is a file full of a secret
	// that stopped working minutes after the install — and on the second start
	// the executor would present a consumed token and be refused, when the
	// credential file it wrote on the first run is what it should be using.
	// A supplied credential is NOT put in argv: it would be visible in `ps` and
	// in the unit file. It goes in the unit's environment instead, where
	// `rafiki executor serve --credential`'s default already reads it.
	if credentialFile != "" {
		args = append(args, "--credential-file", credentialFile)
	}
	for flag, dest := range map[string]string{
		"pin-cert": "--pin-cert", "server-name": "--server-name",
		"rtk": "--rtk", "spill-dir": "--spill-dir",
	} {
		if v, _ := cmd.Flags().GetString(flag); v != "" {
			args = append(args, dest, v)
		}
	}
	if c, _ := cmd.Flags().GetInt("concurrency"); c > 0 && c != 6 {
		args = append(args, "--concurrency", fmt.Sprint(c))
	}
	if b, _ := cmd.Flags().GetInt64("job-output-budget-mb"); b > 0 {
		args = append(args, "--job-output-budget-mb", fmt.Sprint(b))
	}
	proxyArgs, _ := cmd.Flags().GetStringArray("proxy")
	args, err = appendProxyArgs(args, proxyArgs)
	if err != nil {
		return err
	}

	pathEnv, _ := cmd.Flags().GetString("path-env")
	if pathEnv == "" {
		pathEnv = buildPathEnv()
	}
	home, _ := os.UserHomeDir()

	b := newExecutorServiceBackend()
	spec := serviceSpec{
		DaemonBinary: binary,
		Args:         args,
		PathEnv:      pathEnv,
		HomeEnv:      home,
		LogPath:      b.LogPath(),
	}
	if credential != "" {
		spec.ExtraEnv = map[string]string{"RAFIKI_EXECUTOR_CREDENTIAL": credential}
	}

	// Enrol BEFORE installing, so the credential file exists by the time the
	// unit starts. Otherwise the first supervised run consumes the token, and a
	// restart before that run finishes writing the file leaves the machine with
	// a consumed token and no credential — recoverable only by minting another.
	if enrollToken != "" && !credFileExists(credentialFile) {
		fmt.Println("Enrolling once before installing the service…")
		if err := enrollOnce(connect, connectSocket, cmd, enrollToken, credentialFile, resolvedRoot); err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
		fmt.Printf("Enrolled. Credential: %s\n", credentialFile)
	}

	if err := b.Install(spec); err != nil {
		return err
	}
	fmt.Printf("rafiki executor service installed.\nLog: %s\n", b.LogPath())
	return nil
}

// enrollOnce runs a short-lived executor purely to exchange the one-time token
// for a durable credential, then stops it.
//
// Done here, at install time, rather than left to the first supervised run:
// the token is one-time, so if the service manager restarts the unit before
// that run has written the credential file, the machine is left holding a
// consumed token and no credential — recoverable only by minting another.
func enrollOnce(connect, connectSocket string, cmd *cobra.Command, token, credentialFile, root string) error {
	pinCert, _ := cmd.Flags().GetString("pin-cert")
	serverName, _ := cmd.Flags().GetString("server-name")

	srv := executor.NewServer(executor.Options{
		Root:        root,
		Concurrency: 1,
		Version:     version.String(),
	})
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connErr := make(chan error, 1)
	go func() {
		opts := execpool.ConnectOptions{
			PinCert:        pinCert,
			ServerName:     serverName,
			EnrollToken:    token,
			CredentialFile: credentialFile,
			SelfReported: map[string]string{
				"os": runtime.GOOS, "arch": runtime.GOARCH, "version": version.String(),
			},
			Handler: executorHandler(srv),
		}
		if connectSocket != "" {
			opts.SocketPath = connectSocket
		} else {
			opts.Addr = connect
		}
		connErr <- execpool.Connect(ctx, opts)
	}()

	deadline := time.After(30 * time.Second)
	for {
		if credFileExists(credentialFile) {
			return nil
		}
		select {
		case err := <-connErr:
			if err != nil {
				return err
			}
			// Connect returned without error and without writing a credential:
			// nothing more will happen on this attempt.
			return fmt.Errorf("the connection ended before a credential was issued")
		case <-deadline:
			return fmt.Errorf("timed out after 30s waiting for enrollment against %s", connect)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// appendProxyArgs validates --proxy flags (repeatable name=base_url pairs)
// and appends them to args as repeated "--proxy value" pairs for the service
// unit's argv. Validated at install time, before the unit is written, so a
// malformed entry fails the install rather than the first supervised start.
func appendProxyArgs(args []string, proxies []string) ([]string, error) {
	if len(proxies) == 0 {
		return args, nil
	}
	if _, err := executor.ParseProxyFlags(proxies); err != nil {
		return nil, err
	}
	for _, p := range proxies {
		args = append(args, "--proxy", p)
	}
	return args, nil
}

func credFileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

func newExecutorServiceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the executor service",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := newExecutorServiceBackend().Uninstall(); err != nil {
				return err
			}
			fmt.Println("rafiki executor service uninstalled.")
			return nil
		},
	}
}

func newExecutorServiceSimpleCmd(use, short string, run func(serviceBackend) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := run(newExecutorServiceBackend()); err != nil {
				return err
			}
			fmt.Printf("rafiki executor service %sed.\n", use)
			return nil
		},
	}
}

func newExecutorServiceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the executor service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			st, err := newExecutorServiceBackend().Status()
			if err != nil {
				return err
			}
			fmt.Printf("installed: %v\nrunning:   %v\npid:       %d\n%s\n",
				st.Installed, st.Running, st.PID, st.Detail)
			return nil
		},
	}
}

func newExecutorServiceLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the executor service log",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			follow, _ := cmd.Flags().GetBool("follow")
			return streamLogFile(cmdCtx(cmd), newExecutorServiceBackend().LogPath(), follow)
		},
	}
	cmd.Flags().BoolP("follow", "f", false, "Keep streaming new log output instead of exiting at EOF")
	return cmd
}

func newExecutorServiceTailCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Follow the executor service log",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return streamLogFile(cmdCtx(cmd), newExecutorServiceBackend().LogPath(), true)
		},
	}
	return cmd
}
