package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	PathEnv      string
	HomeEnv      string
	ExtraEnv     map[string]string
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
		Short:   "Manage the pi-controller daemon as a system service",
		Long: `Install, start, stop, and inspect the pi-controller daemon as a per-user system service.

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
	)
	return cmd
}

func newServiceInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the pi-controller service",
		Args:  cobra.NoArgs,
		RunE:  runServiceInstall,
	}
	cmd.Flags().String("daemon-binary", "", "Path to pi-controller binary (default: auto-detect next to pic, then PATH)")
	cmd.Flags().String("path-env", "", "PATH value for the service environment (default: auto-detect)")
	return cmd
}

func newServiceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the pi-controller service",
		Args:  cobra.NoArgs,
		RunE:  runServiceUninstall,
	}
}

func newServiceStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the pi-controller service",
		Args:  cobra.NoArgs,
		RunE:  runServiceStart,
	}
}

func newServiceStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the pi-controller service",
		Args:  cobra.NoArgs,
		RunE:  runServiceStop,
	}
}

func newServiceRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the pi-controller service",
		Args:  cobra.NoArgs,
		RunE:  runServiceRestart,
	}
}

func newServiceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show installation and running state of the pi-controller service",
		Args:  cobra.NoArgs,
		RunE:  runServiceStatus,
	}
}

func newServiceLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the pi-controller daemon log",
		Long:  "Show the pi-controller daemon log. Follows by default; use --follow=false to print and exit.",
		Args:  cobra.NoArgs,
		RunE:  runServiceLogs,
	}
	cmd.Flags().BoolP("follow", "f", true, "Follow log output via polling (set --follow=false to print and exit)")
	return cmd
}

// --- run functions ---

func runServiceInstall(cmd *cobra.Command, _ []string) error {
	spec, err := buildServiceSpec(cmd)
	if err != nil {
		return err
	}
	b := newServiceBackend()
	if err := b.Install(spec); err != nil {
		return fmt.Errorf("install service: %w", err)
	}
	fmt.Printf("pi-controller service installed.\nLog: %s\n", b.LogPath())
	return nil
}

func runServiceUninstall(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Uninstall(); err != nil {
		return fmt.Errorf("uninstall service: %w", err)
	}
	fmt.Println("pi-controller service uninstalled.")
	return nil
}

func runServiceStart(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Println("pi-controller service started.")
	return nil
}

func runServiceStop(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Stop(); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	fmt.Println("pi-controller service stopped.")
	return nil
}

func runServiceRestart(_ *cobra.Command, _ []string) error {
	b := newServiceBackend()
	if err := b.Restart(); err != nil {
		return fmt.Errorf("restart service: %w", err)
	}
	fmt.Println("pi-controller service restarted.")
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

	fmt.Println("pi-controller service:")
	fmt.Printf("  Installed: %s\n", installed)
	fmt.Printf("  Running:   %s\n", running)
	fmt.Printf("  Log:       %s\n", b.LogPath())
	if st.Detail != "" {
		fmt.Printf("  Detail:    %s\n", st.Detail)
	}
	return nil
}

func runServiceLogs(cmd *cobra.Command, _ []string) error {
	b := newServiceBackend()
	logPath := b.LogPath()

	follow, _ := cmd.Flags().GetBool("follow")

	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("log file not found: %s (has the daemon ever run?)", logPath)
		}
		return err
	}
	defer f.Close()

	if !follow {
		_, err = io.Copy(os.Stdout, f)
		return err
	}

	// Follow mode: stream existing content then poll for new writes.
	ctx := cmdCtx(cmd)
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

	if v, _ := cmd.Flags().GetString("path-env"); v != "" {
		spec.PathEnv = v
	} else {
		spec.PathEnv = buildPathEnv()
	}

	return spec, nil
}

// findDaemonBinary locates the pi-controller binary. It first looks for a
// sibling next to the running pic executable, then falls back to PATH.
func findDaemonBinary() (string, error) {
	self, _ := os.Executable()
	return findDaemonBinaryFrom(self)
}

// findDaemonBinaryFrom is the testable inner function; self is the
// path of the running executable (typically the pic binary).
func findDaemonBinaryFrom(self string) (string, error) {
	if self != "" {
		sibling := filepath.Join(filepath.Dir(self), "pi-controller")
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}
	path, err := exec.LookPath("pi-controller")
	if err == nil {
		return path, nil
	}
	return "", fmt.Errorf("pi-controller binary not found: not next to pic, and not on PATH; use --daemon-binary to specify a path")
}

// buildPathEnv builds the PATH string for the service environment. It
// prepends the directory containing the `pi` binary (so the daemon can
// spawn it) and appends a set of standard directories.
func buildPathEnv() string {
	standard := []string{
		"/usr/local/bin",
		"/opt/homebrew/bin",
		"/usr/bin",
		"/bin",
	}

	var dirs []string
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
// Used by the platform-specific backends.
func runOSCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
