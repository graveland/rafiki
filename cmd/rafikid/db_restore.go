// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/cobra"
	"github.com/ulikunitz/xz"

	"go.graveland.dev/rafiki/pkg/embedpg"
	"go.graveland.dev/rafiki/pkg/paths"
)

// restoreKind is what a backup file IS, sniffed from its content. See
// sniffRestoreFile: the extension is never consulted, because `db backup`
// appends one but a rename (or a copy through a tool that renames) would make
// the extension a lie, and the two paths differ in what they destroy.
type restoreKind int

const (
	// restoreUnknown is the zero value ON PURPOSE: a kind that was never sniffed
	// cannot name what restoring would destroy, and the dispatch refuses it. A
	// sniff that ran always returns one of the two below.
	restoreUnknown restoreKind = iota
	// restoreDump is a pg_dump custom archive ("PGDMP" magic): restored over the
	// wire into the database the DSN names.
	restoreDump
	// restorePhysical is a tar of the embedded server's data directory, possibly
	// gzip- or xz-compressed: restored by replacing that directory.
	restorePhysical
)

// String names a kind the way a refusal reads best: "expected X or Y".
func (k restoreKind) String() string {
	switch k {
	case restoreDump:
		return "a pg_dump custom archive"
	case restorePhysical:
		return "a physical archive of the embedded data directory"
	}
	return "an unrecognised backup"
}

// The two magics a restore sniffs. A custom dump starts with the five bytes
// pg_dump writes ("PGDMP"); a tar header carries "ustar" at offset 257 in every
// dialect (USTAR, PAX, GNU), which is what makes an uncompressed physical
// archive recognisable without parsing it.
const (
	dumpMagic     = "PGDMP"
	tarMagic      = "ustar"
	tarMagicStart = 257
)

// pgRestoreMissing is the whole remedy an operator needs, in the error itself:
// pg_restore is a separate package from the server on every platform rafiki
// runs on.
const pgRestoreMissing = "pg_restore not found on PATH; install the PostgreSQL client " + pgClientInstallHint

