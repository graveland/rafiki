package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
)

// serviceBackend abstracts per-OS service management operations.
type serviceBackend interface {
	Install(spec serviceSpec) error
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	Status() (serviceStatus, error)
	LogPath() string
}

// serviceSpec carries the information needed to write the service unit.
type serviceSpec struct {
	DaemonBinary string
	// Args are the arguments the unit passes to DaemonBinary. Empty for the
	// daemon, which takes none; the executor unit uses them to run
	// `rafiki executor serve --connect …`.
	Args    []string
	PathEnv string
	HomeEnv string
	// LogPath is where the unit sends the daemon's stdout/stderr. Resolved at
	// install time and baked in as an absolute path: launchd and systemd do not
	// inherit the interactive shell's XDG_* variables, so a unit that referenced
	// them would resolve differently from every CLI invocation.
	LogPath string
	// ExtraEnv are the rafiki variables baked into the unit so the daemon sees
	// them. See unitEnvVars.
	ExtraEnv map[string]string
	// SecretEnv are the credentials that must not go in a world-readable unit.
	// `service install` merges them into paths.ServiceEnvFile. See secretEnvVars.
	SecretEnv map[string]string
	// SkippedEnv names variables that were set but could not be represented in
	// a unit file. Reported at install time so they are not lost silently.
	SkippedEnv []string
}

// unitEnvVars are the rafiki variables that configure the DAEMON and are safe
// to write into a service definition. captureDaemonEnv bakes them into the
// launchd plist / systemd unit at install time.
//
// These are daemon-environment-only: `rafiki create --forward-env` forwards
// the caller's environment to a child, but collectCallerEnv strips every
// RAFIKI_* key first, so exporting them in an interactive shell does nothing
// at all. launchd and systemd do not inherit a login shell either. The unit
// and secretEnvVars' environment file are the only two places they can be set.
//
// Deliberately absent:
//   - paths.ChildID — the daemon sets it per child and `rafikid agent` uses it
//     as the default --ref, so pinning it service-wide would collide every
//     child onto one conversation.
//   - paths.AttachTail, paths.NoAutoInstallHelpers — read by the TUI and by
//     `rafiki create` respectively, i.e. client-side. They reach those from the
//     user's own shell and have no business in the daemon's unit.
//
// paths.URL, paths.ProxyKinds and paths.ProxyListen ARE captured here: a URL,
// a list of kinds and a bind address are not secrets, and having them in the
// unit makes the routing visible to anyone reading the service definition.
//
// paths.EnvFile is here too, and for a related reason: it is a path override
// for paths.ServiceEnvFile, not a credential. If it is absent from the unit,
// an operator with $RAFIKI_ENV_FILE exported gets secrets written to that
// custom path at install time while the daemon — which never sees the
// installing shell's environment — falls back to the default
// ~/.config/rafiki/service.env and finds nothing there. Baking it in keeps
// the daemon resolving the same file the installer just wrote.
var unitEnvVars = []string{
	paths.Socket,
	paths.Instructions,
	paths.SkillsDirsEnv,
	paths.MCPConfig,
	paths.DefaultModel,
	paths.DefaultPreset,
	paths.DefaultLabels,
	paths.GraceHours,
	paths.PiBinary,
	paths.URL,
	paths.ProxyKinds,
	paths.ProxyListen,
	paths.EnvFile,
}

// The two third-party credential names. They stay literals rather than moving
// into pkg/paths/envvar.go, whose header states it names the variables rafiki
// owns — these belong to Anthropic and OpenRouter.
const (
	anthropicAPIKeyEnv  = "ANTHROPIC_API_KEY"
	openrouterAPIKeyEnv = "OPENROUTER_API_KEY"
)

// secretEnvVars are the daemon variables that must NOT be written into a unit
// file: launchd plists and systemd units are world-readable (0644). They go to
// paths.ServiceEnvFile instead, which MergeEnvFile creates 0600 and the daemon
// reads at startup.
//
// paths.DB is here because a postgres DSN routinely carries a password. It
// used to sit in the captured list — the same list whose comment already
// excluded the API keys for exactly this reason — so the rule the code stated
// was not the rule it followed.
var secretEnvVars = []string{
	paths.DB,
	paths.Token,
	anthropicAPIKeyEnv,
	openrouterAPIKeyEnv,
}

