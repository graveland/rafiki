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

      Restoring into a FRESH database is a superuser step: rafiki's own dump
      creates the untrusted vector and pg_textsearch extensions, so a
      non-superuser restore fails at that CREATE EXTENSION and rolls back. And a
      dump restore REFUSES to start while a local rafikid answers on its socket,
      because that daemon keeps writing to the tables this restore replaces:
      stop it first (rafiki service stop), or pass --force to accept it.

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
pg_restore (whose single transaction rolls back), and a physical one never starts
the swap — before the first rename, and again after a stop that took up to 90
seconds — restarting the server it stopped and leaving the data directory
untouched. Once the two renames have begun the command finishes the swap — the
renames themselves are not interruptible — and an interrupt that arrives then
rolls the completed swap BACK: the previous data directory is put in place again,
the restored tree is discarded, and the command exits non-zero. A SECOND signal
kills the process outright, which can leave a physical restore's marker in
place.`,
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

	// A dump restore replaces the tables a running daemon keeps writing to, so a
	// local daemon is refused here, before anything is touched, unless the
	// operator accepts it explicitly.
	if kind == restoreDump {
		if listening, socket := daemonListening(); listening {
			if !force {
				return fmt.Errorf("rafikid db restore: rafikid is running on this host (%s accepts connections), "+
					"and it keeps writing to the tables this restore replaces. Stop it first (rafiki service "+
					"stop), or re-run with --force to restore under a live daemon", socket)
			}
			fmt.Fprintf(stderr, "rafikid db restore: warning: rafikid is running on this host (%s); it may "+
				"write to the tables this restore replaces\n", socket)
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

	// pg_restore's own stderr is passed straight through: a refusal such as an
	// unsupported archive version is the only place it is explained. stdout stays
	// clean, so a script's output is unaffected.
	run := exec.CommandContext(ctx, inv.path, inv.argv[1:]...)
	run.Env = inv.env
	run.Stdout = io.Discard
	run.Stderr = stderr
	if rerr := run.Run(); rerr != nil {
		return fmt.Errorf("rafikid db restore: pg_restore: %w", rerr)
	}

	fmt.Fprintf(stderr, "rafikid db restore: restored %s (format %s)\n", path, formatDump)
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
