//go:build linux

package main

import (
	"bytes"
	"fmt"
	"git.graveland.dev/brent/fundi/internal/paths"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

// Distinct from pi-controller's unit name — see launchdLabel in service_darwin.go.
const systemdUnitName = "fundi"

// unitTemplate is the systemd user service unit for the fundi daemon.
const unitTemplate = `[Unit]
Description=fundi daemon
After=default.target

[Service]
ExecStart={{.DaemonBinary}}
Restart=on-failure
RestartSec=2
# systemd default TimeoutStopSec is 90s.  the daemon's own graceful
# shutdown needs longer because pi children may do final LLM calls
# (compaction, summarisation) before exiting.  Match the daemon's internal
# 180s global shutdown bound.
TimeoutStopSec=200
StandardOutput=append:{{.LogPath}}
StandardError=append:{{.LogPath}}
Environment=HOME={{.HomeEnv}}
Environment=PATH={{.PathEnv}}

[Install]
WantedBy=default.target
`

// renderServiceConfig renders the systemd unit file content for the given spec.
func renderServiceConfig(spec serviceSpec) (string, error) {
	tmpl, err := template.New("unit").Parse(unitTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, spec); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type linuxBackend struct{}

func newServiceBackend() serviceBackend { return &linuxBackend{} }

func (b *linuxBackend) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName+".service"), nil
}

func (b *linuxBackend) LogPath() string {
	return paths.ServiceLogPath()
}

func (b *linuxBackend) Install(spec serviceSpec) error {
	unitPath, err := b.unitPath()
	if err != nil {
		return err
	}

	// Ensure the log directory exists before the daemon tries to write.
	logDir := filepath.Dir(spec.LogPath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log directory %s: %w", logDir, err)
	}

	content, err := renderServiceConfig(spec)
	if err != nil {
		return fmt.Errorf("render unit file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
		return fmt.Errorf("create systemd user unit directory: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write unit file %s: %w", unitPath, err)
	}

	if out, err := runOSCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(out))
	}
	if out, err := runOSCmd("systemctl", "--user", "enable", "--now", systemdUnitName); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %s", systemdUnitName, strings.TrimSpace(out))
	}
	return nil
}

func (b *linuxBackend) Uninstall() error {
	unitPath, err := b.unitPath()
	if err != nil {
		return err
	}

	// Best-effort disable+stop; ignore errors (service might not be loaded).
	_, _ = runOSCmd("systemctl", "--user", "disable", "--now", systemdUnitName)

	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file %s: %w", unitPath, err)
	}

	if out, err := runOSCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *linuxBackend) Start() error {
	out, err := runOSCmd("systemctl", "--user", "start", systemdUnitName)
	if err != nil {
		return fmt.Errorf("systemctl start: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *linuxBackend) Stop() error {
	out, err := runOSCmd("systemctl", "--user", "stop", systemdUnitName)
	if err != nil {
		return fmt.Errorf("systemctl stop: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *linuxBackend) Restart() error {
	out, err := runOSCmd("systemctl", "--user", "restart", systemdUnitName)
	if err != nil {
		return fmt.Errorf("systemctl restart: %s", strings.TrimSpace(out))
	}
	return nil
}

var systemdPIDRe = regexp.MustCompile(`MainPID=(\d+)`)

func (b *linuxBackend) Status() (serviceStatus, error) {
	unitPath, _ := b.unitPath()
	_, statErr := os.Stat(unitPath)
	installed := statErr == nil

	if !installed {
		return serviceStatus{Installed: false}, nil
	}

	activeOut, _ := runOSCmd("systemctl", "--user", "is-active", systemdUnitName)
	running := strings.TrimSpace(activeOut) == "active"

	st := serviceStatus{Installed: true, Running: running}

	if running {
		pidOut, _ := runOSCmd("systemctl", "--user", "show", systemdUnitName, "--property=MainPID")
		if m := systemdPIDRe.FindStringSubmatch(pidOut); m != nil {
			pid, _ := strconv.Atoi(m[1])
			if pid > 0 {
				st.PID = pid
			}
		}
	}

	return st, nil
}
