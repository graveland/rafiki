// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
	"github.com/ulikunitz/xz"

	"go.graveland.dev/rafiki/pkg/embedpg"
)

// The three backup formats. They are not interchangeable: `dump` is a logical
// copy taken by pg_dump through the wire, and `txz`/`tgz` are physical copies of
// the embedded server's data directory, which is why only the second pair needs
// the server stopped.
const (
	formatDump = "dump"
	formatTXZ  = "txz"
	formatTGZ  = "tgz"
)

// backupFormatExt is the suffix each format appends when the operator gave a
// bare path.
var backupFormatExt = map[string]string{
	formatDump: ".dump",
	formatTXZ:  ".txz",
	formatTGZ:  ".tgz",
}

// pgDumpMissing is the whole remedy an operator needs, in the error itself:
// pg_dump is a separate package from the server on every platform rafiki runs
// on.
const pgDumpMissing = "pg_dump not found on PATH; install the PostgreSQL client " + pgClientInstallHint

// newDbBackupCmd writes a backup of rafikid's database to one file.
func newDbBackupCmd() *cobra.Command {
	var (
		format string
		force  bool
	)

	cmd := &cobra.Command{
		Use:   "backup <path>",
		Short: "Write a backup of rafikid's database to <path>",
		Long: `Writes one backup file, in one of three formats:

  dump  pg_dump -Fc, a logical copy that works against any server the DSN names.
  txz   a physical copy of the embedded server's data directory (xz).
  tgz   the same copy, gzip-compressed.

--format picks one explicitly; without it the backup is the dump, the portable
one. For the embedded server it is taken with the pg_dump in the server's own
tree (nothing to install, and the server is started if it is down); for a
configured DSN it needs a pg_dump on PATH and is refused when there is none. The
physical formats are chosen only explicitly: a dump needs no downtime, and a
physical copy of a running server does. tgz is there for a consumer that only
speaks gzip.

The dump is taken over the wire with the connection in the PG* environment,
never on pg_dump's command line, exactly as psql receives it. A pg_dump older
than the server's major version is refused at run time: it cannot dump a newer
server.

A physical copy is of a quiet data directory, and a backup does not change
whether the install is up. A server that is NOT running is left alone and its
directory is copied as it stands. A server that is RUNNING has to be stopped
(-m fast, bounded) for the copy, which takes the database away from whatever is
using it — a live daemon and every turn in flight — for the whole copy plus its
compression, so that is a downtime an operator has to accept with --force; it is
started again afterwards, on a failure or an interrupt too. An interrupt cancels
the copy itself (the archive is abandoned and its partial file removed) and still
restarts the server, so the one thing Ctrl-C cannot do is leave the install
down; only a second signal, which kills this process outright, can. The stop,
the copy and the restart happen under the same lock the daemon takes, so no
other rafikid process can start the server halfway through the archive. A data
directory that does not exist is an error, never something this command creates,
and a backup path inside the data directory is refused (the archive would contain
itself). A configured RAFIKI_DB names a server on some other host, which has no
data directory here, so the physical formats are refused for it.

The path, format and size are printed to stderr, so a script's stdout stays
clean. The extension is appended when <path> has none.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := cmd.Flags().GetString("db")
			if err != nil {
				return err
			}
			return runBackup(cmd.Context(), db, args[0], format, force, os.Stderr)
		},
	}

	cmd.Flags().StringVar(&format, "format", "",
		"dump (pg_dump -Fc), txz or tgz (physical, embedded only); default: dump")
	cmd.Flags().BoolVar(&force, "force", false,
		"stop the embedded server for a physical copy even though it is running (its users lose the database for the whole copy)")

	return cmd
}

// runBackup resolves the format, gives the path its suffix and dispatches.
func runBackup(ctx context.Context, configured, path, format string, force bool, stderr io.Writer) error {
	if configured == "" {
		return errors.New("--db (or RAFIKI_DB) is required")
	}
	if format == "" {
		resolved, err := backupFormatPriority(ctx, configured)
		if err != nil {
			return err
		}
		format = resolved
	}
	switch format {
	case formatDump, formatTXZ, formatTGZ:
	default:
		return fmt.Errorf("rafikid db backup: unknown format %q (want dump, txz or tgz)", format)
	}
	path = withExtension(path, backupFormatExt[format])

	if format == formatDump {
		return runDumpBackup(ctx, configured, path, stderr)
	}
	return runPhysicalBackup(ctx, configured, path, format, force, stderr)
}

// errPgDumpMissing marks "there is no pg_dump at all", as opposed to "the one
// there cannot be used": only the first is silent when the priority falls
// through, because only the first is self-evident.
var errPgDumpMissing = errors.New(pgDumpMissing)

// backupFormatPriority resolves the default format: a logical dump whenever one
// can be taken, because a dump is portable and restores into any server of that
// major. The physical formats (txz, tgz) are chosen only explicitly.
//
// An embedded install can always dump: its own tree ships pg_dump at exactly the
// server's major. A configured DSN has no data directory to fall back to, so a
// pg_dump it cannot use is the answer, not a reason to copy files.
func backupFormatPriority(ctx context.Context, configured string) (string, error) {
	unusable := pgDumpUnusable(ctx, configured)
	if unusable == nil {
		return formatDump, nil
	}
	if errors.Is(unusable, errPgDumpMissing) {
		return "", fmt.Errorf("rafikid db backup: %w; a physical format (txz, tgz) copies the embedded "+
			"server's data directory, and %s names a server this host does not manage",
			unusable, embeddedSettingHint)
	}
	return "", fmt.Errorf("rafikid db backup: %w", unusable)
}

// pgDumpUnusable reports why a dump cannot be taken from here, or nil when it
// can. An embedded install always can: the pg_dump in its tree is built with the
// server, so there is no client to find and no major to compare.
func pgDumpUnusable(ctx context.Context, configured string) error {
	if isEmbedded(configured) {
		return nil
	}
	pgDump, err := lookPgClient("pg_dump", pgDumpMissing)
	if err != nil {
		return errPgDumpMissing
	}
	out, err := exec.CommandContext(ctx, pgDump, "--version").Output()
	if err != nil {
		return fmt.Errorf("pg_dump --version: %w", err)
	}
	// The server's own major needs a connection, and the run-time gate asks it
	// there; the priority only decides what this host can offer.
	if _, err := pgDumpMajor(string(out)); err != nil {
		return err
	}
	return nil
}

// withExtension gives a bare path the format's suffix. A path the operator
// already suffixed is left exactly as given, whatever the format: naming the
// file is their decision, not this command's.
func withExtension(path, ext string) string {
	base := filepath.Base(path)
	switch baseExt := filepath.Ext(base); baseExt {
	case "":
		return path + ext
	case ".":
		// "app." was on its way to a suffix; the dot is replaced rather than
		// doubled, so the operator's keystroke does not produce "app..dump".
		return strings.TrimSuffix(path, ".") + ext
	default:
		if baseExt == base { // a dotfile: ".backup" is a name, not a suffix
			return path + ext
		}
		return path
	}
}

// runDumpBackup takes a logical backup with pg_dump -Fc.
func runDumpBackup(ctx context.Context, configured, path string, stderr io.Writer) error {
	// A pg_dump a target needs from PATH is looked up BEFORE the DSN is resolved,
	// as psql does it: an operator without one should learn that in a
	// millisecond. An embedded target is never refused here — its own tree ships
	// pg_dump, and resolving the DSN is what extracts it.
	if err := precheckPgClient(configured, "pg_dump", pgDumpMissing); err != nil {
		return fmt.Errorf("rafikid db backup: %w", err)
	}
	dsn, err := resolveDSN(ctx, configured)
	if err != nil {
		return err
	}
	pgDump, err := pgClientPath(configured, "pg_dump", pgDumpMissing)
	if err != nil {
		return fmt.Errorf("rafikid db backup: %w", err)
	}
	if err := checkPgDumpVersion(ctx, pgDump, dsn); err != nil {
		return err
	}

	size, err := writeBackupFile(path, func(_ io.Writer, tmp string) error {
		// The connection rides PG*; argv holds only the archive path, which is
		// not a secret (it is what the operator typed).
		inv, err := pgToolInvocation("rafikid db backup", pgDump, "pg_dump", configured, dsn,
			[]string{"-Fc", "-f", tmp}, os.Environ())
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, inv.target)
		cmd := exec.CommandContext(ctx, inv.path, inv.argv[1:]...)
		cmd.Env = inv.env
		// pg_dump -f writes the archive to the file; anything it says goes to our
		// stderr, so the operator sees WHY a dump failed, and stdout stays clean.
		cmd.Stdout = io.Discard
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("pg_dump: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("rafikid db backup: %w", err)
	}
	fmt.Fprintf(stderr, "rafikid db backup: wrote %s (format %s, %s)\n",
		path, formatDump, humanize.Bytes(uint64(size)))
	return nil
}

// checkPgDumpVersion refuses a pg_dump that cannot dump this server.
func checkPgDumpVersion(ctx context.Context, pgDump, dsn string) error {
	out, err := exec.CommandContext(ctx, pgDump, "--version").Output()
	if err != nil {
		return fmt.Errorf("rafikid db backup: pg_dump --version: %w", err)
	}
	serverMajor, err := serverMajorVersion(ctx, dsn)
	if err != nil {
		return err
	}
	return comparePgDumpVersion(string(out), serverMajor)
}

// serverMajorVersion reports the major release of the server dsn reaches. It is
// a variable so the dump path can be tested without a database: the gate's
// subject is the comparison, not the connection.
var serverMajorVersion = func(ctx context.Context, dsn string) (int, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("rafikid db backup: connecting to the server: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	var version string
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("rafikid db backup: reading the server version: %w", err)
	}
	major, err := majorFromVersion(version)
	if err != nil {
		return 0, fmt.Errorf("rafikid db backup: server_version: %w", err)
	}
	return major, nil
}

// comparePgDumpVersion refuses a pg_dump whose major is older than the server's,
// naming both: pg_dump cannot dump a server newer than itself.
func comparePgDumpVersion(versionLine string, serverMajor int) error {
	toolMajor, err := pgDumpMajor(versionLine)
	if err != nil {
		return fmt.Errorf("rafikid db backup: %w", err)
	}
	if toolMajor < serverMajor {
		return fmt.Errorf("rafikid db backup: pg_dump is PostgreSQL %d, older than the server's "+
			"major version %d; pg_dump cannot dump a server newer than itself. Install a matching "+
			"client (macOS: brew install libpq && brew link --force libpq; Debian/Ubuntu: apt "+
			"install postgresql-client-%d), or use --format txz", toolMajor, serverMajor, serverMajor)
	}
	return nil
}

// pgDumpMajor reads the major version out of a `pg_dump --version` line: every
// build prints "pg_dump (PostgreSQL) <major>.<minor> [vendor suffix]".
func pgDumpMajor(versionLine string) (int, error) {
	fields := strings.Fields(versionLine)
	for i, f := range fields {
		if f != "(PostgreSQL)" || i+1 >= len(fields) {
			continue
		}
		if major, err := majorFromVersion(fields[i+1]); err == nil {
			return major, nil
		}
		break
	}
	return 0, fmt.Errorf("cannot read a version from pg_dump --version output %q", strings.TrimSpace(versionLine))
}

// majorFromVersion reads the leading number of a PostgreSQL version string:
// "18.3" is major 18, "9.6.24" is major 9.
func majorFromVersion(v string) (int, error) {
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("cannot read a major version from %q", v)
	}
	return strconv.Atoi(v[:end])
}

// embeddedBackupOps are the operations a physical backup performs on the private
// server. They are function fields because the restart is the safety property
// here, and the only way to prove it is to make packing fail or the context
// cancel: the real set is pg_ctl through pkg/embedpg, the test set records the
// order.
type embeddedBackupOps struct {
	// lock takes the flock every other state change on the server takes, and
	// returns its release. It is what keeps a concurrent resolveDSN — `rafikid
	// psql`, a second daemon, `migrate` — from running Ensure and starting the
	// postmaster in the middle of the archive.
	lock func(context.Context, embedpg.Server) (func(), error)
	// status reports whether the server is running, WITHOUT starting it. A backup
	// must not change whether the install is up, so it may not call Ensure (which
	// starts a server that was down, and initdbs a directory that has none).
	status func(context.Context, embedpg.Server) (bool, error)
	// stop shuts the server down: a quiet cluster is what makes a physical copy
	// consistent. The CALLER holds lock, so it is Server.StopLocked — Stop would
	// take it again and deadlock against ourselves.
	stop func(context.Context, embedpg.Server) error
	// start brings back the server stop shut down, and is called with the lock
	// released so it can take it. It is Server.Ensure, because a restart must end
	// up exactly where a normal start would — same readiness check, same
	// rafiki.conf — and it cannot create anything here: the server was running, so
	// the directory and its PG_VERSION exist.
	start func(context.Context, embedpg.Server) error
	// pack writes one archive of dir in format. It takes the context because a
	// physical copy runs with the server STOPPED: an interrupt has to be able to
	// abort the archive (and let runPhysicalBackup's defer restart the server)
	// rather than leaving an operator waiting on a multi-minute xz with no
	// feedback, which is what would push them to SIGKILL.
	pack func(context.Context, io.Writer, string, string) error
}

var backupEmbedded = embeddedBackupOps{
	lock:   func(ctx context.Context, s embedpg.Server) (func(), error) { return s.Lock(ctx) },
	status: func(ctx context.Context, s embedpg.Server) (bool, error) { return s.Status(ctx) },
	stop: func(ctx context.Context, s embedpg.Server) error {
		return s.StopLocked(context.WithoutCancel(ctx))
	},
	start: func(ctx context.Context, s embedpg.Server) error {
		_, err := s.Ensure(ctx)
		return err
	},
	pack: writeDataDirArchive,
}

// runPhysicalBackup copies the embedded data directory as it is: a server that
// is not running is left alone, and one that is running is stopped for the copy
// and started again afterwards — the latter only when the operator accepted the
// downtime with --force.
func runPhysicalBackup(ctx context.Context, configured, path, format string, force bool, stderr io.Writer) (err error) {
	if !isEmbedded(configured) {
		return fmt.Errorf("rafikid db backup: --format %s copies the embedded server's data "+
			"directory, and %s names a server this host does not manage; use --format dump, or "+
			"point %s at the rafikid-managed database", format, embeddedSettingHint, embeddedSettingHint)
	}
	s := embeddedServer()

	// The directory is copied, never created: there is nothing to back up in one
	// that does not exist, and creating it here would be a backup that quietly
	// produced an empty archive.
	info, err := os.Stat(s.DataDir)
	if err != nil {
		return fmt.Errorf("rafikid db backup: the embedded database has no data directory at %s "+
			"(a backup copies what is there; start the server once, or set RAFIKI_DB to a server "+
			"you provide): %w", s.DataDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("rafikid db backup: %s is not a directory", s.DataDir)
	}
	// The archive is written next to <path> as a .partial-* file and only then
	// renamed; under the data directory the archiver would walk into the growing
	// file it is writing, so that is refused before anything is opened.
	if insideDir(path, s.DataDir) {
		return fmt.Errorf("rafikid db backup: %s is inside the embedded data directory %s; write the "+
			"backup somewhere else", path, s.DataDir)
	}

	// The lock spans stop → copy → start. Without it a concurrent Ensure (any
	// other rafikid command that resolves this DSN) would see the server down and
	// start it while the archiver is reading its files.
	release, lerr := backupEmbedded.lock(ctx, s)
	if lerr != nil {
		return fmt.Errorf("rafikid db backup: %w", lerr)
	}
	restart := false
	// The restart is registered BEFORE the stop, and runs on every way out: a
	// pack failure, a cancelled context, and a stop that failed or timed out. The
	// pg_ctl case is the one that matters — pg_ctl can give up while the
	// postmaster still goes on to shut down, so "the stop failed" does not mean
	// "the server is up", and an install left down by a backup is the outcome this
	// whole sequence exists to prevent. Ensure is idempotent: a server that is
	// still running is a no-op.
	defer func() {
		release()
		if !restart {
			return
		}
		if serr := backupEmbedded.start(context.WithoutCancel(ctx), s); serr != nil {
			err = errors.Join(err, fmt.Errorf("rafikid db backup: restarting the embedded database: %w", serr))
		}
	}()

	running, serr := backupEmbedded.status(ctx, s)
	if serr != nil {
		return fmt.Errorf("rafikid db backup: %w", serr)
	}
	if running {
		if !force {
			return fmt.Errorf("rafikid db backup: the embedded database is running at %s, and a "+
				"physical copy stops it with pg_ctl -m fast for the whole copy and its compression: "+
				"whatever uses it — a live daemon and every turn in flight — loses the database for "+
				"that time. Re-run with --force to accept that downtime, or use --format dump, which "+
				"copies over the wire and never stops the server", s.DataDir)
		}
		// An interrupt that arrived before the stop must not be acted on AT ALL:
		// the server is still up, so the cheapest safe answer is to leave it up
		// rather than stop it and hand the operator a restart they did not ask
		// for.
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("rafikid db backup: %w; the embedded server was not stopped", cerr)
		}
		// Set before the stop: see the defer above.
		restart = true
		if serr := backupEmbedded.stop(ctx, s); serr != nil {
			return fmt.Errorf("rafikid db backup: %w", serr)
		}
	}

	size, perr := writeBackupFile(path, func(w io.Writer, _ string) error {
		return backupEmbedded.pack(ctx, w, s.DataDir, format)
	})
	if perr != nil {
		return fmt.Errorf("rafikid db backup: %w", perr)
	}
	fmt.Fprintf(stderr, "rafikid db backup: wrote %s (format %s, %s)\n",
		path, format, humanize.Bytes(uint64(size)))
	return nil
}

// insideDir reports whether path is dir itself or lies under it. The check is
// lexical — the data directory is not a symlink in any install this command
// serves — and its only job is to keep the archiver from walking into the file it
// is writing.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// writeBackupFile writes a backup atomically: the bytes go to a temporary
// sibling first and are renamed into place only when the write succeeded, so a
// failed backup never leaves a file that looks like a complete one. write is
// handed both the stream and the temporary path (pg_dump wants a path, the
// archiver wants a writer), and the returned size is the finished file's.
func writeBackupFile(path string, write func(w io.Writer, tmp string) error) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".partial-*")
	if err != nil {
		return 0, fmt.Errorf("creating the backup file: %w", err)
	}
	tmp := f.Name()
	// The backup is a copy of the whole database: owner-only, like the data it
	// came from. CreateTemp already gives 0600; naming it here keeps the reason
	// next to the code that relies on it.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, err
	}

	werr := write(f, tmp)
	// The archiver's writer and pg_dump's file are the same fd; closing it after
	// the callback is what flushes what the kernel still holds. Sync is what the
	// kernel does NOT do on close: without it a power loss just after "wrote"
	// can leave a truncated file that looks like a complete backup and is only
	// discovered when it is needed. It comes before the rename, so a failure
	// here leaves no file that pretends to be one.
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("writing the backup: %w", errors.Join(werr, serr, cerr))
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("moving the backup into place: %w", err)
	}
	// The rename is atomic but not durable on its own: flush the directory so
	// the name points at the bytes on disk, not only in the page cache.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return 0, fmt.Errorf("flushing the directory the backup was written to: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("sizing the backup: %w", err)
	}
	return info.Size(), nil
}

