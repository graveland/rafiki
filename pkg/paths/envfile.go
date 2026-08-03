package paths

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
// daemon's own environment first (see cmd/rafikid/agent_runtime.go: daemon env <
// forwarded env < explicit key), so a child spawned with no caller has nothing
// else to fall back on. And systemd's Environment= is line-based, which cannot
// represent ANTHROPIC_CUSTOM_HEADERS at all: that variable accepts a literal
// newline and nothing else as its separator, so any multi-header value breaks
// the unit file.
//
// systemd has EnvironmentFile= for exactly this; launchd has no equivalent, and
// the usual workaround — running the daemon under `sh -c '. file; exec rafikid'`
// — makes ProgramArguments unreadable and the service harder to introspect.
// Reading the file in the daemon is one implementation that behaves identically
// on both platforms.
func ServiceEnvFile() string {
	if v := os.Getenv(EnvFile); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "service.env")
}

// envAssignment is one KEY=VALUE parsed out of an environment file, in file
// order.
type envAssignment struct{ Key, Value string }

// parseEnvFile parses the environment-file format from r. It is the pure half
// of LoadEnvFile: it reports what the file says and changes nothing. name
// appears in warnings only, so a caller with a path can produce the same
// "path:line: ..." messages LoadEnvFile always has.
func parseEnvFile(r io.Reader, name string) (vars []envAssignment, warnings []string, err error) {
	sc := bufio.NewScanner(r)
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
				vars = append(vars, envAssignment{key, unescape(pending.String())})
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
			warnings = append(warnings, fmt.Sprintf("%s:%d: not a KEY=VALUE assignment", name, lineNo))
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			warnings = append(warnings, fmt.Sprintf("%s:%d: empty variable name", name, lineNo))
			continue
		}

		switch {
		case strings.HasPrefix(v, `"`):
			if rest, closed := closeQuoted(v[1:]); closed {
				vars = append(vars, envAssignment{k, unescape(rest)})
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
				vars = append(vars, envAssignment{k, v[1:end]})
			} else {
				warnings = append(warnings, fmt.Sprintf("%s:%d: unterminated single quote", name, lineNo))
			}
		default:
			vars = append(vars, envAssignment{k, strings.TrimSpace(v)})
		}
	}
	if inMulti {
		warnings = append(warnings, fmt.Sprintf("%s: unterminated quoted value for %s", name, key))
	}
	return vars, warnings, sc.Err()
}

// LoadEnvFile parses path and applies each assignment to the process
// environment, and returns the names it set.
//
// A name already present in the environment is left alone and not returned:
// the real environment wins, so `RAFIKI_DB=... rafikid` still overrides the
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

	vars, parseWarnings, parseErr := parseEnvFile(f, path)
	warnings = append(warnings, parseWarnings...)
	for _, v := range vars {
		applyEnv(v.Key, v.Value, &applied)
	}
	return applied, warnings, parseErr
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

// envEscaper is the inverse of envUnescaper. Backslash comes first so its own
// doubling is not re-scanned: strings.Replacer makes one pass and never
// rewrites what it just wrote.
var envEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)

// escapeEnvValue renders v as a double-quoted assignment value that
// parseEnvFile reads back unchanged. Everything is quoted, unconditionally:
// deciding per value which characters need it is how a writer and a parser
// drift apart, and an unnecessary pair of quotes costs nothing.
func escapeEnvValue(v string) string { return `"` + envEscaper.Replace(v) + `"` }

// MergeResult reports what a MergeEnvFile call did. Added, Existing and
// Conflict are disjoint and cover every key passed in.
type MergeResult struct {
	Added     []string // appended to the file, sorted
	Existing  []string // already in the file with the same value, sorted
	Conflict  []string // already in the file with a DIFFERENT value, sorted
	Defined   []string // every key the file defined BEFORE the merge, in file order
	Warnings  []string
	Tightened os.FileMode // the file's permission bits before MergeEnvFile chmod'ed it to 0600, or 0 if untouched
}

// MergeEnvFile appends to the environment file at path every assignment in
// vars whose key the file does not already define, creating it 0600 if absent.
// comment labels the appended block.
//
// Append-only, deliberately. This file is hand-maintained — it is where an
// operator puts an API key, a DSN, and the one multi-line
// ANTHROPIC_CUSTOM_HEADERS value no unit file can express — and a writer that
// rewrote it would have to reproduce every one of those faithfully to avoid
// destroying work it did not create. Appending needs to be correct only about
// what it adds. A key already present is therefore never touched: if its value
// differs from the one offered, that is reported as a Conflict for the caller
// to surface, because silently keeping either one loses information the
// operator has.
//
// If the file already exists with looser-than-0600 permissions, what happens
// depends on whether there is anything to append. When there is nothing new
// (every key is already Existing or Conflict), this call only OBSERVES the
// file, and observing does not justify touching its permissions — that is
// reported as a warning and left alone, same as LoadEnvFile's read-side
// warning. But when there IS something to append, this call is about to ADD a
// credential to that file, and appending a secret into a file left readable by
// everyone would defeat the reason this file exists at all — so the loose
// permissions are tightened to 0600 first, and that is reported via
// MergeResult.Tightened rather than done silently.
func MergeEnvFile(path string, vars map[string]string, comment string) (MergeResult, error) {
	var res MergeResult

	defined := make(map[string]string)
	var loosePerm os.FileMode
	existing, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, by design
	switch {
	case err == nil:
		if fi, statErr := os.Stat(path); statErr == nil {
			if perm := fi.Mode().Perm(); perm&0o077 != 0 {
				loosePerm = perm
			}
		}
		parsed, warnings, parseErr := parseEnvFile(bytes.NewReader(existing), path)
		res.Warnings = append(res.Warnings, warnings...)
		if parseErr != nil {
			return res, parseErr
		}
		for _, v := range parsed {
			if _, seen := defined[v.Key]; !seen {
				res.Defined = append(res.Defined, v.Key)
			}
			defined[v.Key] = v.Value
		}
	case os.IsNotExist(err):
		existing = nil
	default:
		return res, err
	}

	for k, v := range vars {
		switch old, present := defined[k]; {
		case !present:
			res.Added = append(res.Added, k)
		case old == v:
			res.Existing = append(res.Existing, k)
		default:
			res.Conflict = append(res.Conflict, k)
		}
	}
	sort.Strings(res.Added)
	sort.Strings(res.Existing)
	sort.Strings(res.Conflict)

	if len(res.Added) == 0 {
		// Nothing to write: this call only observed the file, so leave its
		// permissions alone too — just warn, the same way LoadEnvFile does on
		// its read path.
		if loosePerm != 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s is mode %04o; it holds credentials and should be 0600 (chmod 600 %s)", path, loosePerm, path))
		}
		return res, nil // nothing to write; leave the file byte-identical
	}

	if loosePerm != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return res, fmt.Errorf("tighten permissions on %s from %04o to 0600: %w", path, loosePerm, err)
		}
		res.Tightened = loosePerm
	}

	var b strings.Builder
	if len(existing) > 0 {
		// Unconditional blank line rather than a trailing-newline check: it
		// separates blocks readably and makes appending to a file whose last
		// line has no newline correct without inspecting it.
		b.WriteString("\n")
	}
	b.WriteString("# " + comment + "\n")
	for _, k := range res.Added {
		b.WriteString(k + "=" + escapeEnvValue(vars[k]) + "\n")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return res, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // operator-supplied path, by design
	if err != nil {
		return res, err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return res, err
	}
	return res, f.Close()
}
