//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"go.graveland.dev/rafiki/pkg/paths"
)

// systemdUnitName is the DAEMON's unit name. Backend methods never use it
// directly — they use linuxBackend.unit — so the executor's backend cannot
// enable, stop or restart the daemon by mistake.
const systemdUnitName = "rafiki"

// systemdExecutorUnitName names the EXECUTOR's unit. Separate from the daemon's
// because the two are independent services with independent lifecycles — an
// executor commonly runs on a machine hosting no daemon at all.
const systemdExecutorUnitName = "rafiki-executor"

// unitTemplate is the systemd user service unit for the rafiki daemon.
const unitTemplate = `[Unit]
Description={{.Description}}
After=default.target

[Service]
ExecStart={{.DaemonBinary}}{{range .Args}} {{unitq .}}{{end}}
Restart=on-failure
RestartSec=2
# systemd default TimeoutStopSec is 90s.  the daemon stops agents in a few
# seconds; this covers its worst case (20s drain, then pool close).
TimeoutStopSec=45
StandardOutput=append:{{.LogPath}}
StandardError=append:{{.LogPath}}
Environment={{unitq (printf "HOME=%s" .HomeEnv)}}
Environment={{unitq (printf "PATH=%s" .PathEnv)}}
{{- range .Extra}}
Environment={{unitq (printf "%s=%s" .Key .Value)}}
{{- end}}

[Install]
WantedBy=default.target
`

// unitQuote renders one Environment= assignment, quoting it when the value
// needs it. systemd splits an unquoted assignment on whitespace, so a value
// containing a space — RAFIKI_DEFAULT_LABELS, or any path under a directory
// with a space in its name — would silently truncate at the first space, with
// the remainder parsed as a second malformed assignment. The backslash and
// quote escapes below are systemd's own unquoting rules.
func unitQuote(assignment string) string {
	if !strings.ContainsAny(assignment, " \t\"'\\") {
		return assignment
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(assignment) + `"`
}

// renderServiceConfig renders the systemd unit file content for the given spec,
// with description as the unit's Description=.
func renderServiceConfig(spec serviceSpec, description string) (string, error) {
	tmpl, err := template.New("unit").Funcs(template.FuncMap{"unitq": unitQuote}).Parse(unitTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	data := struct {
		serviceSpec
		Extra       []envKV
		Description string
	}{serviceSpec: spec, Extra: sortedEnv(spec.ExtraEnv), Description: description}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type linuxBackend struct {
	// unit is the systemd unit name, held on the backend so the daemon's and
	// the executor's units are managed by the same code with distinct
	// identities. Every systemctl call goes through it: a method naming
	// systemdUnitName directly would act on the DAEMON from the executor's
	// backend.
	unit string
	// description is the unit's Description=.
	description string
	// logPath is where this unit's stdout/stderr go; see the darwin backend.
	logPath string
}

func newServiceBackend() serviceBackend {
	return &linuxBackend{unit: systemdUnitName + ".service", description: "rafiki daemon", logPath: paths.ServiceLogPath()}
}

// newExecutorServiceBackend manages the executor's unit rather than the daemon's.
func newExecutorServiceBackend() serviceBackend {
	return &linuxBackend{unit: systemdExecutorUnitName + ".service", description: "rafiki executor", logPath: paths.ExecutorServiceLogPath()}
}

func (b *linuxBackend) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", b.unit), nil
}

func (b *linuxBackend) LogPath() string { return b.logPath }

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

	content, err := renderServiceConfig(spec, b.description)
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
	if out, err := runOSCmd("systemctl", "--user", "enable", "--now", b.unit); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %s", b.unit, strings.TrimSpace(out))
	}
	return nil
}

// Repoint swaps the ExecStart program in the installed unit and restarts it.
func (b *linuxBackend) Repoint(binary string) error {
	unitPath, err := b.unitPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(unitPath)
	if err != nil {
		return fmt.Errorf("read unit file %s: %w", unitPath, err)
	}
	updated, ok := replaceUnitExecStart(data, binary)
	if !ok {
		return fmt.Errorf("unit file %s has no ExecStart to repoint", unitPath)
	}
	if err := os.WriteFile(unitPath, updated, 0644); err != nil {
		return fmt.Errorf("write unit file %s: %w", unitPath, err)
	}
	if out, err := runOSCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(out))
	}
	return b.Restart()
}

func (b *linuxBackend) Uninstall() error {
	unitPath, err := b.unitPath()
	if err != nil {
		return err
	}

	// Best-effort disable+stop; ignore errors (service might not be loaded).
	_, _ = runOSCmd("systemctl", "--user", "disable", "--now", b.unit)

	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file %s: %w", unitPath, err)
	}

	if out, err := runOSCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *linuxBackend) Start() error {
	out, err := runOSCmd("systemctl", "--user", "start", b.unit)
	if err != nil {
		return fmt.Errorf("systemctl start: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *linuxBackend) Stop() error {
	out, err := runOSCmd("systemctl", "--user", "stop", b.unit)
	if err != nil {
		return fmt.Errorf("systemctl stop: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *linuxBackend) Restart() error {
	out, err := runOSCmd("systemctl", "--user", "restart", b.unit)
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

	activeOut, _ := runOSCmd("systemctl", "--user", "is-active", b.unit)
	running := strings.TrimSpace(activeOut) == "active"

	st := serviceStatus{Installed: true, Running: running}
	if data, err := os.ReadFile(unitPath); err == nil {
		st.Binary = unitExecStart(data)
	}

	if running {
		pidOut, _ := runOSCmd("systemctl", "--user", "show", b.unit, "--property=MainPID")
		if m := systemdPIDRe.FindStringSubmatch(pidOut); m != nil {
			pid, _ := strconv.Atoi(m[1])
			if pid > 0 {
				st.PID = pid
			}
		}
	}

	return st, nil
}
