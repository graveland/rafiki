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

// newDbPsqlCmd opens psql on whatever database rafikid is configured for,
// including the embedded one. It exists because the embedded server's binaries
// ship no client (the zonky artifacts carry initdb, pg_ctl and postgres only),
// so an operator inspecting that database needs the system psql pointed at the
// right socket — which is a data directory path, not a hostname.
func newDbPsqlCmd() *cobra.Command {
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
			// --db is the db group's persistent flag; psql reads it from the
			// inherited set rather than declaring a second flag of that name.
			db, err := cmd.Flags().GetString("db")
			if err != nil {
				return err
			}
			if db == "" {
				return errors.New("--db (or RAFIKI_DB) is required")
			}
			// The client is looked up BEFORE the DSN is resolved: resolving an
			// embedded DSN can download tens of megabytes and run initdb, and a
			// user with no psql installed should learn that in a millisecond
			// rather than after that.
			if _, err := lookPsql(); err != nil {
				return err
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
			return execProcess(inv.path, inv.argv, inv.env)
		},
	}

	return cmd
}

// execProcess is syscall.Exec behind a variable. The passthrough REPLACES this
// process with psql — the only way it can hand the terminal to a client without
// going through a shell — and replacing the process is the one thing a test
// cannot observe. So the single test that has to see the argv psql receives
// records it here instead.
var execProcess = syscall.Exec

// pgToolInv is one fully resolved libpq client exec: the program, its argv, the
// environment carrying the connection, and the line telling the operator where
// that connection goes. psql and pg_dump share it because the rule that matters
// — the DSN rides PG*, never argv — is the same for both.
type pgToolInv struct {
	path   string   // absolute program path from exec.LookPath
	argv   []string // the program's name followed by args, unchanged; NEVER any DSN part
	env    []string // environ minus every PG* variable, plus the connection's PG* variables
	target string   // one line naming where the program will connect
}

// psqlMissing is the whole remedy an operator needs, in the error itself: psql
// is a separate package from the server on every platform rafiki runs on.
const psqlMissing = "psql not found on PATH; install the PostgreSQL client " + pgClientInstallHint

// pgClientInstallHint is the install remedy every libpq client these commands
// need shares, so psql and pg_dump never drift apart.
const pgClientInstallHint = "(macOS: brew install libpq && brew link --force libpq; " +
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

// lookPgClient finds a libpq client program, or reports the install remedy.
func lookPgClient(name, missing string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New(missing)
	}
	return path, nil
}

// lookPsql finds the psql binary, or reports the install remedy. RunE calls it
// before resolving the DSN and psqlInvocation calls it again, so the cheap check
// is what a user without psql sees first.
func lookPsql() (string, error) {
	return lookPgClient("psql", psqlMissing)
}

// psqlInvocation resolves psql and the environment it should run in. configured
// is the DSN as the operator wrote it (it selects the wording of target); dsn is
// what resolveDSN returned. The connection reaches psql only through env.
func psqlInvocation(configured, dsn string, args, environ []string) (pgToolInv, error) {
	path, err := lookPsql()
	if err != nil {
		return pgToolInv{}, err
	}
	return pgToolInvocation("rafikid psql", path, "psql", configured, dsn, args, environ)
}

// pgToolInvocation builds the argv, environment and target line for one libpq
// client (psql, pg_dump, ...). prefix is how the command names itself in
// messages; name is argv[0] and the word a refusal tells the operator to run
// instead. The DSN reaches the program through env ONLY — argv carries the
// arguments the caller wrote and nothing they did not.
func pgToolInvocation(prefix, path, name, configured, dsn string, args, environ []string) (pgToolInv, error) {
	kw, err := parseConninfo(dsn)
	if err != nil {
		return pgToolInv{}, fmt.Errorf("%s: %w", prefix, err)
	}

	// Every inherited PG* variable is dropped first: an exported PGHOST from the
	// operator's shell would otherwise be what the client dials, and the DSN's own
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
	for i, k := range keys {
		// pgx pool settings are not libpq parameters and mean nothing to a libpq
		// client; a DSN carrying them is still usable, so they are skipped, not
		// refused.
		if strings.HasPrefix(k, "pool_") {
			continue
		}
		envName, ok := pgEnv[k]
		if !ok {
			// A name libpq knows is worth printing: "service" or "passfile" tells
			// the operator exactly what this command cannot do. A name it does NOT
			// know is never printed — a password that an unquoted space or a `?`
			// split leaves its tail sitting where a key would be, and the tail can
			// even arrive through a URL's query string (postgres://u:1234?abc@h/db).
			if libpqKeywords[k] {
				return pgToolInv{}, fmt.Errorf("%s: cannot pass DSN parameter %q; run %s directly", prefix, k, name)
			}
			return pgToolInv{}, fmt.Errorf("%s: cannot pass the DSN parameter at position %d of %d; run %s directly",
				prefix, i+1, len(keys), name)
		}
		env = append(env, envName+"="+kw[k])
	}

	argv := make([]string, 0, len(args)+1)
	argv = append(argv, name)
	argv = append(argv, args...)

	return pgToolInv{path: path, argv: argv, env: env, target: pgToolTarget(prefix, configured, kw)}, nil
}

// pgToolTarget is the one line printed before exec, so the operator knows which
// database the client is about to reach. The password is never part of it.
func pgToolTarget(prefix, configured string, kw map[string]string) string {
	if configured == embeddedDSN {
		return prefix + ": embedded database at " + kw["host"]
	}
	out := prefix + ":"
	for _, k := range []string{"host", "port", "dbname", "user"} {
		if v, ok := kw[k]; ok {
			out += " " + k + "=" + v
		}
	}
	return out
}