// writeDataDirArchive writes dir as a tar archive to w, compressed in format.
// Entry names are relative to dir with no wrapper directory, so a restore
// unpacks the tree straight into place. It stops between entries when ctx is
// cancelled, so an interrupted physical backup does not hold the server down for
// the whole copy.
func writeDataDirArchive(ctx context.Context, w io.Writer, dir, format string) error {
	var (
		cw  io.WriteCloser
		err error
	)
	switch format {
	case formatTXZ:
		cw, err = xz.NewWriter(w)
	case formatTGZ:
		cw = gzip.NewWriter(w)
	default:
		return fmt.Errorf("unknown archive format %q", format)
	}
	if err != nil {
		return err
	}

	tw := tar.NewWriter(cw)
	walkErr := tarDataDir(ctx, tw, dir)
	// Both closers always run: a truncated archive must still be a readable
	// archive, and the error reported is the first reason it went wrong.
	return errors.Join(walkErr, tw.Close(), cw.Close())
}

// tarDataDir adds every entry under dir to tw, keeping modes, modification
// times and symlinks as they are on disk.
//
// A socket or a fifo is REFUSED rather than skipped: a skipped entry would make
// an incomplete archive that restores as a corrupt database, and a stopped
// cluster has no business producing one.
//
// A symlink whose target is absolute or climbs with `..` is REFUSED for the same
// reason, one step further out: `db restore` unpacks through
// embedpg.ExtractTar, which refuses exactly those targets, so recording one here
// would produce a backup its own restore rejects. A backup nobody can restore is
// not a backup — the refusal names the entry rather than shipping the problem.
// Every link a real cluster's data directory holds points at a sibling or down
// into its own subtree; one that leaves the tree — a hand-moved pg_wal, a
// tablespace kept outside it — has to be re-homed, or the backup taken as a dump.
func tarDataDir(ctx context.Context, tw *tar.Writer, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Between entries, never inside one: a half-copied file would make an
		// archive that unpacks into a corrupt cluster, and the copy already
		// refuses a file that changed underneath it.
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // the archive's root is the directory itself
		}
		info, err := d.Info() // Lstat semantics: a symlink reports the link
		if err != nil {
			return err
		}

		mode := info.Mode()
		var link string
		switch {
		case mode.IsRegular(), mode.IsDir():
		case mode&os.ModeSymlink != 0:
			if link, err = os.Readlink(path); err != nil {
				return err
			}
			if err := checkArchivableSymlink(path, link); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not a regular file, directory or symlink", path)
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if mode.IsRegular() {
			return copyInto(tw, path, info.Size())
		}
		return nil
	})
}

// checkArchivableSymlink refuses a symlink whose target `db restore` would
// refuse: an absolute one, or one that climbs with `..`. The two commands share
// one extractor (embedpg.ExtractTar) and those are the targets it refuses, so
// recording one would produce an archive this install cannot restore — a backup
// that exists only to fail at the worst moment. The `..` test is the string test
// embedpg.hasParentComponent applies, and no more: the containment decision
// stays in one place.
func checkArchivableSymlink(path, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("%s is a symlink to the absolute path %q; db restore refuses an absolute "+
			"symlink target, so this archive could not be restored", path, target)
	}
	for _, part := range strings.Split(target, "/") {
		if part == ".." {
			return fmt.Errorf("%s is a symlink to %q, which climbs with \"..\"; db restore refuses a "+
				"symlink target that climbs, so this archive could not be restored", path, target)
		}
	}
	return nil
}

// copyInto copies exactly size bytes of the file at path into w. The size is
// pinned because the header already said so: a file that changed underneath the
// backup would otherwise be written truncated or long.
func copyInto(w io.Writer, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s changed while it was being archived (%d of %d bytes)", path, n, size)
	}
	return nil
}