// newDbRestoreCmd restores rafikid's database from one backup file.
func newDbRestoreCmd() *cobra.Command {
	var (
		force   bool
		noOwner bool
	)

	cmd := &cobra.Command{
		Use:   "restore <path>",
		Short: "Restore rafikid's database from a backup file (destructive: needs --force)",
		Long: `Restores rafikid's database from a backup file, choosing what to do by what the
file is, sniffed from its content rather than its extension:

  a pg_dump custom archive ("PGDMP" magic)
      pg_restore -d <database> --clean --if-exists --single-transaction, plus
      --no-owner when the connecting role is NOT a superuser
      over the wire: every object the ARCHIVE contains is dropped and recreated,
      and everything else in the database is left alone. The connection is passed
      in the PG* environment and never on pg_restore's command line — argv
      carries the database NAME, which pg_restore requires and which the --force
      report already told you. The whole restore is one transaction, so a failure
      rolls it back rather than leaving a half-rebuilt database.

      OWNERSHIP follows the connecting role, because --no-owner makes that role
      the owner of everything pg_restore creates. An unprivileged role can only
      own what it creates, so it gets the flag; a SUPERUSER does not, and the
      archive's own OWNER TO statements put the original owners back — for an
      in-place restore of a real install that is the daemon's unprivileged role.
      Restoring as a superuser WITH --no-owner would hand it the whole schema and
      leave the daemon with permission denied on its first query, which is why
      the flag is decided rather than constant. A role the archive names that
      does not exist on the target fails the whole restore (loudly) instead of
      having its objects reassigned — which is also the case for a dump taken
      from a DIFFERENT cluster, and the reason this command has its own
      --no-owner: it owns every restored object as the connecting role instead,
      and then ownership has to be fixed by hand or the daemon cannot read the
      data.

      On a database with the timescaledb extension it is bracketed by that
      extension's own timescaledb_pre_restore()/timescaledb_post_restore()
      calls: pg_restore --clean issues ALTER TABLE ONLY statements, which
      TimescaleDB refuses against a hypertable, so without them the restore
      fails on the first foreign key it tries to drop. The post call runs even
      when the restore fails, because pre_restore leaves the extension's DDL
      hooks off database-wide until it does. Those hooks need a SUPERUSER
      connection: they run ALTER DATABASE ... SET timescaledb.restoring, which
      no other role may set, and then stop background workers, which only a
      superuser may do (a GRANT SET ON PARAMETER covers the setting and not the
      workers). So a restore of a non-empty TimescaleDB database is refused
      cleanly, before anything is touched, unless --db names a superuser's
      connection. A database where the extension is NOT installed needs no hooks
      — the archive's own CREATE EXTENSION makes it — but rafiki's own dump
      creates the untrusted vector and pg_textsearch extensions too, so a
      non-superuser restore into a fresh database fails at that CREATE EXTENSION
      and rolls back. Whichever way: stop the daemon first if the database has
      the extension (a session opened while the restore runs inherits
      timescaledb.restoring for its whole life), and a dump restore REFUSES to
      start while a local rafikid answers on its socket unless --force accepts
      that.

  a physical archive (a tar, or a gzip/xz stream of one)
      the embedded server's data directory is replaced: the server is stopped
      (only if it is running), the directory on disk is moved to
      <dir>.replaced-<unix> — never deleted — and the archive is unpacked in its
      place, then the server is started again. A running rafikid is NOT stopped
      by this command: stopping the server drops its connections, so stop the
      daemon first.

A restore DESTROYS what it covers, so it does nothing without --force: the run
prints the database (name and host) or the data directory path that would be
replaced, and exits non-zero. Everything that can be refused is refused before
anything is touched — an unrecognised file, a physical archive against a
configured RAFIKI_DB, a missing client — and a physical archive is also unpacked
and checked in a staging directory beside the data directory before the live one
is moved, so a corrupt, truncated or escaping archive cannot take the install
down.

A physical archive is refused for a configured RAFIKI_DB, which names a server on
some other host: it has no data directory here, and only a dump can be restored
into it. Unpacking enforces the contract 'db backup' writes: names relative to
the data directory with no wrapper directory, and regular files, directories and
symlinks only. An absolute name, a '..' component, a symlink pointing out of the
tree, or any other entry type (a socket, a fifo) aborts the restore rather than
being skipped.

A failed restore rolls back: the previous data directory is put back where it
was (there is nothing to roll forward to, the archive is untouched) and the
server is left running if it was running. A successful one leaves the replaced
directory at <dir>.replaced-<unix> for the operator to keep or delete.

The whole physical swap happens while nothing else can change the server: the
same lock 'db backup' takes is held from before the server is stopped until it
is started again, so another rafikid command cannot start or initialise the
server underneath it. For the window no lock can cover — a SIGKILL between
moving the directory aside and putting the restored one in place — a marker is
left beside the data directory, and rafikid refuses to initialise an EMPTY
cluster while that marker or a <dir>.replaced-<unix> directory is there: a
missing data directory next to a preserved one is an interrupted restore, never
a fresh install, and starting an empty database over real data is the one
outcome worth refusing outright.

SIGINT, SIGTERM and SIGHUP cancel a restore rather than killing the process, so
an interrupted run takes the failure path and says so: a dump restore stops
pg_restore (whose single transaction rolls back) and puts TimescaleDB's hooks
back, and a physical one never starts the swap — before the first rename, and
again after a stop that took up to 90 seconds — restarting the server it stopped
and leaving the data directory untouched. Once the two renames have begun the
command finishes the swap — the renames themselves are not interruptible — and an
interrupt that arrives then rolls the completed swap BACK: the previous data
directory is put in place again, the restored tree is discarded, and the command
exits non-zero. A SECOND signal kills the process outright, which can leave a
TimescaleDB database in restoring mode (the manual remedy is SELECT
timescaledb_post_restore()) or a physical restore's marker in place.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := cmd.Flags().GetString("db")
			if err != nil {
				return err
			}
			return runRestore(cmd.Context(), db, args[0], force, noOwner, os.Stderr)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false,
		"replace what the backup covers; without it nothing is changed and the target is only named")
	cmd.Flags().BoolVar(&noOwner, "no-owner", false,
		"own every restored object as the CONNECTING role instead of the archive's original owners "+
			"(the escape hatch for a dump taken from another cluster whose role names do not exist here); "+
			"ownership then has to be fixed afterwards, or the daemon cannot read the data")

	return cmd
}

// runRestore sniffs the file, refuses everything it can refuse, and only then
// dispatches. The order here is the safety property: the --force gate, the
// embedded-only rule and the target description all run before anything is
// started, stopped, renamed or dropped.
func runRestore(ctx context.Context, configured, path string, force, noOwner bool, stderr io.Writer) error {
	if configured == "" {
		return errors.New("--db (or RAFIKI_DB) is required")
	}

	kind, err := sniffRestoreFile(path)
	if err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}

	if kind == restorePhysical && configured != embeddedDSN {
		return fmt.Errorf("rafikid db restore: %s is a physical archive of the embedded server's data "+
			"directory, and RAFIKI_DB names a server this host does not manage; a dump archive is the "+
			"only thing that restores into it", path)
	}

	// A dump restore into a database with the TimescaleDB extension changes a
	// DATABASE-level default (timescaledb.restoring), and a daemon connection
	// opened while the restore runs keeps it for its whole life: its DDL hooks
	// stay off after the restore has "succeeded", which is not something the
	// docs can be trusted to prevent. So a local daemon is refused here, before
	// anything is touched, unless the operator accepts it explicitly.
	if kind == restoreDump {
		if listening, socket := daemonListening(); listening {
			if !force {
				return fmt.Errorf("rafikid db restore: rafikid is running on this host (%s accepts connections), "+
					"and a connection it opens while this restore runs keeps timescaledb.restoring on for its "+
					"whole life — the extension's DDL hooks stay off in that session. Stop it first (rafiki "+
					"service stop), or re-run with --force to accept that a live daemon may hold a session in "+
					"restore mode", socket)
			}
			fmt.Fprintf(stderr, "rafikid db restore: warning: rafikid is running on this host (%s); a database "+
				"session it opens while this restore runs can keep timescaledb.restoring on until it is closed\n", socket)
		}
	}

	destroys, err := describeRestoreTarget(kind, configured)
	if err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}
	if !force {
		return fmt.Errorf("rafikid db restore: refusing to restore %s without --force: %s. "+
			"Nothing has been changed — re-run with --force once that is the target you want",
			path, destroys)
	}

	switch kind {
	case restoreDump:
		return restoreDumpArchive(ctx, configured, path, noOwner, stderr)
	case restorePhysical:
		return restorePhysicalArchive(ctx, configured, path, stderr)
	default:
		// Unreachable from sniffRestoreFile: a kind is always one of the two.
		return fmt.Errorf("rafikid db restore: %s is %s, which this command cannot restore", path, kind)
	}
}

// describeRestoreTarget says, in one sentence, exactly what a restore of this
// file would destroy. It resolves nothing: the embedded server is described from
// its paths, not started, so a --force-less run cannot bring a server up,
// download binaries or initdb a directory.
func describeRestoreTarget(kind restoreKind, configured string) (string, error) {
	if kind == restorePhysical {
		dir := newEmbeddedServer().DataDir
		return fmt.Sprintf("it would stop the embedded server if it is running, move the data directory "+
			"%s to %s%s<unix> (never deleted), unpack the archive in its place, and start the "+
			"server on the restored copy", dir, dir, embedpg.ReplacedDataDirPrefix), nil
	}

	if configured == embeddedDSN {
		dir := newEmbeddedServer().DataDir
		return fmt.Sprintf("it would run pg_restore -d %s --clean --single-transaction against the embedded "+
			"database %q served from %s: every object the ARCHIVE contains is dropped and recreated, and "+
			"everything else in that database is left alone",
			embedpg.Database, embedpg.Database, dir), nil
	}

	// The DSN is parsed for its name and host only: no connection is opened. Its
	// own parse errors are the safe kind — they never echo the DSN.
	kw, err := parseConninfo(configured)
	if err != nil {
		return "", err
	}
	dbname, err := restoreDatabaseNameFrom(kw)
	if err != nil {
		return "", err
	}
	host := kw["host"]
	if host == "" {
		host = hostaddrOrLocal(kw)
	}
	if port := kw["port"]; port != "" {
		host += ":" + port
	}
	return fmt.Sprintf("it would run pg_restore -d %s --clean --single-transaction against the database "+
		"%q on host %s: every object the ARCHIVE contains is dropped and recreated, and everything "+
		"else in that database is left alone", dbname, dbname, host), nil
}

// restoreDatabaseName is the database a dump restore targets. Only the NAME ever
// reaches argv, never the DSN: pg_restore needs -d explicitly — with no -d and an
// archive file it writes SQL to stdout instead of restoring — while the rest of
// the connection still comes from PG*. A database name is safe to name in a way a
// DSN is not, and it is already in the --force description and the target line.
func restoreDatabaseName(configured string) (string, error) {
	if configured == embeddedDSN {
		return embedpg.Database, nil
	}
	kw, err := parseConninfo(configured)
	if err != nil {
		return "", err
	}
	return restoreDatabaseNameFrom(kw)
}

// restoreDatabaseNameFrom is restoreDatabaseName over an already-parsed DSN. A DSN
// with no dbname is refused rather than guessed: libpq would fall back to the user
// name for psql, and copying that guess here would restore into a database the
// operator did not name.
func restoreDatabaseNameFrom(kw map[string]string) (string, error) {
	if name := kw["dbname"]; name != "" {
		return name, nil
	}
	return "", errors.New("the DSN names no database, and pg_restore cannot infer one; " +
		"name it in --db or RAFIKI_DB")
}

// hostaddrOrLocal names where a DSN with no host connects, so the sentence always
// says something.
func hostaddrOrLocal(kw map[string]string) string {
	if addr := kw["hostaddr"]; addr != "" {
		return addr
	}
	return "(the local socket)"
}

// restoreDumpArchive restores a pg_dump custom archive over the wire.
func restoreDumpArchive(ctx context.Context, configured, path string, noOwner bool, stderr io.Writer) error {
	// A pg_restore a target needs from PATH is looked up BEFORE the DSN is
	// resolved, as psql and pg_dump do it: an operator without one should learn
	// that in a millisecond. An embedded target is never refused here — its own
	// tree ships pg_restore, and resolving the DSN is what extracts it.
	if err := precheckPgClient(configured, "pg_restore", pgRestoreMissing); err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}
	dsn, err := resolveDSN(ctx, configured)
	if err != nil {
		return err
	}
	pgRestore, err := pgClientPath(configured, "pg_restore", pgRestoreMissing)
	if err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}
	// pg_restore needs the database named explicitly; the name rides argv, the
	// connection does not.
	database, err := restoreDatabaseName(configured)
	if err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}

	// Ownership is the difference between a usable database and a dead one, and
	// it follows the connecting role rather than a constant. `--no-owner` makes
	// the CONNECTING user the owner of everything pg_restore creates: correct
	// for the daemon's own unprivileged role, which can only own what it
	// creates, and wrong for a superuser, which would then own the whole schema
	// and leave the daemon role with permission denied on its next query. A
	// superuser restore therefore keeps the archive's OWNER TO statements, which
	// put the original owners back — for an in-place restore that IS the daemon's
	// role.
	superuser, err := restoreConnectedSuperuser(ctx, dsn)
	if err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}

	// argv holds the flags, the database name and the archive path — none is a
	// secret. The connection rides PG* alone.
	//
	// --single-transaction is what makes a failed dump restore leave the database
	// as it was: pg_restore runs the whole thing in one transaction and rolls it
	// back on the first error. Without it, --clean drops and recreates objects as
	// it goes, and a failure halfway leaves a half-rebuilt database.
	args := []string{"-d", database, "--clean", "--if-exists", "--single-transaction"}
	switch {
	case noOwner:
		// The operator's escape hatch, for a dump taken from another cluster
		// whose role names do not exist here: the archive's OWNER TO would fail
		// the whole transaction. The cost is stated where it is paid.
		args = append(args, "--no-owner")
		fmt.Fprintf(stderr, "rafikid db restore: --no-owner: every restored object will belong to the "+
			"connecting role; fix ownership afterwards (`ALTER ... OWNER TO`) or the daemon cannot read "+
			"the restored data\n")
	case !superuser:
		args = append(args, "--no-owner")
	default:
		fmt.Fprintf(stderr, "rafikid db restore: restoring object ownership from the archive (the "+
			"connecting role is a superuser); a role the archive names that does not exist here fails the "+
			"whole restore rather than reassigning its objects — pass --no-owner to own everything as "+
			"this role instead\n")
	}
	// `--` ends pg_restore's options, so a file whose name begins with "-" is a
	// filename and not a flag.
	args = append(args, "--", path)

	inv, err := pgToolInvocation("rafikid db restore", pgRestore, "pg_restore", configured, dsn,
		args, os.Environ())
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, inv.target)

	// TimescaleDB has to be told the restore is coming, or pg_restore's own
	// `ALTER TABLE ONLY` statements are refused against every hypertable. See
	// beginTimescaleRestore: a database without the extension gets a no-op.
	post, err := timescaleRestoreHooks(ctx, dsn, database)
	if err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}

	runErr := func() error {
		cmd := exec.CommandContext(ctx, inv.path, inv.argv[1:]...)
		cmd.Env = inv.env
		// pg_restore's own stderr is passed straight through: a refusal such as
		// an unsupported archive version is the only place it is explained.
		// stdout stays clean, so a script's output is unaffected.
		cmd.Stdout = io.Discard
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("rafikid db restore: pg_restore: %w", err)
		}
		return nil
	}()

	// The post hook runs on BOTH paths. timescaledb_pre_restore sets a
	// DATABASE-level GUC, so a restore that died between the two calls would
	// leave the extension's DDL hooks off for every later session on that
	// database — a hypertable created afterwards would silently be a plain
	// table, which is a worse outcome than the restore failing loudly. Its
	// context is detached and bounded for the same reason: a cancelled command
	// still has to put the extension back.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timescalePostRestoreTimeout)
	defer cancel()
	if perr := post(pctx); perr != nil {
		return errors.Join(runErr, fmt.Errorf("rafikid db restore: %w", perr))
	}
	if runErr != nil {
		return runErr
	}

	fmt.Fprintf(stderr, "rafikid db restore: restored %s (format %s)\n", path, formatDump)
	return nil
}

// timescalePostRestoreTimeout bounds the post hook, which runs detached from the
// restore's own context: it must complete even when the operator interrupted the
// restore.
const timescalePostRestoreTimeout = 30 * time.Second

// timescaleRestoreHooks brackets a dump restore with TimescaleDB's own pre/post
// restore calls when the target database has the extension. It is a variable so
// the dump path's tests can stand in for the server: what has to be right is the
// ORDER — pre before pg_restore, post even when pg_restore fails — and that is
// observable without a database. The third argument is the database NAME (already
// printed with the target), used by the error that says the database was left in
// restoring mode; it is never a DSN.
var timescaleRestoreHooks = beginTimescaleRestore

// beginTimescaleRestore runs timescaledb_pre_restore() when the database dsn
// names has the extension, and returns the matching post call. A database
// without it gets a no-op, because there are no hooks to disable there.
//
// It exists because `pg_restore --clean` drops and recreates every object in the
// archive with `ALTER TABLE ONLY`, and TimescaleDB refuses those against a
// hypertable: "ONLY option not supported on hypertable operations". The
// extension's own hooks are the supported way through — pre_restore sets the
// DATABASE-level timescaledb.restoring GUC, which makes the extension leave
// hypertables alone while pg_restore works, and post_restore clears it. Without
// them a dump restore into a TimescaleDB install fails on the first foreign key
// it tries to drop, and TimescaleDB is what the deployment guide describes.
func beginTimescaleRestore(ctx context.Context, dsn, database string) (func(context.Context) error, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to the server: %w", err)
	}
	var installed bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&installed); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("reading the installed extensions: %w", err)
	}
	if !installed {
		_ = conn.Close(context.WithoutCancel(ctx))
		return func(context.Context) error { return nil }, nil
	}
	if _, err := conn.Exec(ctx, `SELECT timescaledb_pre_restore()`); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		// The hint is the remedy for ONE failure, the privilege refusal; a
		// cancelled context or a lost connection gets the real error instead.
		// Those can still have COMMITTED the setting before the error reached
		// us, and it is database state, so putting it back is attempted on
		// every other failure path too (post_restore is idempotent).
		if isPermissionDenied(err) {
			return nil, fmt.Errorf("timescaledb_pre_restore: %w; %s", err, timescaleSuperuserHint)
		}
		return nil, errors.Join(fmt.Errorf("timescaledb_pre_restore: %w", err),
			clearTimescaleRestoring(ctx, dsn, database))
	}
	// The session stays open across pg_restore: the flag it set is database-wide,
	// but the post call belongs to the same server. It may die meanwhile (an idle
	// timeout, a proxy, a server restart), which is why the post call redials
	// once: the flag it clears is database state, so a fresh connection is as
	// good as the old one, and the alternative is a database left in restoring
	// mode.
	return func(fctx context.Context) error {
		defer func() { _ = conn.Close(context.WithoutCancel(fctx)) }()
		first := execPostRestore(fctx, conn)
		if first == nil {
			return nil
		}
		fresh, derr := pgx.Connect(fctx, dsn)
		if derr != nil {
			return postRestoreLeftTheDatabase(database, first, derr)
		}
		defer func() { _ = fresh.Close(context.WithoutCancel(fctx)) }()
		if rerr := execPostRestore(fctx, fresh); rerr != nil {
			return postRestoreLeftTheDatabase(database, first, rerr)
		}
		return nil
	}, nil
}

// execPostRestore runs the post hook on one session.
func execPostRestore(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, `SELECT timescaledb_post_restore()`); err != nil {
		return fmt.Errorf("timescaledb_post_restore: %w", err)
	}
	return nil
}

// timescaleSuperuserHint is the remedy for the refusal a production install WILL
// hit: timescaledb_pre_restore() runs `ALTER DATABASE ... SET
// timescaledb.restoring`, and that setting is superuser-only, so the hook cannot
// run as the daemon's own role (which the deployment guide deliberately keeps
// unprivileged). Naming it here saves an operator from reading the hook's own
// CONTEXT lines.
const timescaleSuperuserHint = "the hook runs ALTER DATABASE ... SET timescaledb.restoring, a superuser-only " +
	"setting, so the restore has to connect as a superuser (the daemon's own role deliberately is not one)"

// postRestoreLeftTheDatabase is the error for a post hook that could not be run
// at all, even after a redial. It names the state the database is in and the
// manual remedy, because there is nothing left to try automatically and silence
// would leave every later session with the extension's DDL hooks switched off.
func postRestoreLeftTheDatabase(database string, errs ...error) error {
	return fmt.Errorf("the extension could not be put back into service (%v): the database %q "+
		"is left with timescaledb.restoring on, which switches the extension's DDL hooks off for every "+
		"session started from now on — clear it with `rafikid db psql -- -c 'SELECT timescaledb_post_restore()'` "+
		"as a superuser", errors.Join(errs...), database)
}

// clearTimescaleRestoring puts the extension back after a pre_restore whose
// outcome is unknown, on its OWN connection: the session that ran it is gone,
// which is usually why the outcome is unknown. nil means the database is back to
// normal.
func clearTimescaleRestoring(ctx context.Context, dsn, database string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timescalePostRestoreTimeout)
	defer cancel()
	conn, err := pgx.Connect(cctx, dsn)
	if err != nil {
		return postRestoreLeftTheDatabase(database, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(cctx)) }()
	if perr := execPostRestore(cctx, conn); perr != nil {
		return postRestoreLeftTheDatabase(database, perr)
	}
	return nil
}

// restoreConnectedSuperuser reports whether the role dsn connects as is a
// PostgreSQL superuser. It is a variable so the dump path's tests can answer
// without a database; the pooled/one-shot connection it opens is closed again
// before pg_restore runs.
var restoreConnectedSuperuser = connectedRoleIsSuperuser

// connectedRoleIsSuperuser asks the server what it already knows about the role
// on this connection: no pg_authid read, no privilege beyond the connection the
// restore is about to use anyway.
func connectedRoleIsSuperuser(ctx context.Context, dsn string) (bool, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false, fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var superuser bool
	if err := conn.QueryRow(ctx,
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&superuser); err != nil {
		return false, fmt.Errorf("reading the connecting role: %w", err)
	}
	return superuser, nil
}

// daemonListening reports whether a rafikid on THIS host answers on its control
// socket, and names the socket. It is a variable so the refusal can be tested
// without a daemon.
var daemonListening = localDaemonListening

// localDaemonListening dials the daemon's unix socket: anything listening is a
// daemon, and a socket file with nothing behind it is not. The deadline is short
// because this only decides whether to ask the operator to stop something.
func localDaemonListening() (bool, string) {
	socket := paths.SocketPath()
	conn, err := net.DialTimeout("unix", socket, 250*time.Millisecond)
	if err != nil {
		return false, socket
	}
	_ = conn.Close()
	return true, socket
}

// syncDir flushes a directory's own entries (the pointers a rename writes) to
// disk. A rename is atomic but not durable: without this, a power loss can
// leave the directory pointing at neither the old tree nor the new one.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// isPermissionDenied reports whether err is PostgreSQL's own refusal (SQLSTATE
// 42501) rather than a cancellation or a lost connection — the one case where
// the superuser requirement is the remedy the operator needs told.
func isPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// embeddedRestoreOps are the operations a physical restore performs on the
// private server. They are function fields for the same reason the backup's are:
// the rollback is the safety property here, and the only way to prove it is to
// make a step fail — the real set is pg_ctl and the archiver through pkg/embedpg,
// the test set records the order and writes a fixture tree.
type embeddedRestoreOps struct {
	// lock takes the flock every other state change on the server takes, and
	// returns its release. A restore holds it across status → stop → the two
	// renames → start, which is what keeps a concurrent Ensure (any other rafikid
	// command resolving this DSN) from starting a postmaster on a directory about
	// to be renamed away, or initdb'ing a fresh cluster between the two renames.
	lock func(context.Context, embedpg.Server) (func(), error)
	// status reports whether the server is running, WITHOUT starting it. A
	// restore must know what it owes a restart, and Ensure is the wrong question:
	// it starts a server that was down.
	status func(context.Context, embedpg.Server) (bool, error)
	// stop shuts the server down before its directory is moved. The CALLER holds
	// lock, so it is Server.StopLocked — Stop would take the flock a second time
	// from this process and deadlock against ourselves. It runs detached from the
	// restore's context and is bounded by embedpg.StopTimeout, because a
	// half-stopped server is worse than a slow one.
	stop func(context.Context, embedpg.Server) error
	// start brings the server back up: on the restored directory after a success,
	// on the untouched or replaced-back one after a failure. It is
	// Server.EnsureLocked for the same reason stop is StopLocked, and because a
	// restart must end up exactly where a normal start would — same readiness
	// check, same rafiki.conf written for THIS install's paths (an archive restored
	// from another install names that install's socket dir), and the 0700 mode
	// re-asserted on a directory this command created.
	start func(context.Context, embedpg.Server) error
	// unpack unpacks the physical archive at its first argument into the
	// directory at its second.
	unpack func(archive, dir string) error
	// sync flushes an unpacked tree to disk before it becomes the live data
	// directory. A clean shutdown's data directory is what a restarted server
	// starts from with no crash recovery, so a torn tree after a power loss is
	// a corrupt cluster that looks restored. It is pkg/embedpg's
	// `initdb --sync-only`, the same call PostgreSQL itself makes.
	sync func(ctx context.Context, dir string) error
}

// restoreStamp is when one restore began. The sibling names it builds carry it,
// so two restores of the same install a second apart cannot collide; a variable
// so a test can pin the names a refusal and a report mention.
var restoreStamp = time.Now

// renamePath is os.Rename behind a variable. The two renames ARE the destructive
// step of a physical restore, and the case the rollback must get right — the
// second one failing after the live directory is already aside — cannot be
// reached from a test any other way.
var renamePath = os.Rename

var restoreEmbedded = embeddedRestoreOps{
	lock:   func(ctx context.Context, s embedpg.Server) (func(), error) { return s.Lock(ctx) },
	status: func(ctx context.Context, s embedpg.Server) (bool, error) { return s.Status(ctx) },
	stop: func(ctx context.Context, s embedpg.Server) error {
		return s.StopLocked(context.WithoutCancel(ctx))
	},
	start: func(ctx context.Context, s embedpg.Server) error {
		_, err := s.EnsureLocked(context.WithoutCancel(ctx))
		return err
	},
	unpack: unpackDataDirArchive,
	sync: func(ctx context.Context, dir string) error {
		return embeddedServer().SyncTree(ctx, dir)
	},
}

// restorePhysicalArchive replaces the embedded server's data directory with the
// archive at path.
//
// The order is chosen so that every refusal happens before any destructive step,
// which for the bytes means: unpack and check a STAGING directory first, and only
// then stop the server and move the live directory aside. A corrupt, truncated,
// escaping or wrong-major-version archive therefore leaves the install exactly as
// it was, still running.
//
// The embedpg flock is held for the whole sequence (unpack included, so the
// staging tree cannot be built while another process replaces the install under
// it). Without it a concurrent `rafikid db psql` — which runs Ensure — could start a
// postmaster on the directory the next rename moves away, or initdb a fresh
// cluster in the gap between the two renames. The two renames are on one
// filesystem, so the window in which no data directory exists is two rename(2)
// calls long, and a marker file covers the one case neither the lock nor the
// rollback can: a SIGKILL in that window (see embedpg's init path, which refuses
// to initialise an empty cluster beside that marker or a preserved directory).
func restorePhysicalArchive(ctx context.Context, configured, path string, stderr io.Writer) (err error) {
	if !isEmbedded(configured) {
		return fmt.Errorf("rafikid db restore: %s is a physical archive of the embedded server's data "+
			"directory, and %s names a server this host does not manage; a dump archive is the only "+
			"thing that restores into it", path, embeddedSettingHint)
	}
	s := embeddedServer()
	stamp := restoreStamp().Unix()
	staging := fmt.Sprintf("%s.restoring-%d", s.DataDir, stamp)
	replaced := s.DataDir + embedpg.ReplacedDataDirPrefix + fmt.Sprint(stamp)
	marker := s.RestoreInProgressPath()

	// The lock spans the whole sequence. It is registered before the rollback so
	// that the rollback runs while it is still held — it restarts the server, and
	// it must not be racing another process while it does.
	release, lerr := restoreEmbedded.lock(ctx, s)
	if lerr != nil {
		return fmt.Errorf("rafikid db restore: %w", lerr)
	}
	defer release()

	// Refusals that need nothing done: a leftover from an earlier restore, and a
	// data directory that exists but is not one. Both are re-read under the lock,
	// because until it was taken another process could have created either.
	if _, serr := os.Lstat(replaced); serr == nil {
		return fmt.Errorf("rafikid db restore: %s already exists; a restore never overwrites the directory "+
			"it replaced, so move it away first", replaced)
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return fmt.Errorf("rafikid db restore: %s: %w", replaced, serr)
	}
	hadDataDir := false
	if info, serr := os.Stat(s.DataDir); serr == nil {
		if !info.IsDir() {
			return fmt.Errorf("rafikid db restore: %s is not a directory", s.DataDir)
		}
		hadDataDir = true
	} else if !errors.Is(serr, fs.ErrNotExist) {
		// A missing data directory is not an error: restoring into a fresh install
		// is how a database comes back after one. Creating the archive's tree IS
		// the restore, and there is nothing to move aside.
		return fmt.Errorf("rafikid db restore: %s: %w", s.DataDir, serr)
	}

	if serr := os.MkdirAll(filepath.Dir(s.DataDir), 0o700); serr != nil {
		return fmt.Errorf("rafikid db restore: %w", serr)
	}
	// The staging directory gets the data directory's own mode: it is a copy of a
	// data directory in every respect except its name.
	if serr := os.Mkdir(staging, 0o700); serr != nil {
		return fmt.Errorf("rafikid db restore: creating %s: %w", staging, serr)
	}

	// Every flag below is set AFTER the step it describes SUCCEEDED: the rollback
	// decides what to undo from them, and one set too early makes it undo work this
	// command did not do.
	stopped := false    // the server was running, and this command stopped it
	movedAside := false // the live directory is at `replaced`
	placed := false     // the unpacked tree is at s.DataDir

	// The defer IS the rollback, and it runs however the body ended — a returned
	// error or a cancelled context. It removes only this command's own work (the
	// staging tree, and the tree it unpacked), and puts back what was there before.
	// `placed` is what separates them: without it a failed second rename would
	// delete a directory this command never created.
	defer func() {
		if err == nil {
			return
		}
		_ = os.RemoveAll(staging)
		if placed {
			// The tree at DataDir is this command's, but a failed start may have left
			// a postmaster running on it: it is stopped before the tree it is using
			// goes away. If it will not stop, BOTH trees stay exactly where they are —
			// deleting the files under a live postmaster is how a rollback corrupts a
			// cluster, and the operator still has the original in `replaced` — and the
			// marker stays, so a missing data directory later refuses rather than
			// initdb'ing an empty cluster.
			if serr := restoreEmbedded.stop(context.WithoutCancel(ctx), s); serr != nil {
				err = errors.Join(err, fmt.Errorf("rafikid db restore: stopping the database the failed "+
					"restore started: %w; the restored directory is still at %s and the previous one at %s",
					serr, s.DataDir, replaced))
				return
			}
			if rerr := os.RemoveAll(s.DataDir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("rafikid db restore: removing the restored directory: %w", rerr))
			}
		}
		restored := !movedAside
		if movedAside {
			if rerr := renamePath(replaced, s.DataDir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("rafikid db restore: the previous data directory is at %s: %w",
					replaced, rerr))
			} else {
				restored = true
				// The forward path flushes the parent after its renames; the
				// rollback is the same kind of pointer write and gets the same
				// treatment, or a power loss right after a rollback can leave
				// the directory naming a tree that is not there.
				if serr := syncDir(filepath.Dir(s.DataDir)); serr != nil {
					err = errors.Join(err, fmt.Errorf("rafikid db restore: flushing the directory the "+
						"previous data directory was moved back into: %w", serr))
				}
			}
		}
		if restored {
			// The install is back where it was, so the marker has done its job. A
			// leftover one would make the next start refuse rather than initdb —
			// correct while the data is displaced, wrong once it is not.
			_ = os.Remove(marker)
		}
		if stopped {
			if rerr := restoreEmbedded.start(context.WithoutCancel(ctx), s); rerr != nil {
				err = errors.Join(err, fmt.Errorf("rafikid db restore: restarting the embedded database: %w", rerr))
			}
		}
	}()

	if uerr := restoreEmbedded.unpack(path, staging); uerr != nil {
		return fmt.Errorf("rafikid db restore: unpacking %s: %w", path, uerr)
	}
	if verr := checkRestoredDataDir(staging); verr != nil {
		return fmt.Errorf("rafikid db restore: %s: %w", path, verr)
	}

	// Everything above is refusable work; everything below takes the install out
	// of service. An interrupt that has already arrived stops here, while the
	// server is still running the database it was running: the rollback removes
	// the staging tree, and no data directory is ever moved.
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("rafikid db restore: %w; nothing was replaced", cerr)
	}
	// The tree becomes the live data directory in a moment and a server starts
	// on it with NO crash recovery to replay, so its files have to be on the
	// platter before that: a power loss after "restored" would otherwise leave a
	// torn cluster that looks healthy.
	if serr := restoreEmbedded.sync(ctx, staging); serr != nil {
		return fmt.Errorf("rafikid db restore: flushing the unpacked data directory: %w", serr)
	}

	if hadDataDir {
		running, serr := restoreEmbedded.status(ctx, s)
		if serr != nil {
			return fmt.Errorf("rafikid db restore: %w", serr)
		}
		if running {
			if serr := restoreEmbedded.stop(ctx, s); serr != nil {
				// The server was never stopped, so no restart is owed: `stopped`
				// stays false.
				return fmt.Errorf("rafikid db restore: %w", serr)
			}
			stopped = true

			// A stop can take until embedpg.StopTimeout (90s on a busy
			// server), and an interrupt that arrived while it ran is honoured
			// HERE — before the marker, before any directory moves. The
			// rollback only has to restart a server this command stopped, so
			// refusing costs one restart and saves an operator who pressed
			// Ctrl-C the destructive outcome.
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("rafikid db restore: %w; the data directory was not replaced", cerr)
			}
		}

		// The marker is written BEFORE the directory moves, so the gap between the
		// two renames is recorded: a run killed there leaves no data directory and
		// no rollback, and the next start must refuse to initdb an empty cluster
		// over a database that is still on disk.
		if werr := os.WriteFile(marker, []byte("rafikid db restore is replacing this data directory; "+
			"the previous one is at "+replaced+"\n"), 0o600); werr != nil {
			return fmt.Errorf("rafikid db restore: writing %s: %w", marker, werr)
		}

		// The one move that takes the live directory out of service. It is a
		// rename, never a delete: <dir>.replaced-<unix> is where yesterday's
		// database stays until the operator deletes it.
		if merr := renamePath(s.DataDir, replaced); merr != nil {
			return fmt.Errorf("rafikid db restore: moving %s aside: %w", s.DataDir, merr)
		}
		movedAside = true
		if serr := syncDir(filepath.Dir(s.DataDir)); serr != nil {
			return fmt.Errorf("rafikid db restore: flushing the directory the data directory was moved "+
				"out of: %w", serr)
		}
	}

	if rerr := renamePath(staging, s.DataDir); rerr != nil {
		return fmt.Errorf("rafikid db restore: moving the restored directory into place: %w", rerr)
	}
	placed = true
	if serr := syncDir(filepath.Dir(s.DataDir)); serr != nil {
		return fmt.Errorf("rafikid db restore: flushing the directory the restored data directory was "+
			"moved into: %w", serr)
	}

	if serr := restoreEmbedded.start(ctx, s); serr != nil {
		return fmt.Errorf("rafikid db restore: starting the restored database: %w", serr)
	}
	if rerr := os.Remove(marker); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		// Not fatal: the restore itself succeeded and the marker only matters if a
		// data directory goes missing later, so it is reported rather than undone.
		fmt.Fprintf(stderr, "rafikid db restore: warning: could not remove %s: %v\n", marker, rerr)
	}

	fmt.Fprintf(stderr, "rafikid db restore: restored %s (physical archive) into %s\n", path, s.DataDir)
	if movedAside {
		fmt.Fprintf(stderr, "rafikid db restore: the directory it replaced is at %s\n", replaced)
	}
	return nil
}

