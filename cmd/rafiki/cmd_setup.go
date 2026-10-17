// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/version"
)

// The steps `rafiki setup` walks, in order. Each one that succeeds records its
// name in install.toml's last_step, so a failure leaves a resumable record and
// a re-run knows how far it got.
const (
	stepLoad      = "load"
	stepPrereq    = "prerequisites"
	stepDatabase  = "database"
	stepMigrate   = "migrate"
	stepProviders = "providers"
	stepDaemon    = "daemon"
	stepIdentity  = "identity"
	stepExecutor  = "executor"
)

// exposeWarning is printed when the proxy face is bound to every interface, and
// is folded into the prompt title so it is read before the question is answered.
const exposeWarning = "the proxy face is plain HTTP — every token (CLI, MCP, claude children) crosses the network in cleartext; only expose on networks you trust"

// setupSocketTimeout bounds the wait for the daemon's socket to accept a
// connection. A package var so tests can shorten it.
var setupSocketTimeout = 30 * time.Second

// setupProxyListenLoopback and setupProxyListenAll are the two bind addresses
// setup writes to RAFIKI_PROXY_LISTEN.
const (
	setupProxyListenLoopback = "127.0.0.1:8035"
	setupProxyListenAll      = ":8035"
)

func newSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up rafiki on this machine",
		Long: `Set up rafiki on this machine: prerequisites, a database, LLM providers (providers.toml), the daemon service, your identity, and a local executor.

It is safe to re-run. Every answer is saved to ` + installstate.FileName + ` and
every secret (the database DSN and your API keys) to ` + filepath.Base(paths.ServiceEnvFile()) + ` (0600),
so a re-run resumes from where it got to and only asks for what changed.

With --non-interactive no question is asked: every answer comes from a flag, a
saved answer, or the default — the path a script, a reinstall, or a config
upgrade uses. Without a terminal, setup refuses to run unless --non-interactive
is given.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTerminalOrNonInteractive(cmd, term.IsTerminal(int(os.Stdin.Fd()))); err != nil {
				return err
			}
			return runSetup(cmdCtx(cmd), cmd, newWizard(cmd), execRunner{}, cmdOut(cmd))
		},
	}
	cmd.Flags().Bool("non-interactive", false, "Never prompt; use flags, saved answers, then defaults")
	cmd.Flags().String("dsn", "", "Database DSN (postgres://...) instead of a managed container")
	cmd.Flags().String("user", "", "Your rafiki user name")
	cmd.Flags().String(keyOpenRouterFlag, "", "OpenRouter API key")
	cmd.Flags().String(keyAnthropicFlag, "", "Anthropic API key")
	cmd.Flags().Bool("expose", false, "Make the daemon reachable from other machines on your network")
	cmd.Flags().StringSlice("with", nil, "Optional components to install (uv, ollama, rtk)")

	// The local executor-setup flags, registered under the same names as
	// newExecutorSetupCmd's so runExecutorSetup reads them from this command's
	// flags. The remote-daemon flags (--connect, --pin-cert, --server-name,
	// --enroll-token) are only on `rafiki executor setup`: this command sets up
	// the executor for the daemon it just installed.
	cmd.Flags().String("root", "", "default working directory for agents on this machine (not a sandbox; default: your home directory)")
	cmd.Flags().StringSlice("launch", nil, "agent kinds this machine hosts: claude, script (repeatable; default: whichever of claude and python3 are on PATH)")
	cmd.Flags().StringSlice("env", nil, "environment variable names the executor's tools inherit (repeatable)")
	cmd.Flags().String("rtk", "", "rewrite known commands through rtk: auto|on|off")
	return cmd
}

// cmdOut returns the command's stdout, falling back to os.Stdout.
func cmdOut(cmd *cobra.Command) io.Writer {
	if cmd != nil {
		return cmd.OutOrStdout()
	}
	return os.Stdout
}

// runSetup is the whole `rafiki setup` flow. r is the subprocess seam (tests
// inject a fake), and out is where the step log is written.
func runSetup(ctx context.Context, cmd *cobra.Command, w *wizard, r runner, out io.Writer) error {
	// ── 1. load ─────────────────────────────────────────────────────────────
	step(out, stepLoad)
	st, _, err := installstate.Load()
	if err != nil {
		return fmt.Errorf("%s: %w", installstate.Path(), err)
	}
	if st.Channel == installstate.ChannelUnset {
		exe, _ := os.Executable()
		if resolved, lerr := filepath.EvalSymlinks(exe); lerr == nil {
			exe = resolved
		}
		st.Channel = installstate.DetectChannel(exe, version.Version)
	}
	savedEnv, err := readEnvFile(paths.ServiceEnvFile())
	if err != nil {
		return err
	}
	if err := saveStep(st, stepLoad); err != nil {
		return err
	}
	fmt.Fprintf(out, "    ok (channel %s)\n", st.Channel)

	// ── 2. prerequisites ────────────────────────────────────────────────────
	step(out, stepPrereq)
	optional, err := checkPrereqs(ctx, r, w, runtime.GOOS, st.Optional)
	if err != nil {
		return err
	}
	st.Optional = optional
	if err := saveStep(st, stepPrereq); err != nil {
		return err
	}
	fmt.Fprintf(out, "    ok (optional: %s)\n", joinOrNone(optional))

	// envChanged accumulates across steps 3, 5 and 6: an already-installed
	// daemon is restarted only when one of them actually rewrote service.env
	// or providers.toml — a rewritten providers.toml also needs a restart, as
	// the daemon reads it once at startup.
	envChanged := false
	track := func(res paths.SetResult) {
		if len(res.Added) > 0 || len(res.Replaced) > 0 {
			envChanged = true
		}
	}

	// ── 3. database ─────────────────────────────────────────────────────────
	step(out, stepDatabase)
	var dbRes dbResult
	st.DB, dbRes, err = setupDatabase(ctx, r, w, st.DB, savedEnv["RAFIKI_DB"])
	if err != nil {
		return err
	}
	setRes, err := paths.SetEnvFile(paths.ServiceEnvFile(), map[string]string{"RAFIKI_DB": dbRes.DSN}, "rafiki setup")
	if err != nil {
		return err
	}
	track(setRes)
	if err := saveStep(st, stepDatabase); err != nil {
		return err
	}
	fmt.Fprintf(out, "    ok (mode %s)\n", dbRes.Mode)

	// ── 4. migrate ──────────────────────────────────────────────────────────
	step(out, stepMigrate)
	rafikidPath, err := findDaemonBinary()
	if err != nil {
		return err
	}
	if err := migrateDatabase(ctx, r, rafikidPath, dbRes.DSN); err != nil {
		return err
	}
	if err := saveStep(st, stepMigrate); err != nil {
		return err
	}
	fmt.Fprintln(out, "    ok (schema up to date)")

	// ── 5. providers ────────────────────────────────────────────────────────
	step(out, stepProviders)
	prov, err := runProvidersStep(ctx, w, r, out, &st, savedEnv)
	if err != nil {
		return err
	}
	if prov.Changed {
		envChanged = true
	}
	if err := saveStep(st, stepProviders); err != nil {
		return err
	}
	if len(prov.Providers) == 0 {
		fmt.Fprintln(out, "    ok (no providers; claude agents use your Claude login)")
	} else {
		fmt.Fprintf(out, "    ok (%s; default %s)\n", strings.Join(prov.Providers, ", "), prov.Default)
	}

	// ── 6. daemon ───────────────────────────────────────────────────────────
	step(out, stepDaemon)
	// askConfirm carries no description, so the cleartext warning rides the
	// title — the point is that it is seen BEFORE the question is answered.
	expose, err := w.askConfirm("Reachable from other machines on your network? ("+exposeWarning+")", "expose", st.Daemon.Expose, false)
	if err != nil {
		return err
	}
	listen := setupProxyListenLoopback
	if expose {
		listen = setupProxyListenAll
	}
	// An operator may have hand-set RAFIKI_PROXY_LISTEN to something of their
	// own (a non-loopback interface, a different port). Setup owns only its two
	// canonical values, so it writes only when the file is silent or already
	// carries one of them — never silently repointing a deliberate choice.
	if cur := savedEnv["RAFIKI_PROXY_LISTEN"]; cur != "" && cur != setupProxyListenLoopback && cur != setupProxyListenAll {
		fmt.Fprintf(out, "    note: keeping RAFIKI_PROXY_LISTEN=%s\n", cur)
	} else {
		setRes, err = paths.SetEnvFile(paths.ServiceEnvFile(), map[string]string{"RAFIKI_PROXY_LISTEN": listen}, "rafiki setup")
		if err != nil {
			return err
		}
		track(setRes)
	}
	st.Daemon.Expose = &expose
	if expose {
		fmt.Fprintf(out, "    warning: %s\n", exposeWarning)
	}

	installed, err := daemonServiceInstalled()
	if err != nil {
		return err
	}
	switch {
	case !installed:
		if err := installDaemonService(daemonServiceOptions{CaptureShellEnv: true}, out); err != nil {
			return err
		}
		fmt.Fprintln(out, "    ok (service installed)")
	case envChanged:
		restart, err := confirmRestart(ctx, w, out)
		if err != nil {
			return err
		}
		if restart {
			if err := restartDaemonService(); err != nil {
				return err
			}
			fmt.Fprintln(out, "    ok (service restarted)")
		} else {
			fmt.Fprintln(out, "    ok (restart declined)")
		}
	default:
		fmt.Fprintln(out, "    ok (service already installed)")
	}

	if err := awaitSocket(ctx, out); err != nil {
		return err
	}
	if err := saveStep(st, stepDaemon); err != nil {
		return err
	}

	// ── 7. identity ─────────────────────────────────────────────────────────
	step(out, stepIdentity)
	profileName, err := runIdentityStep(ctx, w, r, out, &st, dbRes.DSN, rafikidPath)
	if err != nil {
		return err
	}

	// ── 8. executor ─────────────────────────────────────────────────────────
	step(out, stepExecutor)
	ref, lerr := resolveLaunchExecutor(ctx, cmd, profileName, protocol.KindClaude)
	if lerr != nil {
		// A real lookup failure (daemon down, auth) must not read as "no
		// executor exists": say so before falling back to setup.
		fmt.Fprintf(out, "    note: could not check for a live executor (%v); continuing to executor setup\n", lerr)
	}
	if lerr == nil && ref != "" {
		fmt.Fprintf(out, "    ok (executor %s is live)\n", ref)
	} else {
		if err := runExecutorSetup(ctx, w, out); err != nil {
			return err
		}
		// runExecutorSetup saved its own state; pick it back up so the step
		// record and the executor fields stay in sync. A failed reload must not
		// be swallowed: writing the stale pre-executor `st` would clobber the
		// answers the executor step just saved.
		s, _, lerr := installstate.Load()
		if lerr != nil {
			return lerr
		}
		st = s
	}
	if err := saveStep(st, stepExecutor); err != nil {
		return err
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "rafiki is ready. Try: rafiki create")
	switch {
	case prov.OllamaModel != "":
		fmt.Fprintf(out, "  (or a local model: rafiki create --kind fundi --model ollama/%s)\n", prov.OllamaModel)
	case len(prov.Providers) > 0:
		fmt.Fprintln(out, "  (or: rafiki create --kind fundi --model <model>)")
	}
	return nil
}

// step prints one step header.
func step(out io.Writer, name string) {
	fmt.Fprintf(out, "==> %s\n", name)
}

// saveStep records the last completed step and persists the state.
func saveStep(st installstate.State, name string) error {
	st.LastStep = name
	return installstate.Save(st)
}

// awaitSocket waits for the daemon to accept connections on its socket. A
// daemon that never comes up is a FAILURE, not a warning: marking the daemon
// step done would send the operator into the next step's opaque "did not
// accept the new token" instead of at the daemon they need to look at. The
// service log tail and the log path are printed so the cause is visible.
func awaitSocket(ctx context.Context, out io.Writer) error {
	wctx, cancel := context.WithTimeout(ctx, setupSocketTimeout)
	defer cancel()
	if err := waitForSocket(wctx, paths.SocketPath()); err != nil {
		if tail := lastServiceLogLines(20); tail != "" {
			fmt.Fprintln(out, tail)
		}
		return fmt.Errorf("the daemon did not answer on %s within %s: %w\ncheck its log at %s",
			paths.SocketPath(), setupSocketTimeout, err, paths.ServiceLogPath())
	}
	fmt.Fprintln(out, "    ok (daemon socket is up)")
	return nil
}

// lastServiceLogLines returns the last n lines of the daemon's service log, or
// "" when there is no log to read.
func lastServiceLogLines(n int) string {
	b, err := os.ReadFile(paths.ServiceLogPath())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// confirmRestart reports whether the daemon should be restarted. When the
// daemon answers Status with live children it warns first (restarting kills
// running agents) and, interactively, asks to confirm. A daemon that does not
// answer, or has no live children, restarts without a prompt.
func confirmRestart(ctx context.Context, w *wizard, out io.Writer) (bool, error) {
	n, err := liveChildCount(ctx)
	if err == nil && n > 0 {
		fmt.Fprintf(out, "    restarting rafikid will interrupt %d running agent(s)\n", n)
		if w.interactive {
			return w.confirmRun([]string{"rafiki", "service", "restart"})
		}
	}
	return true, nil
}

// liveChildCount asks the local daemon (anonymous, over its own socket — the
// identity step has not run yet) how many children are live.
func liveChildCount(ctx context.Context) (int, error) {
	ep, err := connectEndpointFor(profile.Resolved{Profile: profile.Profile{Socket: paths.SocketPath()}})
	if err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := ep.control().Status(cctx, connect.NewRequest(&rafikiv1.StatusRequest{}))
	if err != nil {
		return 0, err
	}
	return int(resp.Msg.GetChildren().GetLive()), nil
}

// setupEndpoint resolves the Connect endpoint for a NAMED profile, without
// going through newConnectEndpoint's process-global selection: `rafiki setup`
// must target the profile it is creating even before that profile is the
// selected one. It reuses connectEndpointFor, the same core newConnectEndpoint
// calls, so the endpoint a verb would dial is the endpoint setup checks.
func setupEndpoint(profileName string) (connectEndpoint, error) {
	set, err := profile.Load()
	if err != nil {
		return connectEndpoint{}, err
	}
	p, ok := set.Get(profileName)
	if !ok {
		return connectEndpoint{}, fmt.Errorf("profile %q is not configured yet", profileName)
	}
	return connectEndpointFor(profile.Resolved{Profile: p, Token: profile.ReadToken(profileName)})
}

// readEnvFile parses an environment file into a map without touching the
// process environment. pkg/paths exposes no parse-only reader (LoadEnvFile
// mutates the process env), so this snapshots os.Environ, clears it, runs the
// package loader against an empty environment — so the file's own values are
// what land — captures them, and restores. A missing file is an empty map, not
// an error, matching LoadEnvFile's optional-config contract.
func readEnvFile(path string) (map[string]string, error) {
	saved := os.Environ()
	os.Clearenv()
	_, _, err := paths.LoadEnvFile(path)
	out := make(map[string]string)
	for _, e := range os.Environ() {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	os.Clearenv()
	for _, e := range saved {
		if k, v, ok := strings.Cut(e, "="); ok {
			_ = os.Setenv(k, v)
		}
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
