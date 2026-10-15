package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
)

// `rafiki executor setup` is the interactive counterpart to
// `rafiki executor service install`: it asks the handful of questions an
// operator would otherwise have to spell out as flags, records the answers in
// install.toml for a later replay, and then installs the service. It is the
// executor half of `rafiki setup`, and the whole flow lives in runExecutorSetup
// so both callers run the same code.

func newExecutorSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Interactively set up this machine's executor",
		Long: `Set up this machine as a rafiki executor, asking a few questions.

	The answers are saved to ` + installstate.FileName + ` so a reinstall or update can
	replay them, and the executor is installed as a per-user service (the same
	unit ` + "`rafiki executor service install`" + ` writes).

	With --yes every question takes its default (or the value a saved answer or
	flag supplies) and no form is shown, which is the path a script or a
	reinstall uses.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExecutorSetup(cmdCtx(cmd), newWizard(cmd), os.Stdout)
		},
	}
	cmd.Flags().Bool("yes", false, "Accept every default without prompting")
	cmd.Flags().String("connect", "", "daemon to connect to (host:port; blank means this machine's daemon)")
	cmd.Flags().String("root", "", "directory agents may work in")
	cmd.Flags().StringSlice("launch", nil, "agent kinds this machine hosts (repeatable)")
	cmd.Flags().StringSlice("env", nil, "environment variable names the executor's tools inherit (repeatable)")
	cmd.Flags().String("rtk", "", "rewrite known commands through rtk: auto|on|off")
	cmd.Flags().String("pin-cert", "", "SHA-256 fingerprint of the daemon's leaf certificate")
	cmd.Flags().String("server-name", "", "TLS server name (SNI), when it differs from the connect host")
	cmd.Flags().String("enroll-token", "", "one-time enrollment token, for the first connect")
	return cmd
}

// defaultExecutorEnv are the environment variable NAMES a fresh setup
// preselects, drawn from the names captureExecutorEnv would carry: locale, the
// editor, Claude Code's config dir, and the language-toolchain homes whose
// absence breaks an editor or language server under a login-shell-less unit.
// They are only a preselection — the operator can carry any name, and a saved
// answer always outranks this list.
var defaultExecutorEnv = []string{
	"LANG", "LC_ALL", "LC_CTYPE", "EDITOR", "VISUAL", "CLAUDE_CONFIG_DIR",
	"GOPATH", "CARGO_HOME", "RUSTUP_HOME", "PNPM_HOME", "NVM_DIR",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
}

// executorEnrollClient mints a one-time enrollment token over the current
// profile, exactly as `rafiki executor enroll` does (same defaults for
// isolation and workspace mode, same one-hour TTL). A package var so tests can
// substitute a stub; the returned error keeps its Connect code so the caller
// can tell a permission refusal from a transport failure.
var executorEnrollClient = func(ctx context.Context, cmd *cobra.Command, name string, roots []string) (string, error) {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return "", err
	}
	resp, err := ep.control().EnrollExecutor(ctx, connect.NewRequest(&rafikiv1.EnrollExecutorRequest{
		Name:          name,
		Roots:         roots,
		Isolation:     "none",
		WorkspaceMode: "pinned",
		Ttl:           durationpb.New(time.Hour),
	}))
	if err != nil {
		return "", fmt.Errorf("mint enrollment token via %s: %w", ep.describe, err)
	}
	return resp.Msg.GetToken(), nil
}

// executorSetupWaitLive waits until the freshly installed executor reports
// live, through waitExecutorLive. A package var so tests can substitute a stub;
// production resolves the endpoint from the command's profile and passes no
// stream-end channel (waitExecutorLive tolerates a nil channel — its select
// only reaches the receive in the ctx.Done branch, guarded by a default).
var executorSetupWaitLive = func(ctx context.Context, cmd *cobra.Command, selector string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	return waitExecutorLive(ctx, ep.control(), selector, nil)
}

// runExecutorSetup is the whole executor-setup flow, driven by w; the cobra
// RunE is a one-line wrapper. Exposed so `rafiki setup` and `rafiki create`'s
// missing-executor prompt call the same code.
func runExecutorSetup(ctx context.Context, w *wizard, out io.Writer) error {
	st, _, err := installstate.Load()
	if err != nil {
		return fmt.Errorf("%s: %w", installstate.Path(), err)
	}

	// Connect target. Blank means this machine's own daemon, reached over its
	// executor socket; a host:port means a remote daemon over TLS.
	connectAddr, err := w.askString("Daemon to connect to (blank = this machine's daemon)", "connect", st.Executor.Address, "", false)
	if err != nil {
		return err
	}
	var connectSocket, pinCert, serverName string
	if connectAddr == "" {
		connectSocket = paths.ExecutorSocketPath()
		st.Executor.Connect = installstate.ExecutorConnectSocket
		st.Executor.Address = ""
		st.Executor.PinCert = ""
		st.Executor.ServerName = ""
	} else {
		st.Executor.Connect = installstate.ExecutorConnectTCP
		st.Executor.Address = connectAddr
		if pinCert, err = w.askString("SHA-256 fingerprint of the daemon's certificate (optional)", "pin-cert", st.Executor.PinCert, "", false); err != nil {
			return err
		}
		if serverName, err = w.askString("TLS server name, if it differs from the host (optional)", "server-name", st.Executor.ServerName, "", false); err != nil {
			return err
		}
		st.Executor.PinCert = pinCert
		st.Executor.ServerName = serverName
	}

	home, _ := os.UserHomeDir()
	root, err := w.askString("Directory agents may work in", "root", st.Executor.Root, home, true)
	if err != nil {
		return err
	}

	var launchDetected []string
	if _, err := exec.LookPath("claude"); err == nil {
		launchDetected = []string{"claude"}
	}
	launch, err := w.askMulti("Agent kinds this machine hosts", "launch", []string{"claude"}, st.Executor.Launch, launchDetected)
	if err != nil {
		return err
	}

	// Env carry-over. The candidates are exactly the names captureExecutorEnv
	// would carry from this shell; the detected preselection is those of
	// defaultExecutorEnv that are actually set. A name saved from an earlier
	// run but no longer set in this shell stays saved — it is listed as not set
	// and left out of the file, since its value cannot be read.
	candidates := sortedNames(captureExecutorEnv(os.Environ()))
	detectedEnv := intersectNames(defaultExecutorEnv, candidates)
	chosen, err := w.askMulti("Environment variables the executor's tools inherit", "env", candidates, st.Executor.Env, detectedEnv)
	if err != nil {
		return err
	}
	var notSet []string
	for _, n := range st.Executor.Env {
		if !containsString(candidates, n) {
			notSet = append(notSet, n)
		}
	}
	savedEnv := append(append([]string{}, chosen...), notSet...)

	// Enrollment token, only when there is no credential file yet. The token is
	// consumed by the first connect, so a machine that already holds a
	// credential never needs one.
	enrollToken, _ := w.cmd.Flags().GetString("enroll-token")
	credentialFile := filepath.Join(paths.DataDir(), "executor.cred")
	if enrollToken == "" && !credFileExists(credentialFile) {
		name, _, err := paths.MachineName()
		if err != nil {
			return err
		}
		token, err := executorEnrollClient(ctx, w.cmd, name, []string{root})
		if err != nil {
			if code := connect.CodeOf(err); code == connect.CodePermissionDenied || code == connect.CodeUnauthenticated {
				if !w.interactive {
					p, perr := resolveProfile(w.cmd)
					if perr != nil {
						return perr
					}
					return fmt.Errorf("could not mint an enrollment token with profile %q (%v); pass --enroll-token from an admin", p.Name, err)
				}
				fmt.Fprintln(out, "Could not mint a token with this profile; paste one printed by `rafiki executor enroll` on a machine with admin access.")
				if token, err = w.askString("Enrollment token", "enroll-token", "", "", true); err != nil {
					return err
				}
			} else {
				return err
			}
		}
		enrollToken = token
	}

	// Summary before installing — names only, never values. Interactive asks to
	// confirm; --yes and a piped stdin take confirmRun's answer.
	fmt.Fprint(out, executorSetupSummary(connectAddr, connectSocket, root, launch, savedEnv))
	confirmed, err := w.confirmRun([]string{"rafiki", "executor", "service", "install"})
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(out, "Aborted.")
		return nil
	}

	if err := installExecutorService(executorInstallOptions{
		Connect:        connectAddr,
		ConnectSocket:  connectSocket,
		EnrollToken:    enrollToken,
		CredentialFile: credentialFile,
		Root:           root,
		PinCert:        pinCert,
		ServerName:     serverName,
		RTK:            st.Executor.RTK,
		Launch:         launch,
		EnvNames:       chosen,
	}, out); err != nil {
		return err
	}

	// Wait until the executor the unit started reports live, so the operator
	// learns immediately if it cannot connect rather than at the first spawn.
	name, _, err := paths.MachineName()
	if err != nil {
		return err
	}
	if err := executorSetupWaitLive(ctx, w.cmd, "machine="+name); err != nil {
		return err
	}

	st.Executor.Root = root
	st.Executor.Launch = launch
	st.Executor.Env = savedEnv
	if err := installstate.Save(st); err != nil {
		return fmt.Errorf("save install state: %w", err)
	}
	fmt.Fprintln(out, "\nExecutor ready.")
	return nil
}

// executorSetupSummary renders the pre-install summary: the target, root, launch
// kinds and the env variable NAMES. Values are deliberately absent — this text
// is printed to a terminal that may be read by someone else.
func executorSetupSummary(target, connectSocket, root string, launch, envNames []string) string {
	if target == "" {
		target = "unix:" + connectSocket
	}
	return fmt.Sprintf("Target: %s\nRoot:   %s\nLaunch: %s\nEnv:    %s\n",
		target, root, joinOrNone(launch), joinOrNone(envNames))
}

func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// sortedNames returns the keys of m, sorted. Sorting is what keeps the
// MultiSelect options stable from run to run.
func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intersectNames returns the members of want that are also in have, in want's
// order.
func intersectNames(want, have []string) []string {
	var out []string
	for _, w := range want {
		if containsString(have, w) {
			out = append(out, w)
		}
	}
	return out
}
