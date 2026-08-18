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

// launchdLabel is the service identity: it names the plist, and `launchctl`
// addresses the service by it. It is a constant rather than a literal in each
// call site so the plist path, the load/unload commands and the status query
// cannot drift apart.
const launchdLabel = "dev.graveland.rafiki"

// executorLaunchdLabel names the EXECUTOR's unit. A second label rather than a
// second field on the first: the two are independent services with independent
// lifecycles — an executor is expected to run on machines that host no daemon at
// all — and sharing an identity would make `stop` ambiguous.
const executorLaunchdLabel = "dev.graveland.rafiki-executor"

// plistTemplate is the launchd property list for the rafiki daemon.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{xml .DaemonBinary}}</string>
{{- range .Args}}
		<string>{{xml .}}</string>
{{- end}}
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
func renderServiceConfig(spec serviceSpec, label string) (string, error) {
	tmpl, err := template.New("plist").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(plistTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	data := plistData{serviceSpec: spec, Label: label, Extra: sortedEnv(spec.ExtraEnv)}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type darwinBackend struct {
	// label is the launchd identity: it names the plist and every launchctl
	// target. Held on the backend so the daemon's unit and the executor's are
	// managed by the same code with different identities.
	label string
	// logPath is where this unit's stdout/stderr go. Held per backend so the
	// daemon's log and the executor's do not interleave.
	logPath string
}

func newServiceBackend() serviceBackend {
	return &darwinBackend{label: launchdLabel, logPath: paths.ServiceLogPath()}
}

// newExecutorServiceBackend manages the executor's unit rather than the daemon's.
func newExecutorServiceBackend() serviceBackend {
	return &darwinBackend{label: executorLaunchdLabel, logPath: paths.ExecutorServiceLogPath()}
}

func (b *darwinBackend) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", b.label+".plist"), nil
}

// serviceTarget returns the launchctl service target string (gui/UID/label).
func (b *darwinBackend) serviceTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), b.label)
}

// domainTarget returns the launchctl domain target string (gui/UID).
func (b *darwinBackend) domainTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func (b *darwinBackend) LogPath() string { return b.logPath }

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

	content, err := renderServiceConfig(spec, b.label)
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
	unloadConfirmed := bootoutErr != nil && isServiceNotFoundOutput(bootoutOut) // fast path: never loaded
	if !unloadConfirmed {
		unloadConfirmed = b.waitForUnload()
	}

	// Modern macOS: bootstrap. Fall back to legacy load on older versions.
	bootstrapOut, err := runOSCmd("launchctl", "bootstrap", b.domainTarget(), plistPath)
	bootstrapFailed := err != nil
	if bootstrapFailed {
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

	// launchctl print reporting "loaded" here is NOT proof the new plist
	// took effect: if the old job was never confirmed unloaded (the poll
	// gave up before seeing it gone) and bootstrap itself failed — meaning
	// we're relying on the legacy load fallback, which lies with exit 0
	// whether or not it loaded anything — what's now "loaded" could equally
	// be the stale job bootout was trying to replace, still running under
	// its old environment. This is gated on the poll result, not on
	// bootstrap failure alone, because a bootstrap failure is also what an
	// older macOS lacking the subcommand looks like, and there the legacy
	// `load` genuinely does the work.
	//
	// That gate is not airtight on such a machine: `bootout`, `bootstrap`
	// and `print` all arrived together in the macOS 10.10 launchctl
	// rewrite, so a launchctl without `bootstrap` has no `print` either —
	// the poll can never confirm anything, and a reinstall there would take
	// this branch and report a false failure. Accepted: Status() already
	// hard-depends on `launchctl print` and is degraded on those machines
	// regardless, so the legacy `load` path is vestigial. If it ever stops
	// being vestigial, gate on a real launchctl-generation probe rather
	// than widening this condition.
	if !unloadConfirmed && bootstrapFailed {
		return fmt.Errorf("service install may not have taken effect: launchctl print now reports the service loaded, but the previous job was never confirmed unloaded within %s and bootstrap failed — this may be the STALE job still running under its old environment, not the plist just written; run `launchctl bootout %s` by hand, confirm with `launchctl print %s` that it is gone, then retry `rafiki service install` (legacy load output: %s)",
			installPollCap, b.serviceTarget(), b.serviceTarget(), strings.TrimSpace(bootstrapOut))
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
// attempts is exhausted — whichever comes first. It reports whether the
// unload was confirmed; giving up without confirming is not itself an
// error, since Install always proceeds to bootstrap afterward regardless —
// but the caller uses the confirmation to decide whether a bootstrap
// failure afterward can be trusted to mean anything benign.
func (b *darwinBackend) waitForUnload() (confirmedGone bool) {
	attempts := int(installPollCap / installPollInterval)
	for i := 0; i < attempts; i++ {
		out, err := runOSCmd("launchctl", "print", b.serviceTarget())
		if err != nil && isServiceNotFoundOutput(out) {
			return true
		}
		sleepFn(installPollInterval)
	}
	return false
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
	// Graceful restart: bootout (launchd sends the job SIGTERM and waits for it
	// to exit, bounded by ExitTimeOut — 200s in our plist), poll to confirm it
	// is gone, then bootstrap a fresh instance. This replaces the previous
	// kickstart -k, which sent SIGKILL with no chance to drain.
	plistPath, err := b.plistPath()
	if err != nil {
		return err
	}

	// Best-effort bootout first. It may fail if the job was never loaded; that
	// is fine — the bootstrap below will load it regardless.
	bootoutOut, bootoutErr := runOSCmd("launchctl", "bootout", b.serviceTarget())
	unloadConfirmed := bootoutErr != nil && isServiceNotFoundOutput(bootoutOut) // fast path: never loaded
	if !unloadConfirmed {
		unloadConfirmed = b.waitForUnload()
	}

	// Bootstrap the fresh instance; fall back to legacy load.
	bootstrapOut, err := runOSCmd("launchctl", "bootstrap", b.domainTarget(), plistPath)
	bootstrapFailed := err != nil
	if bootstrapFailed {
		bootstrapOut, err = runOSCmd("launchctl", "load", plistPath)
		if err != nil {
			return fmt.Errorf("launchctl bootstrap and legacy load both failed during restart: %s", strings.TrimSpace(bootstrapOut))
		}
	}

	// Verify the service actually loaded, same post-condition Install enforces.
	verifyOut, verifyErr := runOSCmd("launchctl", "print", b.serviceTarget())
	if verifyErr != nil && isServiceNotFoundOutput(verifyOut) {
		return fmt.Errorf("restart: service did not reload — launchctl print reports the job is not loaded after bootstrap/load (bootstrap output: %s)", strings.TrimSpace(bootstrapOut))
	}
	if !unloadConfirmed && bootstrapFailed {
		return fmt.Errorf("restart: previous job was never confirmed unloaded within %s and bootstrap failed — the running instance may be the stale job; run `launchctl bootout %s` by hand and retry",
			installPollCap, b.serviceTarget())
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