// captureDaemonEnv sorts the variables set in environ (in os.Environ "K=V"
// form) into the ones that belong in the unit, the ones that belong in the
// environment file, and the ones it had to skip.
//
// Unset and empty variables are omitted rather than written through: an empty
// RAFIKI_DB is not the same as an absent one to the daemon's "is persistence
// configured" check, and writing one would turn a missing export into a
// configured-but-broken DSN.
//
// A unit value containing a newline is skipped, because no unit file can hold
// one: systemd's Environment= is line-based, and a launchd plist would carry it
// but only the systemd side would then be broken — a difference that shows up
// as a service that installs on one platform and not the other. Secrets carry
// no such restriction: MergeEnvFile quotes and escapes, which is what lets a
// newline-bearing value live in service.env at all.
//
// ANTHROPIC_CUSTOM_HEADERS is neither a unitEnvVars nor a secretEnvVars entry
// — install never captures it, from the caller's shell or anywhere else. An
// operator who needs it sets it directly in service.env by hand; see
// .env.example.
func captureDaemonEnv(environ []string) (unit, secret map[string]string, skipped []string) {
	wantUnit := make(map[string]bool, len(unitEnvVars))
	for _, k := range unitEnvVars {
		wantUnit[k] = true
	}
	wantSecret := make(map[string]bool, len(secretEnvVars))
	for _, k := range secretEnvVars {
		wantSecret[k] = true
	}

	unit = make(map[string]string)
	secret = make(map[string]string)
	for _, e := range environ {
		k, v, ok := strings.Cut(e, "=")
		if !ok || v == "" {
			continue
		}
		switch {
		case wantSecret[k]:
			secret[k] = v
		case wantUnit[k]:
			if strings.ContainsAny(v, "\n\r") {
				skipped = append(skipped, k)
				continue
			}
			unit[k] = v
		}
	}
	sort.Strings(skipped)
	return unit, secret, skipped
}

// envKV is one captured variable. Templates render a sorted slice rather than
// ranging a map so that reinstalling with an unchanged environment produces a
// byte-identical unit file instead of one with randomly reordered keys.
type envKV struct{ Key, Value string }

func sortedEnv(m map[string]string) []envKV {
	out := make([]envKV, 0, len(m))
	for k, v := range m {
		out = append(out, envKV{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// serviceStatus describes the current state of the installed service.
type serviceStatus struct {
	Installed bool
	Running   bool
	PID       int
	Detail    string
}

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "service",
		Aliases: []string{"svc"},
		Short:   "Manage the rafiki daemon as a system service",
		Long: `Install, start, stop, and inspect the rafiki daemon as a per-user system service.

On macOS this uses launchd (launchctl); on Linux it uses systemd --user.`,
	}
	cmd.AddCommand(
		newServiceInstallCmd(),
		newServiceUninstallCmd(),
		newServiceStartCmd(),
		newServiceStopCmd(),
		newServiceRestartCmd(),
		newServiceStatusCmd(),
		newServiceLogsCmd(),
		newServiceTailCmd(),
	)
	return cmd
}

func newServiceInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the rafiki service",
		Args:  cobra.NoArgs,
		RunE:  runServiceInstall,
	}
	cmd.Flags().String("daemon-binary", "", "Path to the rafiki daemon binary (default: auto-detect next to rafiki, then PATH)")
	cmd.Flags().String("path-env", "", "PATH value for the service environment (default: auto-detect)")
	return cmd
}

func newServiceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the rafiki service",
		Args:  cobra.NoArgs,
		RunE:  runServiceUninstall,
	}
}

func newServiceStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the rafiki service",
		Args:  cobra.NoArgs,
		RunE:  runServiceStart,
	}
}

func newServiceStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the rafiki service",
		Args:  cobra.NoArgs,
		RunE:  runServiceStop,
	}
}

func newServiceRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the rafiki service",
		Args:  cobra.NoArgs,
		RunE:  runServiceRestart,
	}
}

func newServiceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show installation and running state of the rafiki service",
		Args:  cobra.NoArgs,
		RunE:  runServiceStatus,
	}
}

func newServiceLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the rafiki daemon log and exit",
		Long:  "Print the rafiki daemon log and exit. Use --follow (or `rafiki service tail`) to keep streaming.",
		Args:  cobra.NoArgs,
		RunE:  runServiceLogs,
	}
	cmd.Flags().BoolP("follow", "f", false, "Keep streaming new log output instead of exiting at EOF")
	return cmd
}

func newServiceTailCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tail",
		Short: "Follow the rafiki daemon log",
		Long:  "Print the rafiki daemon log and keep streaming new output. Equivalent to `rafiki service logs --follow`.",
		Args:  cobra.NoArgs,
		RunE:  runServiceTail,
	}
}

// --- run functions ---

func runServiceInstall(cmd *cobra.Command, _ []string) error {
	spec, err := buildServiceSpec(cmd)
	if err != nil {
		return err
	}

	// Write secrets to service.env BEFORE starting the daemon. b.Install below
	// bootstraps the launchd job (RunAtLoad) or runs `systemctl enable --now`,
	// either of which spawns the daemon immediately — and the daemon reads
	// service.env once, at startup (loadServiceEnv, very early in
	// cmd/rafikid/main.go). Merging after install would mean the freshly
	// spawned daemon reads the file before this call ever writes it, so on a
	// clean machine it comes up with no DSN: in-memory conversations, no
	// per-turn cost, until the next restart. A merge failure still does not
	// fail the install (see installReport's mergeErr handling below) — by
	// writing first we get to report a failure AND still let the install
	// proceed, rather than trading one for the other.
	envFile := paths.ServiceEnvFile()
	res, mergeErr := paths.MergeEnvFile(envFile, spec.SecretEnv, "added by rafiki service install on "+time.Now().Format("2006-01-02"))

	b := newServiceBackend()
	installErr := b.Install(spec)
	if installErr == nil {
		fmt.Printf("rafiki service installed.\nLog: %s\n", b.LogPath())
	}

	// Print the report even when Install failed: the unit file and secret
	// writes above already happened on disk regardless of what Install did
	// (including a service.env chmod tightening — a real, already-applied
	// permission change) and the operator must not lose that record just
	// because the reload itself came back with an error.
	fmt.Print(installReport(spec, envFile, res, mergeErr))
	if installErr != nil {
		return fmt.Errorf("install service: %w", installErr)
	}
	return nil
}

// installReport renders what `service install` tells the operator: where each
// captured variable went, and what is still missing.
//
// Pure, and separate from the install itself, so the wording can be tested
// without writing a unit file. Values are never printed — only names. The
// whole point of routing these to a 0600 file is that they do not end up
// somewhere readable, and a terminal scrollback is readable.
func installReport(spec serviceSpec, envFile string, res paths.MergeResult, mergeErr error) string {
	var b strings.Builder

	if unit := sortedEnv(spec.ExtraEnv); len(unit) > 0 {
		b.WriteString("\nBaked into the unit (not secret):\n")
		for _, kv := range unit {
			fmt.Fprintf(&b, "  %s\n", kv.Key)
		}
	}
	if res.Tightened != 0 {
		fmt.Fprintf(&b, "\nTightened %s from %04o to 0600 — it now holds credentials.\n", envFile, res.Tightened)
	}
	if len(res.Added) > 0 {
		fmt.Fprintf(&b, "\nWritten to %s (0600):\n", envFile)
		for _, k := range res.Added {
			fmt.Fprintf(&b, "  %s\n", k)
		}
	}
	if len(res.Existing) > 0 {
		b.WriteString("\nAlready set there, left alone:\n")
		for _, k := range res.Existing {
			fmt.Fprintf(&b, "  %s\n", k)
		}
	}
	if len(res.Conflict) > 0 {
		b.WriteString("\nAlready set there and left alone, but differs from this shell's value:\n")
		for _, k := range res.Conflict {
			fmt.Fprintf(&b, "  %s\n", k)
		}
		fmt.Fprintf(&b, "The file wins. Edit %s if the shell's value is the one you want.\n", envFile)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "\nWarning: %s\n", w)
	}
	if mergeErr != nil {
		// The unit is already written and the service already running, so this
		// is not an install failure — but it is the difference between a daemon
		// that persists conversations and one that silently does not.
		//
		// MergeEnvFile categorizes into Added/Existing/Conflict BEFORE it ever
		// attempts the write (pkg/paths/envfile.go), so a write failure only
		// ever costs the keys in Added — Existing and Conflict were already
		// correctly persisted from a prior run. Naming them here as failed
		// would be false, and would send an operator hand-editing a file that
		// is already correct. Fall back to every captured secret only when the
		// failure happened before categorization ran at all (e.g. the existing
		// file could not even be read).
		failed := res.Added
		if len(res.Added) == 0 && len(res.Existing) == 0 && len(res.Conflict) == 0 {
			failed = sortedKeys(spec.SecretEnv)
		}
		fmt.Fprintf(&b, "\nCould not write %s: %v\n"+
			"These did NOT reach the daemon's environment: %s\n"+
			"Add them by hand and `chmod 600` the file.\n",
			envFile, mergeErr, strings.Join(failed, ", "))
	}

	// The DSN is the one whose absence degrades silently: conversations revert
	// to in-memory with no per-turn cost recorded, and the only visible symptom
	// is a "mem-" session id where a UUIDv7 belongs. Warn only when it is in
	// neither the shell nor the file — a DSN already in service.env is fine.
	_, inShell := spec.SecretEnv[paths.DB]
	if !inShell && !slices.Contains(res.Defined, paths.DB) {
		fmt.Fprintf(&b, "\nNo %s in this shell's environment or in %s — agent\n"+
			"conversations will be in-memory and no per-turn cost will be recorded.\n"+
			"Export it (or source your .env) and re-run `rafiki service install`.\n",
			paths.DB, envFile)
	}

	if len(spec.SkippedEnv) > 0 {
		fmt.Fprintf(&b, "\nNot baked in (a unit file cannot hold a newline): %s\n"+
			"Put these in %s instead — the daemon reads it at startup.\n",
			strings.Join(spec.SkippedEnv, ", "), envFile)
	}
	return b.String()
}