// checkRestoredDataDir refuses an unpacked tree that is not a data directory
// these binaries can serve. It runs while the tree is still in the staging
// directory, which is what keeps "a backup from another major version" from
// taking the live directory out of service before it is refused: there is no
// upgrade path here (pkg/embedpg's Ensure refuses the same mismatch, but only
// after the swap).
func checkRestoredDataDir(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "PG_VERSION"))
	if err != nil {
		return fmt.Errorf("the archive is not a PostgreSQL data directory (it has no PG_VERSION): %w", err)
	}
	if got := strings.TrimSpace(string(b)); got != embedpg.MajorVersion {
		return fmt.Errorf("the archive is a PostgreSQL %s data directory and these binaries are %s; "+
			"major upgrades are not supported, so this rafikid cannot restore it", got, embedpg.MajorVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, "postgresql.conf")); err != nil {
		return fmt.Errorf("the archive is not a complete data directory (it has no postgresql.conf): %w", err)
	}
	return nil
}

// sniffRestoreFile reports what kind of backup the file at path is, by reading
// its content: a pg_dump custom archive begins with "PGDMP", and a physical
// archive is a tar — carried plain, or through the gzip and xz streams `db
// backup` writes — whose first header parses. The extension is never consulted.
//
// A compressed stream's magic proves the container, not the content, so the tar
// itself is probed: a gzip of something that is not a tar is refused here rather
// than discovered after the live data directory has been moved aside.
func sniffRestoreFile(path string) (restoreKind, error) {
	f, err := os.Open(path)
	if err != nil {
		return restoreUnknown, err
	}
	defer f.Close()

	head := make([]byte, 512) // one tar block: long enough for both magics
	n, rerr := io.ReadFull(f, head)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return restoreUnknown, rerr
	}
	head = head[:n]

	if bytes.HasPrefix(head, []byte(dumpMagic)) {
		return restoreDump, nil
	}
	if !looksLikeTar(head) {
		return restoreUnknown, fmt.Errorf("%s is not a rafikid backup: expected %s or %s",
			path, restoreDump, restorePhysical)
	}
	if perr := probeDataDirArchive(path); perr != nil {
		return restoreUnknown, fmt.Errorf("%s is not a rafikid backup: expected %s or %s — %w",
			path, restoreDump, restorePhysical, perr)
	}
	return restorePhysical, nil
}

