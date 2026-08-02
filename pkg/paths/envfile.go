package paths

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvFile names an override for the daemon's environment file.
const EnvFile = "RAFIKI_ENV_FILE"

// ServiceEnvFile is the daemon's environment file: $RAFIKI_ENV_FILE if set,
// else <config dir>/service.env.
//
// This exists because neither init system can carry what the daemon needs. A
// launchd plist and a systemd unit are both world-readable, so credentials must
// not be written into them — but agent children resolve their API key from the
// daemon's own environment first (see cmd/fundid/agent_runtime.go: daemon env <
// forwarded env < explicit key), so a child spawned with no caller has nothing
// else to fall back on. And systemd's Environment= is line-based, which cannot
// represent ANTHROPIC_CUSTOM_HEADERS at all: that variable accepts a literal
// newline and nothing else as its separator, so any multi-header value breaks
// the unit file.
//
// systemd has EnvironmentFile= for exactly this; launchd has no equivalent, and
// the usual workaround — running the daemon under `sh -c '. file; exec fundid'`
// — makes ProgramArguments unreadable and the service harder to introspect.
// Reading the file in the daemon is one implementation that behaves identically
// on both platforms.
func ServiceEnvFile() string {
	if v := os.Getenv(EnvFile); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "service.env")
}

// LoadEnvFile parses path and applies each assignment to the process
// environment, and returns the names it set.
//
// A name already present in the environment is left alone and not returned:
// the real environment wins, so `RAFIKI_DB=... fundid` still overrides the
// file, and a service manager's own settings are not silently replaced by a
// file the operator forgot about.
//
// A missing file is not an error — this is optional configuration, and a daemon
// that refuses to start because an optional file is absent is worse than one
// that starts without it. Malformed lines are returned as warnings rather than
// failing the load, for the same reason: one bad line should not cost the
// operator every good one. (Contrast pkg/models, where a malformed models.json
// silently yields nothing at all; that is the behaviour this avoids.)
func LoadEnvFile(path string) (applied []string, warnings []string, err error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path, by design
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer f.Close() //nolint:errcheck // read-only close is not actionable

	if fi, statErr := f.Stat(); statErr == nil {
		// Warn rather than refuse. This file holds credentials and ought to be
		// 0600, but declining to boot over a permission bit is a worse outcome
		// than the exposure it prevents — and an operator who sees the warning
		// can fix it in one command.
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%s is mode %04o; it holds credentials and should be 0600 (chmod 600 %s)", path, perm, path))
		}
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		lineNo  int
		pending strings.Builder // accumulates a double-quoted value spanning lines
		key     string
		inMulti bool
	)
	for sc.Scan() {
		lineNo++
		line := sc.Text()

		if inMulti {
			// Inside a double-quoted value: a line ending the quote closes it,
			// everything else is literal content including the newline.
			if rest, closed := closeQuoted(line); closed {
				pending.WriteString(rest)
				applyEnv(key, unescape(pending.String()), &applied)
				inMulti = false
				pending.Reset()
			} else {
				pending.WriteString(line)
				pending.WriteByte('\n')
			}
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")

		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s:%d: not a KEY=VALUE assignment", path, lineNo))
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			warnings = append(warnings, fmt.Sprintf("%s:%d: empty variable name", path, lineNo))
			continue
		}

		switch {
		case strings.HasPrefix(v, `"`):
			if rest, closed := closeQuoted(v[1:]); closed {
				applyEnv(k, unescape(rest), &applied)
			} else {
				// Opens a multi-line value: this is how a literal newline gets
				// into ANTHROPIC_CUSTOM_HEADERS, which is the one variable that
				// demands one and cannot be expressed in a unit file at all.
				key, inMulti = k, true
				pending.WriteString(v[1:])
				pending.WriteByte('\n')
			}
		case strings.HasPrefix(v, `'`):
			// Single quotes are literal, shell-style: no escape processing.
			if end := strings.LastIndex(v, `'`); end > 0 {
				applyEnv(k, v[1:end], &applied)
			} else {
				warnings = append(warnings, fmt.Sprintf("%s:%d: unterminated single quote", path, lineNo))
			}
		default:
			applyEnv(k, strings.TrimSpace(v), &applied)
		}
	}
	if inMulti {
		warnings = append(warnings, fmt.Sprintf("%s: unterminated quoted value for %s", path, key))
	}
	if scanErr := sc.Err(); scanErr != nil {
		return applied, warnings, scanErr
	}
	return applied, warnings, nil
}

// closeQuoted reports whether s contains the closing double quote of a value,
// returning the content before it. A quote preceded by a backslash is escaped
// and does not close.
func closeQuoted(s string) (content string, closed bool) {
	for i := range len(s) {
		if s[i] != '"' {
			continue
		}
		back := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			back++
		}
		if back%2 == 0 {
			return s[:i], true
		}
	}
	return s, false
}

var envUnescaper = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`)

func unescape(s string) string { return envUnescaper.Replace(s) }

// applyEnv sets k unless it is already present: the real environment wins.
func applyEnv(k, v string, applied *[]string) {
	if _, ok := os.LookupEnv(k); ok {
		return
	}
	if err := os.Setenv(k, v); err == nil {
		*applied = append(*applied, k)
	}
}
