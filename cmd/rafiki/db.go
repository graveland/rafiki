// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
)

// newDbCmd is `rafiki db`: a pass-through to rafikid's own `db` group.
//
// The verbs under it reach a database only the daemon's host can locate.
// RAFIKI_DB is daemon configuration — a DSN, or "embedded" for the private
// server whose socket lives inside its data directory — and resolving either
// means opening (or starting) PostgreSQL, which this binary must never do:
// cmd/rafiki links no pgx at all, pinned by TestClientDoesNotLinkPostgres.
// So the client execs `rafikid db …` instead of dialling anything, and rafikid
// on this machine resolves the database exactly as the daemon does.
//
// Argument handling is deliberately verbatim. The flag set is NON-INTERSPERSED,
// so pflag stops parsing at the first positional argument and hands back
// everything after it untouched — a bare "--" included. That is what makes
// `rafiki db psql -- -c 'select 1'` mean precisely what `rafikid db psql -- -c
// 'select 1'` means. The client's own flags (-P, -o, -c …) must therefore come
// BEFORE the verb; after it they belong to the passthrough, and a `-c` reaching
// psql is psql's --command, not the client's --color.
func newDbCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db <verb> [args...]",
		Short: "Run rafikid's database verbs on the daemon's host: psql, backup, restore",
		Long: `Run rafikid's own ` + "`db`" + ` verbs against rafikid's database on this machine.

There is no remote database face: RAFIKI_DB names either a server this host can
dial or the private embedded PostgreSQL, whose socket is a directory inside its
data directory. So these verbs belong to rafikid, and this command forwards to
` + "`rafikid db …`" + ` — it never opens a database connection itself.

A profile naming a remote daemon is refused before anything runs: the database
lives on the daemon's host, so run this command there (or over ssh).

That check is about where the DAEMON is, not where the database lives. rafikid
resolves RAFIKI_DB on its own host — its environment, and service.env when that
is where the daemon's settings live — so a local daemon configured against a
remote database means these verbs act on THAT database. rafikid db prints the
target (host and database, or the data directory) before it acts: read that line
before giving a destructive verb its --force.

Everything from the verb onward is passed through unchanged, including a bare
` + "`--`" + `. This client's flags (-P, -o, -c …) must therefore come BEFORE the
verb; rafikid's own flags (--db) belong AFTER it, where they are forwarded
verbatim (` + "`rafiki db psql --db …`" + `).

Example:
  rafiki db psql -- -c 'select count(*) from conversations'
  rafiki db backup /tmp/rafiki.dump`,
		Args: cobra.ArbitraryArgs,
		RunE: runDb,
	}
	// See the comment above: everything from the first positional onwards is
	// the passthrough's, so pflag must not look at it.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// dbExec is syscall.Exec behind a variable. The passthrough REPLACES this
// process with rafikid (which in turn replaces itself with psql for
// `rafiki db psql`), which is the only way to hand over the terminal without
// an intermediary: it inherits stdio by construction, needs no signal
// forwarding, and leaves the child's exit status with the shell. The downside
// is that a test cannot observe a successful exec, so the test that checks the
// argv records it here — the same seam rafikid's own psql uses (execProcess,
// cmd/rafikid/db_psql.go).
var dbExec = syscall.Exec

// runDb forwards the verb and its arguments to rafikid, after refusing a
// profile whose daemon is not on this machine.
func runDb(cmd *cobra.Command, args []string) error {
	// A bare `rafiki db` hands the terminal to `rafikid db`, which lists the
	// verbs it actually has. The verbs live in rafikid, so its help is the only
	// one that cannot drift from them.
	p, err := resolveProfile(cmd)
	if err != nil {
		return err
	}
	if !dbEndpointIsLocal(p) {
		return fmt.Errorf(
			"profile %q is not local: its endpoint is %s, and the database lives on the daemon's host — "+
				"run `rafiki db` on that machine, or select a local profile with -P",
			p.Name, p.URL)
	}

	rafikidPath, err := findDaemonBinary()
	if err != nil {
		return err
	}
	argv := append([]string{rafikidPath, "db"}, args...)
	return dbExec(rafikidPath, argv, os.Environ())
}

// dbEndpointIsLocal reports whether the profile's daemon runs on THIS machine.
// It mirrors connectEndpointFor's branch (an empty URL is the unix socket, a
// URL is a dialled endpoint) but deliberately does not CALL it: that function
// refuses a tokenless remote profile with errRemoteNoToken, and the credential
// is irrelevant here — nothing dials. Reporting a missing token would send the
// operator to `rafiki login` when the real answer is "run this on the host".
//
// A loopback host counts as local whether or not the URL is https, because the
// question is where the daemon runs, not how it is reached. A hostname that
// merely RESOLVES to loopback (an /etc/hosts entry) does not: resolving DNS in
// a refusal check would let a typo pass, and the failure it guards costs data.
func dbEndpointIsLocal(p profile.Resolved) bool {
	if p.URL == "" {
		return true
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}