// looksLikeTar reports whether head begins a tar archive or a gzip/xz stream of
// one.
func looksLikeTar(head []byte) bool {
	if len(head) >= tarMagicStart+len(tarMagic) &&
		string(head[tarMagicStart:tarMagicStart+len(tarMagic)]) == tarMagic {
		return true
	}
	return isGzipStream(head) || isXZStream(head)
}

// gzipStreamMagic and xzStreamMagic are the leading bytes of the two
// compressors `db backup` writes.
var (
	gzipStreamMagic = []byte{0x1f, 0x8b}
	xzStreamMagic   = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}
)

// isGzipStream reports whether head begins a gzip stream.
func isGzipStream(head []byte) bool {
	return bytes.HasPrefix(head, gzipStreamMagic)
}

// isXZStream reports whether head begins an xz stream.
func isXZStream(head []byte) bool {
	return bytes.HasPrefix(head, xzStreamMagic)
}

// probeDataDirArchive opens the archive and reads its first header, so a file
// that carries a tar container but no tar entry is refused as unreadable
// before anything is unpacked.
func probeDataDirArchive(path string) error {
	r, err := openDataDirArchive(path)
	if err != nil {
		return err
	}
	defer r.Close()

	if _, terr := tar.NewReader(r).Next(); terr != nil {
		if errors.Is(terr, io.EOF) {
			return errors.New("it is an empty archive, so there is no data directory in it")
		}
		return fmt.Errorf("it is not a tar archive: %w", terr)
	}
	return nil
}