// sortedKeys returns m's keys in a deterministic order, for messages.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func runServiceUninstall(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Uninstall(); err != nil {
		return fmt.Errorf("uninstall service: %w", err)
	}
	fmt.Println("rafiki service uninstalled.")
	return nil
}

func runServiceStart(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Println("rafiki service started.")
	return nil
}

func runServiceStop(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Stop(); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	fmt.Println("rafiki service stopped.")
	return nil
}

func runServiceRestart(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Restart(); err != nil {
		return fmt.Errorf("restart service: %w", err)
	}
	fmt.Println("rafiki service restarted.")
	return nil
}

func runServiceStatus(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	st, err := b.Status()
	if err != nil {
		return fmt.Errorf("service status: %w", err)
	}

	installed := "no"
	if st.Installed {
		installed = "yes"
	}
	running := "no"
	if st.Running {
		if st.PID > 0 {
			running = fmt.Sprintf("yes (pid %d)", st.PID)
		} else {
			running = "yes"
		}
	}

	fmt.Println("rafiki service:")
	fmt.Printf("  Installed: %s\n", installed)
	fmt.Printf("  Running:   %s\n", running)
	fmt.Printf("  Log:       %s\n", b.LogPath())
	if st.Detail != "" {
		fmt.Printf("  Detail:    %s\n", st.Detail)
	}
	return nil
}

func runServiceLogs(cmd *cobra.Command, _ []string) error {
	follow, _ := cmd.Flags().GetBool("follow")
	return streamServiceLog(cmdCtx(cmd), follow)
}

func runServiceTail(cmd *cobra.Command, _ []string) error {
	return streamServiceLog(cmdCtx(cmd), true)
}

// streamServiceLog copies the daemon log to stdout. When follow is false it
// stops at EOF; when true it keeps polling for new writes until ctx is done.
func streamServiceLog(ctx context.Context, follow bool) error {
	return streamLogFile(ctx, newServiceBackend().LogPath(), follow)
}

// streamLogFile is streamServiceLog over an explicit path, so the executor
// service can reuse it without pretending to be the daemon.
func streamLogFile(ctx context.Context, logPath string, follow bool) error {

	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("log file not found: %s (has the service ever run?)", logPath)
		}
		return err
	}
	defer f.Close()

	if !follow {
		_, err = io.Copy(os.Stdout, f)
		return err
	}

	// Follow mode: stream existing content then poll for new writes.
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		n, readErr := f.Read(buf)
		if n > 0 {
			if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if readErr == io.EOF {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if readErr != nil {
			return readErr
		}
	}
}

