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
	"time"

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

	// Best-effort bootout before bootstrap. `launchctl bootstrap` fails with
	// "service already loaded" against a job that is already bootstrapped —
	// which is exactly the case on a reinstall — and in that failure it does
	// NOT replace the running job's definition. The subsequent legacy `load`
	// fallback below is also a no-op against an already-loaded job. Without
	// this bootout, `service install` can write a fresh plist to disk while
	// the live launchd job (and every env var it holds — this mechanism
	// exists because RAFIKI_DB used to be one of them) keeps running under
	// the stale one, and `service restart` only kickstarts *within* that
	// stale definition rather than picking up the new one.
	//
	// bootout is ASYNCHRONOUS: it returns before the job is actually gone
	// (the daemon takes ~250ms to shut down its DB pool). A bootstrap issued
	// immediately after can therefore race the still-departing old job and
	// fail — and the legacy `load` fallback below returns exit 0 without
	// having loaded anything, which used to make Install return nil while
	// the service sat completely unloaded. So: if bootout succeeded (or
	// failed ambiguously — anything other than a clean "wasn't loaded"), the
	// job may still be draining and we poll `launchctl print` until it is
	// gone, capped at ~2s. Skip the poll only on the fast path where bootout
	// itself reports the job was never loaded (a clean machine). Whether or
	// not the cap expires, we always fall through to bootstrap below — never
	// give up and leave the machine unloaded. The real gate is the
	// post-bootstrap verification, not this poll.
	bootoutOut, bootoutErr := runOSCmd("launchctl", "bootout", b.serviceTarget())
	if bootoutErr == nil || !isServiceNotFoundOutput(bootoutOut) {
		b.waitForUnload()
	}

	// Modern macOS: bootstrap. Fall back to legacy load on older versions.
	bootstrapOut, err := runOSCmd("launchctl", "bootstrap", b.domainTarget(), plistPath)
	if err != nil {
		bootstrapOut, err = runOSCmd("launchctl", "load", plistPath)
		if err != nil {
			return fmt.Errorf("launchctl bootstrap and legacy load both failed: %s", strings.TrimSpace(bootstrapOut))
		}
	}

	// VERIFY the service is actually loaded. Neither bootstrap's nor the
	// legacy load's exit code is proof: the legacy fallback in particular
	// returns exit 0 without loading anything against a job that failed to
	// bootout in time. Trust launchctl print instead — the exact same
	// not-found detection Status() uses.
	verifyOut, verifyErr := runOSCmd("launchctl", "print", b.serviceTarget())
	if verifyErr != nil && isServiceNotFoundOutput(verifyOut) {
		return fmt.Errorf("service install did not take effect: launchctl print reports the job is not loaded after bootstrap/load (bootstrap output: %s); the rafiki daemon is NOT running — check `launchctl print %s` and retry `rafiki service install`",
			strings.TrimSpace(bootstrapOut), b.serviceTarget())
	}
	return nil
}

// sleepFn is time.Sleep, stubbed in tests so the poll below runs instantly.
var sleepFn = time.Sleep

const (
	installPollInterval = 50 * time.Millisecond
	installPollCap      = 2 * time.Second
)

// waitForUnload polls `launchctl print` for the service target until it
// reports not-found (the job is gone), or until installPollCap worth of
// attempts is exhausted — whichever comes first. It never returns an error:
// giving up here is fine, because Install always proceeds to bootstrap
// afterward regardless, and verifies the real outcome itself.
func (b *darwinBackend) waitForUnload() {
	attempts := int(installPollCap / installPollInterval)
	for i := 0; i < attempts; i++ {
		out, err := runOSCmd("launchctl", "print", b.serviceTarget())
		if err != nil && isServiceNotFoundOutput(out) {
			return
		}
		sleepFn(installPollInterval)
	}
}

// isServiceNotFoundOutput reports whether launchctl output indicates "no
// such service is loaded", across the various phrasings launchctl uses
// depending on macOS version and which subcommand produced it. Shared by
// Status (interpreting a `print` failure) and Install (polling for a
// bootout to complete) so both trust exactly the same detection.
func isServiceNotFoundOutput(out string) bool {
	outLower := strings.ToLower(out)
	return strings.Contains(outLower, "could not find service") ||
		strings.Contains(outLower, "no such process") ||
		strings.Contains(outLower, "domain does not contain")
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
		if isServiceNotFoundOutput(out) {
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