// dataDirArchiveReader is one archive opened for reading: the decompressing
// reader, and the whole close chain behind it. xz's reader holds no state worth
// closing, so only gzip and the file appear in closers.
type dataDirArchiveReader struct {
	io.Reader
	closers []io.Closer
}

// Close closes the chain innermost-first, so a decompressor's error is not lost
// behind the file's.
func (r *dataDirArchiveReader) Close() error {
	var errs []error
	for i := len(r.closers) - 1; i >= 0; i-- {
		errs = append(errs, r.closers[i].Close())
	}
	return errors.Join(errs...)
}

// openDataDirArchive opens the physical archive at path, decompressing it when it
// is gzip or xz. Which compressor is asked of the bytes, not of the name.
func openDataDirArchive(path string) (*dataDirArchiveReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(f)
	head, _ := br.Peek(len(xzStreamMagic)) // a short prefix is simply "neither"

	switch {
	case isGzipStream(head):
		gr, gerr := gzip.NewReader(br)
		if gerr != nil {
			f.Close()
			return nil, fmt.Errorf("reading the gzip stream in %s: %w", path, gerr)
		}
		return &dataDirArchiveReader{Reader: gr, closers: []io.Closer{gr, f}}, nil
	case isXZStream(head):
		xr, xerr := xz.NewReader(br)
		if xerr != nil {
			f.Close()
			return nil, fmt.Errorf("reading the xz stream in %s: %w", path, xerr)
		}
		return &dataDirArchiveReader{Reader: xr, closers: []io.Closer{f}}, nil
	}
	return &dataDirArchiveReader{Reader: br, closers: []io.Closer{f}}, nil
}

// unpackDataDirArchive unpacks the physical archive at path into dir, which must
// be a directory this command created for exactly this purpose.
//
// The unpacker is pkg/embedpg's ExtractTar, and these are its containment rules:
// an absolute name or a `..` component is refused lexically; a write may not be
// redirected through a symlink (O_NOFOLLOW on the final component, and the
// deepest existing ancestor of the path resolved); a symlink may not be absolute
// or climb with `..`; every link is created only after the last file; and any
// other entry type — a socket, a fifo, a device — is refused rather than skipped,
// because `db backup` refuses to archive one (a skipped entry restores as a
// corrupt database). That code lives in ONE place deliberately: a second copy
// here would be the copy that drifts, and it is the same untrusted-input problem
// in both directions. `db backup` applies the same symlink rule before it writes,
// so what this refuses is what the archiver refuses to produce.
func unpackDataDirArchive(path, dir string) error {
	r, err := openDataDirArchive(path)
	if err != nil {
		return err
	}
	defer r.Close()
	return embedpg.ExtractTar(r, dir)
}