// libpqKeywords is libpq's own parameter vocabulary: every name a connection
// string may carry, whether or not pgEnv can express it in the environment. It
// is what lets an error say "service is unsupported" instead of "some parameter
// is unsupported".
//
// What it buys is BOUNDED, not secret: the only DSN text this command can ever
// echo is one of these names or a byte offset. It is not a secrecy guarantee —
// a password whose fragment happens to BE a libpq parameter name
// (`password=my service=1`) is still printed. Tokens outside the vocabulary are
// never printed, which is why the vocabulary is kept small: growing it grows
// what can be echoed.
var libpqKeywords = map[string]bool{
	"application_name":          true,
	"channel_binding":           true,
	"client_encoding":           true,
	"connect_timeout":           true,
	"dbname":                    true,
	"fallback_application_name": true,
	"gssdelegation":             true,
	"gssencmode":                true,
	"gsslib":                    true,
	"host":                      true,
	"hostaddr":                  true,
	"keepalives":                true,
	"keepalives_count":          true,
	"keepalives_idle":           true,
	"keepalives_interval":       true,
	"krbsrvname":                true,
	"load_balance_hosts":        true,
	"options":                   true,
	"passfile":                  true,
	"password":                  true,
	"port":                      true,
	"replication":               true,
	"require_auth":              true,
	"requirepeer":               true,
	"service":                   true,
	"sslcert":                   true,
	"sslcompression":            true,
	"sslcrl":                    true,
	"sslcrldir":                 true,
	"sslkey":                    true,
	"sslmode":                   true,
	"sslnegotiation":            true,
	"sslpassword":               true,
	"sslrootcert":               true,
	"ssl_max_protocol_version":  true,
	"ssl_min_protocol_version":  true,
	"target_session_attrs":      true,
	"tcp_user_timeout":          true,
	"user":                      true,
}

// conninfoKeyPhrase names a DSN parameter inside a sentence: a libpq keyword by
// name, an unrecognised token by POSITION only. The position is the token's byte
// offset, and it exists because the token may be the tail of a password that an
// unquoted space or a stray quote cut in two. See libpqKeywords for exactly what
// the vocabulary does and does not promise.
func conninfoKeyPhrase(key string, off int) string {
	if libpqKeywords[key] {
		return fmt.Sprintf("the keyword %q", key)
	}
	return fmt.Sprintf("the parameter at offset %d", off)
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
		return nil, errors.New("cannot parse the DSN as a postgres URL (the DSN is not echoed: it may hold a password)")
	}
	if strings.Contains(u.Host, ",") {
		return nil, errors.New("multi-host DSNs are not supported")
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
// escapes.
//
// NO error here repeats the DSN's text. A token that failed to parse can be the
// continuation of a password (`password=my s3cret` makes `s3cret` a bare token,
// and `password='abc'def` leaves `def` after the closing quote), so naming the
// offending fragment would publish part of a secret. The errors locate it by
// byte offset and, when the preceding token is a real libpq parameter name, by
// that name. The vocabulary bounds what can be echoed to a fixed set of
// parameter names, which is what keeps a password out of the message — it is
// not a claim that a name is secret, since a password can contain the word
// "service" itself.
func parseConninfoKeywords(s string) (map[string]string, error) {
	kw := make(map[string]string, 8)
	i := 0
	lastKey := ""
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
			return nil, conninfoTokenError("a key=value pair", start, lastKey)
		}
		key := s[start:i]
		if !validConninfoKey(key) {
			return nil, conninfoTokenError("a keyword", start, lastKey)
		}
		i++ // '='
		val, next, err := parseConninfoValue(s, i, key, start)
		if err != nil {
			return nil, err
		}
		i = next
		kw[key] = val
		lastKey = key
	}
}

// conninfoTokenError reports an unparsable token WITHOUT its text: the offset
// locates it, and — only when the keyword it follows is a real libpq parameter —
// the keyword says what it follows. `validConninfoKey` is a SHAPE check, so the
// preceding token can be a password fragment (`password=my s3cret=1 x` makes
// `s3cret` the preceding "keyword"); gating on libpqKeywords is what keeps that
// text out of the message, and a fragment that happens to be a libpq name is the
// one thing this cannot rule out.
func conninfoTokenError(want string, off int, lastKey string) error {
	msg := fmt.Sprintf("cannot parse DSN: expected %s at offset %d", want, off)
	if libpqKeywords[lastKey] {
		msg += fmt.Sprintf(" (after the keyword %q)", lastKey)
	}
	return errors.New(msg)
}

// parseConninfoValue reads one value starting at i, returning it and the index
// just past it. A value may be quoted; the quote is the only thing that lets it
// hold whitespace, and \' and \\ are its escapes. keyOff is where the key token
// started, which is all an unrecognised key may be located by.
func parseConninfoValue(s string, i int, key string, keyOff int) (string, int, error) {
	var b strings.Builder
	if i < len(s) && s[i] == '\'' {
		i++
		for {
			if i >= len(s) {
				return "", 0, fmt.Errorf("cannot parse DSN: unterminated quoted value for %s",
					conninfoKeyPhrase(key, keyOff))
			}
			switch s[i] {
			case '\\':
				if i+1 >= len(s) {
					return "", 0, fmt.Errorf("cannot parse DSN: unterminated quoted value for %s",
						conninfoKeyPhrase(key, keyOff))
				}
				b.WriteByte(s[i+1])
				i += 2
			case '\'':
				i++
				if i < len(s) && !isConninfoSpace(s[i]) {
					// Neither the character nor an unrecognised key is named: after
					// `password='abc'` the character is the first byte of the
					// password's continuation.
					return "", 0, fmt.Errorf("cannot parse DSN: unexpected character after the quoted value of %s",
						conninfoKeyPhrase(key, keyOff))
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
