package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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
		pathEnv = executorPathEnv()
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

	// Capture this shell's environment into the executor's environment file.
	// launchd provides no login shell, so without this the supervised executor
	// runs with nothing but HOME, PATH and (when --credential is given) a
	// credential — and every bash/git/language-server call it serves inherits
	// that barren environment. The unit stays narrow (it is world-readable);
	// the breadth lives in a 0600 file serve applies at startup. See
	// captureExecutorEnv for what is excluded and why.
	envFile := paths.ExecutorEnvFile()
	captured := captureExecutorEnv(os.Environ())
	res, mergeErr := paths.MergeEnvFile(envFile, captured,
		"added by rafiki executor service install on "+time.Now().Format("2006-01-02"))
	// The override must be baked into the unit whether or not anything was
	// written: an operator who pointed RAFIKI_EXECUTOR_ENV_FILE somewhere
	// custom needs serve to resolve THAT file, not the default — exactly the
	// reasoning behind baking paths.EnvFile into the daemon's unit.
	if override := os.Getenv(paths.ExecutorEnvFileEnv); override != "" {
		if spec.ExtraEnv == nil {
			spec.ExtraEnv = map[string]string{}
		}
		spec.ExtraEnv[paths.ExecutorEnvFileEnv] = override
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
		// The env file write already happened on disk regardless, so the report
		// is still true — print it before the failure, same reasoning as the
		// daemon's installReport.
		fmt.Print(executorEnvReport(envFile, captured, res, mergeErr))
		return err
	}
	fmt.Print(executorEnvReport(envFile, captured, res, mergeErr))
	fmt.Printf("rafiki executor service installed.\nLog: %s\n", b.LogPath())
	return nil
}

// executorEnvReport renders what `executor service install` tells the operator
// about the environment capture: which variables reached the 0600 file, which
// were already there (the file wins — MergeEnvFile never rewrites), and which
// it could not write. Values are never printed; a terminal scrollback is
// readable and much of this environment is not.
func executorEnvReport(envFile string, captured map[string]string, res paths.MergeResult, mergeErr error) string {
	var b strings.Builder
	if len(res.Added) > 0 {
		fmt.Fprintf(&b, "\nWritten to %s (0600):\n", envFile)
		for _, k := range res.Added {
			fmt.Fprintf(&b, "  %s\n", k)
		}
	}
	if len(res.Existing) > 0 {
		fmt.Fprintf(&b, "\nAlready in %s with the same value (%d variables), left alone.\n", envFile, len(res.Existing))
	}
	if len(res.Conflict) > 0 {
		b.WriteString("\nAlready in the environment file with a DIFFERENT value than this shell:\n")
		for _, k := range res.Conflict {
			fmt.Fprintf(&b, "  %s\n", k)
		}
		fmt.Fprintf(&b, "The file wins at serve time. Edit %s if this shell's value is the one you want.\n", envFile)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "\nWarning: %s\n", w)
	}
	if mergeErr != nil {
		sorted := make([]string, 0, len(captured))
		for k := range captured {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\nCould not write %s: %v\n"+
			"These did NOT reach the executor's environment: %s\n"+
			"Add them by hand to the file and chmod 600 it.\n",
			envFile, mergeErr, strings.Join(sorted, ", "))
	}
	return b.String()
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
			Handler: executorHandler(srv, nil),
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

// executorPathEnv is the PATH baked into the EXECUTOR's unit: the installing
// shell's own PATH, verbatim.
//
// buildPathEnv — the daemon's choice — curates a minimal list around the
// binaries IT spawns (pi, claude) plus standard directories. The executor has
// no such fixed inventory: language-server auto-detection resolves whatever
// is installed on its PATH, and every bash/git/toolchain call inherits it, so
// curation here is guessing at the operator's toolchain from inside the
// installer. The shell's PATH is what worked interactively; it is what works
// supervised.
//
// Keeping PATH out of executor.env is deliberate regardless: LoadEnvFile
// never overrides a variable the process already has, so a copy in the file
// would be inert — the unit's EnvironmentVariables entry always wins. The
// override lives where it takes effect.
func executorPathEnv() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return buildPathEnv()
}

func credFileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// captureExecutorEnv sorts an installing shell's environment into the
// variables worth carrying into the supervised executor and the ones that are
// not. It is deliberately BROAD — unlike captureDaemonEnv's narrow rafiki-only
// list — because the executor runs the operator's local toolchain directly:
// bash, git and language servers all inherit this process's environment, so a
// variable like GOPATH or http_proxy is load-bearing there even though it has
// no meaning to the executor itself. Everything captured goes to the 0600
// executor environment file, not the unit, so breadth costs no exposure.
//
// Unset and empty variables are dropped, same as captureDaemonEnv.
//
// Only two variables are excluded as UNIT-OWNED:
//
//   - HOME is baked into every unit (spec.HomeEnv).
//   - PATH is baked into the unit from the installing shell's own PATH
//     (executorPathEnv), because LoadEnvFile never overrides a variable the
//     process already has — a copy of PATH in the file would be inert.
//
// Three further classes are skipped, each for a concrete failure:
//
//   - Reserved rafiki variables (RAFIKI_*, retired FUNDI_*/PI_CONTROLLER_*)
//     and the two provider keys, via paths.IsReservedEnvKey. Real captures
//     carried RAFIKI_DB — a credentialed DSN — plus RAFIKI_URL and both API
//     keys. collectCallerEnv strips exactly these from a caller before
//     spawning a child; capture must enforce the same boundary or every bash
//     tool child on the executor inherits them from this file.
//   - Shell session state: PWD/OLDPWD (wherever install happened to stand),
//     SHLVL, _ (the last command), GPG_TTY (a per-boot /dev/ttysN path gpg
//     computes per-invocation anyway), and direnv's DIRENV_* bookkeeping —
//     transient diff/watch blobs for one prompt, with DIRENV_DIR claiming a
//     "current directory" that will be frozen wrong forever.
//   - Terminal/display residue, headless by definition: TERM and its whole
//     family (including iTerm's ids, LC_TERMINAL*, TERMINFO_DIRS,
//     COLORFGBG), DISPLAY/WAYLAND_DISPLAY, TMPDIR, SSH_AGENT_PID (OpenSSH
//     consults only SSH_AUTH_SOCK), and the macOS GUI-session identifiers a
//     real capture turned up: SECURITYSESSIONID, LaunchInstanceID, XPC_*,
//     __CFBundleIdentifier, __CF_USER_TEXT_ENCODING, COMMAND_MODE,
//     OSLogRateLimit, STARSHIP_SESSION_KEY.
//
// Everything else is captured, INCLUDING SSH_AUTH_SOCK despite being
// session-scoped: operators deliberately configure fixed agent socket paths
// precisely so they survive reboots, and ssh signing on an executor is a
// real workflow. An operator who prefers not to carry one deletes the line
// from the file — a file they own beats a filter guessing on their behalf.
var executorEnvExcluded = map[string]bool{
	"HOME":                    true,
	"PATH":                    true,
	"TMPDIR":                  true,
	"SSH_AGENT_PID":           true,
	"DISPLAY":                 true,
	"WAYLAND_DISPLAY":         true,
	"TERM":                    true,
	"COLORTERM":               true,
	"COLORFGBG":               true,
	"TERM_FEATURES":           true,
	"TERM_PROGRAM":            true,
	"TERM_PROGRAM_VERSION":    true,
	"TERM_SESSION_ID":         true,
	"TERMINFO_DIRS":           true,
	"ITERM_PROFILE":           true,
	"ITERM_SESSION_ID":        true,
	"LC_TERMINAL":             true,
	"LC_TERMINAL_VERSION":     true,
	"PWD":                     true,
	"OLDPWD":                  true,
	"SHLVL":                   true,
	"_":                       true,
	"GPG_TTY":                 true,
	"DIRENV_DIFF":             true,
	"DIRENV_WATCHES":          true,
	"DIRENV_DIR":              true,
	"DIRENV_FILE":             true,
	"SECURITYSESSIONID":       true,
	"LaunchInstanceID":        true,
	"XPC_FLAGS":               true,
	"XPC_SERVICE_NAME":        true,
	"__CFBundleIdentifier":    true,
	"__CF_USER_TEXT_ENCODING": true,
	"COMMAND_MODE":            true,
	"OSLogRateLimit":          true,
	"STARSHIP_SESSION_KEY":    true,
}

// captureExecutorEnv returns environ minus the exclusions above. It does NOT
// consult ExecutorEnvFileEnv: that variable is handled separately by install,
// which must bake it into the unit whether or not the file itself captured it.
func captureExecutorEnv(environ []string) map[string]string {
	out := make(map[string]string)
	for _, e := range environ {
		k, v, ok := strings.Cut(e, "=")
		if !ok || v == "" {
			continue
		}
		if executorEnvExcluded[k] || paths.IsReservedEnvKey(k) {
			continue
		}
		out[k] = v
	}
	return out
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
