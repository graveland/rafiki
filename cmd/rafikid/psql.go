// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// newPsqlCmd opens psql on whatever database rafikid is configured for,
// including the embedded one. It exists because the embedded server's binaries
// ship no client (the zonky artifacts carry initdb, pg_ctl and postgres only),
// so an operator inspecting that database needs the system psql pointed at the
// right socket — which is a data directory path, not a hostname.
func newPsqlCmd() *cobra.Command {
	var db string

	cmd := &cobra.Command{
		Use:   "psql [-- psql args...]",
		Short: "Open psql on rafikid's database (RAFIKI_DB, including the embedded one)",
		Long: `Opens psql against rafikid's database, resolving RAFIKI_DB=embedded first so
the private server is running before the client connects.

The connection is passed to psql in the PG* environment variables, never on its
command line: psql's argv is visible in the process table to every user on the
machine, and a DSN may hold a password. Inherited PG* variables are dropped, so
what psql connects to is exactly what --db (or RAFIKI_DB) named.

Everything after "--" is handed to psql unchanged.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if db == "" {
				return errors.New("--db (or RAFIKI_DB) is required")
			}
			dsn, err := resolveDSN(cmd.Context(), db)
			if err != nil {
				return err
			}
			inv, err := psqlInvocation(db, dsn, args, os.Environ())
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, inv.target)
			return syscall.Exec(inv.path, inv.argv, inv.env)
		},
	}

	cmd.Flags().StringVar(&db, "db", dbDSNDefault(), "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")
	return cmd
}

// psqlInv is one fully resolved psql exec: the program, its argv, the
// environment carrying the connection, and the line telling the operator where
// that connection goes.
type psqlInv struct {
	path   string   // absolute psql path from exec.LookPath
	argv   []string // "psql" followed by args, unchanged; NEVER any DSN part
	env    []string // environ minus every PG* variable, plus the connection's PG* variables
	target string   // one line naming where psql will connect
}

// psqlMissing is the whole remedy an operator needs, in the error itself: psql
// is a separate package from the server on every platform rafiki runs on.
const psqlMissing = "psql not found on PATH; install the PostgreSQL client " +
	"(macOS: brew install libpq && brew link --force libpq; " +
	"Debian/Ubuntu: apt install postgresql-client)"

// pgEnv maps one libpq connection keyword to the environment variable psql
// reads for it. A keyword absent from this table cannot be expressed in the
// environment, and is refused rather than silently dropped.
var pgEnv = map[string]string{
	"host":                 "PGHOST",
	"hostaddr":             "PGHOSTADDR",
	"port":                 "PGPORT",
	"dbname":               "PGDATABASE",
	"user":                 "PGUSER",
	"password":             "PGPASSWORD",
	"sslmode":              "PGSSLMODE",
	"sslrootcert":          "PGSSLROOTCERT",
	"sslcert":              "PGSSLCERT",
	"sslkey":               "PGSSLKEY",
	"sslnegotiation":       "PGSSLNEGOTIATION",
	"connect_timeout":      "PGCONNECT_TIMEOUT",
	"application_name":     "PGAPPNAME",
	"options":              "PGOPTIONS",
	"target_session_attrs": "PGTARGETSESSIONATTRS",
	"channel_binding":      "PGCHANNELBINDING",
	"gssencmode":           "PGGSSENCMODE",
}

// psqlInvocation resolves psql and the environment it should run in. configured
// is the DSN as the operator wrote it (it selects the wording of target); dsn is
// what resolveDSN returned. The connection reaches psql only through env.
func psqlInvocation(configured, dsn string, args, environ []string) (psqlInv, error) {
	path, err := exec.LookPath("psql")
	if err != nil {
		return psqlInv{}, errors.New(psqlMissing)
	}
	kw, err := parseConninfo(dsn)
	if err != nil {
		return psqlInv{}, err
	}

	// Every inherited PG* variable is dropped first: an exported PGHOST from the
	// operator's shell would otherwise be what psql dials, and the DSN's own
	// values are appended after, so each mapped keyword wins.
	env := make([]string, 0, len(environ)+len(kw))
	for _, kv := range environ {
		if strings.HasPrefix(kv, "PG") {
			continue
		}
		env = append(env, kv)
	}
	keys := make([]string, 0, len(kw))
	for k := range kw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable env order, so a test can compare it
	for _, k := range keys {
		// pgx pool settings are not libpq parameters and mean nothing to psql;
		// a DSN carrying them is still usable, so they are skipped, not refused.
		if strings.HasPrefix(k, "pool_") {
			continue
		}
		name, ok := pgEnv[k]
		if !ok {
			return psqlInv{}, fmt.Errorf("rafikid psql: cannot pass DSN parameter %q; run psql directly", k)
		}
		env = append(env, name+"="+kw[k])
	}

	argv := make([]string, 0, len(args)+1)
	argv = append(argv, "psql")
	argv = append(argv, args...)

	return psqlInv{path: path, argv: argv, env: env, target: psqlTarget(configured, kw)}, nil
}

// psqlTarget is the one line printed before exec, so the operator knows which
// database psql is about to open. The password is never part of it.
func psqlTarget(configured string, kw map[string]string) string {
	if configured == embeddedDSN {
		return "rafikid psql: embedded database at " + kw["host"]
	}
	out := "rafikid psql:"
	for _, k := range []string{"host", "port", "dbname", "user"} {
		if v, ok := kw[k]; ok {
			out += " " + k + "=" + v
		}
	}
	return out
}

// parseConninfo turns a DSN into libpq keyword/value pairs. Both shapes libpq
// accepts are handled: a postgres:// URL, and the keyword/value string pgx's
// own "embedded" server hands back.
func parseConninfo(dsn string) (map[string]string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return parseConninfoURL(dsn)
	}
	return parseConninfoKeywords(dsn)
}

// parseConninfoURL splits a postgres:// URL into keywords. Errors never carry
// the URL: it may hold a password.
func parseConninfoURL(dsn string) (map[string]string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// url.Parse's own error embeds the whole URL, so it is deliberately not
		// wrapped: the password would go straight to stderr.
		return nil, errors.New("rafikid psql: cannot parse the DSN as a postgres URL (the DSN is not echoed: it may hold a password)")
	}
	if strings.Contains(u.Host, ",") {
		return nil, errors.New("rafikid psql: multi-host DSNs are not supported")
	}

	kw := make(map[string]string, 8)
	if u.User != nil {
		if user := u.User.Username(); user != "" {
			kw["user"] = user
		}
		if password, ok := u.User.Password(); ok {
			kw["password"] = password
		}
	}
	if host := u.Hostname(); host != "" {
		kw["host"] = host
	}
	if port := u.Port(); port != "" {
		kw["port"] = port
	}
	if dbname := strings.TrimPrefix(u.Path, "/"); dbname != "" {
		kw["dbname"] = dbname
	}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			kw[k] = vs[len(vs)-1]
		}
	}
	return kw, nil
}

// parseConninfoKeywords parses libpq's keyword/value syntax: whitespace
// separated key=value pairs, a value optionally single-quoted with \' and \\
// escapes. The error text names the offending key or token and never a value
// that belongs to the password.
func parseConninfoKeywords(s string) (map[string]string, error) {
	kw := make(map[string]string, 8)
	i := 0
	for {
		for i < len(s) && isConninfoSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return kw, nil
		}
		start := i
		for i < len(s) && s[i] != '=' && !isConninfoSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			return nil, fmt.Errorf("rafikid psql: cannot parse DSN: %q is not a key=value pair",
				conninfoFragment(s[start:i]))
		}
		key := s[start:i]
		if !validConninfoKey(key) {
			return nil, fmt.Errorf("rafikid psql: cannot parse DSN: %q is not a keyword",
				conninfoFragment(key))
		}
		i++ // '='
		val, next, err := parseConninfoValue(s, i, key)
		if err != nil {
			return nil, err
		}
		i = next
		kw[key] = val
	}
}

// parseConninfoValue reads one value starting at i, returning it and the index
// just past it. A value may be quoted; the quote is the only thing that lets it
// hold whitespace, and \' and \\ are its escapes.
func parseConninfoValue(s string, i int, key string) (string, int, error) {
	var b strings.Builder
	if i < len(s) && s[i] == '\'' {
		i++
		for {
			if i >= len(s) {
				return "", 0, fmt.Errorf("rafikid psql: cannot parse DSN: unterminated quoted value for %q", key)
			}
			switch s[i] {
			case '\\':
				if i+1 >= len(s) {
					return "", 0, fmt.Errorf("rafikid psql: cannot parse DSN: unterminated quoted value for %q", key)
				}
				b.WriteByte(s[i+1])
				i += 2
			case '\'':
				i++
				if i < len(s) && !isConninfoSpace(s[i]) {
					return "", 0, fmt.Errorf("rafikid psql: cannot parse DSN: %q follows the quoted value of %q",
						string(s[i]), key)
				}
				return b.String(), i, nil
			default:
				b.WriteByte(s[i])
				i++
			}
		}
	}
	for i < len(s) && !isConninfoSpace(s[i]) {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), i, nil
}

// conninfoFragment makes a piece of the DSN safe to name in an error: a
// password's value is elided, everything else is shown so the operator can find
// it.
func conninfoFragment(tok string) string {
	if k, _, ok := strings.Cut(tok, "="); ok && strings.Contains(strings.ToLower(k), "pass") {
		return k + "=…"
	}
	return tok
}

// validConninfoKey reports whether key is shaped like a libpq parameter name.
func validConninfoKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isConninfoSpace is libpq's separator set.
func isConninfoSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
