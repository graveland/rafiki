//go:build darwin

package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"go.graveland.dev/rafiki/pkg/paths"
)

// Distinct from pi-controller's own "dev.graveland.pi-controller": sharing the
// label would make the two services clobber each other's plist.
const launchdLabel = "dev.graveland.rafiki"

// plistTemplate is the launchd property list for the rafiki daemon.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.DaemonBinary}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<!-- launchd default ExitTimeOut is 20s.  the daemon's own graceful
	     shutdown needs longer because pi children may do final LLM calls
	     (compaction, summarisation) before exiting.  Match the daemon's
	     internal 180s global shutdown bound. -->
	<key>ExitTimeOut</key>
	<integer>200</integer>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>{{xml .HomeEnv}}</string>
		<key>PATH</key>
		<string>{{xml .PathEnv}}</string>
{{- range .Extra}}
		<key>{{xml .Key}}</key>
		<string>{{xml .Value}}</string>
{{- end}}
	</dict>
</dict>
</plist>
`

type plistData struct {
	serviceSpec
	Label string
	// Extra is ExtraEnv in deterministic order; see sortedEnv.
	Extra []envKV
}

// xmlEscape escapes a value for a plist <string>. text/template does no
// context-aware escaping, and these values are not tame: a postgres DSN
// routinely carries a query string, so "?sslmode=disable&application_name=x"
// would emit a bare ampersand and produce a plist launchd refuses to parse —
// leaving the service uninstallable over the very variable this mechanism
// exists to carry.
func xmlEscape(s string) (string, error) {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderServiceConfig renders the launchd plist content for the given spec.
func renderServiceConfig(spec serviceSpec) (string, error) {
	tmpl, err := template.New("plist").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(plistTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	data := plistData{serviceSpec: spec, Label: launchdLabel, Extra: sortedEnv(spec.ExtraEnv)}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type darwinBackend struct{}

func newServiceBackend() serviceBackend { return &darwinBackend{} }

func (b *darwinBackend) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// serviceTarget returns the launchctl service target string (gui/UID/label).
func (b *darwinBackend) serviceTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)
}

// domainTarget returns the launchctl domain target string (gui/UID).
func (b *darwinBackend) domainTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func (b *darwinBackend) LogPath() string {
	return paths.ServiceLogPath()
}

func (b *darwinBackend) Install(spec serviceSpec) error {
	plistPath, err := b.plistPath()
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
		return fmt.Errorf("render plist: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write plist %s: %w", plistPath, err)
	}

	// Modern macOS: bootstrap. Fall back to legacy load on older versions.
	_, err = runOSCmd("launchctl", "bootstrap", b.domainTarget(), plistPath)
	if err != nil {
		out2, err2 := runOSCmd("launchctl", "load", plistPath)
		if err2 != nil {
			return fmt.Errorf("launchctl bootstrap and legacy load both failed: %s", strings.TrimSpace(out2))
		}
	}
	return nil
}

func (b *darwinBackend) Uninstall() error {
	plistPath, err := b.plistPath()
	if err != nil {
		return err
	}

	// Modern bootout; fall back to legacy unload. Errors here are best-effort.
	_, err = runOSCmd("launchctl", "bootout", b.serviceTarget())
	if err != nil {
		_, _ = runOSCmd("launchctl", "unload", plistPath)
	}

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist %s: %w", plistPath, err)
	}
	return nil
}

func (b *darwinBackend) Start() error {
	out, err := runOSCmd("launchctl", "kickstart", b.serviceTarget())
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *darwinBackend) Stop() error {
	out, err := runOSCmd("launchctl", "kill", "SIGTERM", b.serviceTarget())
	if err != nil {
		return fmt.Errorf("launchctl kill: %s", strings.TrimSpace(out))
	}
	return nil
}

func (b *darwinBackend) Restart() error {
	// kickstart -k kills any running instance and starts fresh.
	out, err := runOSCmd("launchctl", "kickstart", "-k", b.serviceTarget())
	if err != nil {
		return fmt.Errorf("launchctl kickstart -k: %s", strings.TrimSpace(out))
	}
	return nil
}

var launchdPIDRe = regexp.MustCompile(`\bpid\s*=\s*(\d+)`)

func (b *darwinBackend) Status() (serviceStatus, error) {
	out, err := runOSCmd("launchctl", "print", b.serviceTarget())
	if err != nil {
		outLower := strings.ToLower(out)
		if strings.Contains(outLower, "could not find service") ||
			strings.Contains(outLower, "no such process") ||
			strings.Contains(outLower, "domain does not contain") {
			return serviceStatus{Installed: false}, nil
		}
		// Ambiguous error. Check if the plist file exists to distinguish
		// "installed but not running" from "not installed".
		plistPath, _ := b.plistPath()
		if _, serr := os.Stat(plistPath); serr == nil {
			return serviceStatus{Installed: true, Running: false, Detail: strings.TrimSpace(out)}, nil
		}
		return serviceStatus{}, fmt.Errorf("launchctl print: %w: %s", err, strings.TrimSpace(out))
	}

	st := serviceStatus{Installed: true}
	if m := launchdPIDRe.FindStringSubmatch(out); m != nil {
		pid, _ := strconv.Atoi(m[1])
		if pid > 0 {
			st.Running = true
			st.PID = pid
		}
	}
	// Trim the detail to a single line summary for the status display.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state") || strings.HasPrefix(line, "pid") {
			st.Detail += line + " "
		}
	}
	st.Detail = strings.TrimSpace(st.Detail)
	return st, nil
}