// --- spec helpers ---

// buildServiceSpec constructs a serviceSpec from command flags and defaults.
func buildServiceSpec(cmd *cobra.Command) (serviceSpec, error) {
	spec := serviceSpec{}

	if v, _ := cmd.Flags().GetString("daemon-binary"); v != "" {
		spec.DaemonBinary = v
	} else {
		bin, err := findDaemonBinary()
		if err != nil {
			return spec, err
		}
		spec.DaemonBinary = bin
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return spec, fmt.Errorf("cannot determine home directory: %w", err)
	}
	spec.HomeEnv = home
	spec.LogPath = paths.ServiceLogPath()
	var skipped []string
	spec.ExtraEnv, spec.SecretEnv, skipped = captureDaemonEnv(os.Environ())
	spec.SkippedEnv = skipped

	if v, _ := cmd.Flags().GetString("path-env"); v != "" {
		spec.PathEnv = v
	} else {
		spec.PathEnv = buildPathEnv()
	}

	return spec, nil
}

// daemonBinaryName is the daemon's executable name. It must NOT be the client's
// own name: the client is `rafiki` and the daemon is `rafikid`, and both normally
// sit in the same directory, so a sibling lookup for "rafiki" finds the client
// and happily installs a service unit pointing at it. That fails at launch, not
// at install, which is the worst place for it to fail.
const daemonBinaryName = "rafikid"

// findDaemonBinary locates the rafiki daemon binary. It first looks for a
// sibling next to the running client executable, then falls back to PATH.
func findDaemonBinary() (string, error) {
	self, _ := os.Executable()
	return findDaemonBinaryFrom(self)
}

// findDaemonBinaryFrom is the testable inner function; self is the
// path of the running executable (typically the client binary).
func findDaemonBinaryFrom(self string) (string, error) {
	if self != "" {
		sibling := filepath.Join(filepath.Dir(self), daemonBinaryName)
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}
	path, err := exec.LookPath(daemonBinaryName)
	if err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s binary not found: not next to the client, and not on PATH; use --daemon-binary to specify a path", daemonBinaryName)
}

// buildPathEnv builds the PATH string for the service environment. It
// prepends the directories containing the child backends the daemon spawns by
// name — `pi` and `claude` — and appends a set of standard directories.
//
// claude is here for the same reason pi is, and is easier to miss: it is
// commonly installed under ~/.local/bin (as a symlink into a versioned
// directory), which is on nobody's default service PATH. Worse, in an
// interactive shell `claude` is often a shell function, so it appears to be
// "on PATH" when checked by hand while launchd has no equivalent and cannot
// find it at all — --kind claude children then fail to spawn under the service
// while working perfectly when rafikid is run from a terminal. exec.LookPath
// resolves the real executable, which is exactly what the daemon needs.
func buildPathEnv() string {
	standard := []string{
		"/usr/local/bin",
		"/opt/homebrew/bin",
		"/usr/bin",
		"/bin",
	}

	var dirs []string
	if claudePath, err := exec.LookPath("claude"); err == nil {
		dirs = append(dirs, filepath.Dir(claudePath))
	}
	if piPath, err := exec.LookPath("pi"); err == nil {
		dirs = append(dirs, filepath.Dir(piPath))
	} else {
		fmt.Fprintln(os.Stderr, "warning: `pi` not found on PATH; the daemon may not be able to spawn pi until PATH is configured")
	}
	dirs = append(dirs, standard...)

	// Deduplicate while preserving order.
	seen := make(map[string]bool, len(dirs))
	result := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d != "" && !seen[d] {
			seen[d] = true
			result = append(result, d)
		}
	}
	return strings.Join(result, ":")
}

// runOSCmd runs an OS command and returns combined output and any error.
// Used by the platform-specific backends. A var, not a func, so tests can
// stub it out to assert on the args a backend constructs (e.g. that the
// systemd/launchd unit name a call embeds matches the constant) without
// actually invoking systemctl/launchctl.
var runOSCmd = func(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
