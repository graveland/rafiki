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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/ulikunitz/xz"

	"go.graveland.dev/rafiki/pkg/embedpg"
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
	var force bool

	cmd := &cobra.Command{
		Use:   "restore <path>",
		Short: "Restore rafikid's database from a backup file (destructive: needs --force)",
		Long: `Restores rafikid's database from a backup file, choosing what to do by what the
file is, sniffed from its content rather than its extension:

  a pg_dump custom archive ("PGDMP" magic)
      pg_restore -d <database> --clean --if-exists --no-owner --single-transaction
      over the wire. The connection is passed in the PG* environment and never on
      pg_restore's command line — argv carries the database NAME, which pg_restore
      requires and which the --force report already told you.

  a physical archive (a tar, or a gzip/xz stream of one)
      the embedded server's data directory is replaced: the running server is
      stopped, the directory on disk is moved to <dir>.replaced-<unix> — never
      deleted — and the archive is unpacked in its place, then the server is
      started again. A running rafikid is NOT stopped by this command: stopping
      the server drops its connections, so stop the daemon first.

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
directory at <dir>.replaced-<unix> for the operator to keep or delete.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := cmd.Flags().GetString("db")
			if err != nil {
				return err
			}
			return runRestore(cmd.Context(), db, args[0], force, os.Stderr)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false,
		"replace what the backup covers; without it nothing is changed and the target is only named")

	return cmd
}

// runRestore sniffs the file, refuses everything it can refuse, and only then
// dispatches. The order here is the safety property: the --force gate, the
// embedded-only rule and the target description all run before anything is
// started, stopped, renamed or dropped.
func runRestore(ctx context.Context, configured, path string, force bool, stderr io.Writer) error {
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
		return restoreDumpArchive(ctx, configured, path, stderr)
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
		return fmt.Sprintf("it would replace the embedded data directory %s (moved to %s.replaced-<unix>, "+
			"never deleted) and start the server on the unpacked copy", dir, dir), nil
	}

	if configured == embeddedDSN {
		dir := newEmbeddedServer().DataDir
		return fmt.Sprintf("it would run pg_restore -d %s --clean --single-transaction against the embedded "+
			"database %q served from %s, dropping and recreating every object in it",
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
		"%q on host %s, dropping and recreating every object in it", dbname, dbname, host), nil
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

// lookPgRestore finds pg_restore, or reports the install remedy.
func lookPgRestore() (string, error) {
	return lookPgClient("pg_restore", pgRestoreMissing)
}

// restoreDumpArchive restores a pg_dump custom archive over the wire.
func restoreDumpArchive(ctx context.Context, configured, path string, stderr io.Writer) error {
	// The client is looked up BEFORE the DSN is resolved, as psql and pg_dump do
	// it: resolving an embedded DSN downloads and initdbs, and an operator
	// without pg_restore should learn that in a millisecond rather than after
	// that.
	pgRestore, err := lookPgRestore()
	if err != nil {
		return fmt.Errorf("rafikid db restore: %w", err)
	}
	dsn, err := resolveDSN(ctx, configured)
	if err != nil {
		return err
	}
	// pg_restore needs the database named explicitly; the name rides argv, the
	// connection does not.
	database, err := restoreDatabaseName(configured)
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
	inv, err := pgToolInvocation("rafikid db restore", pgRestore, "pg_restore", configured, dsn,
		[]string{"-d", database, "--clean", "--if-exists", "--no-owner", "--single-transaction", path},
		os.Environ())
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, inv.target)

	cmd := exec.CommandContext(ctx, inv.path, inv.argv[1:]...)
	cmd.Env = inv.env
	// pg_restore's own stderr is passed straight through: a refusal such as an
	// unsupported archive version is the only place it is explained. stdout stays
	// clean, so a script's output is unaffected.
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rafikid db restore: pg_restore: %w", err)
	}

	fmt.Fprintf(stderr, "rafikid db restore: restored %s (format %s)\n", path, formatDump)
	return nil
}

// embeddedRestoreOps are the operations a physical restore performs on the
// private server. They are function fields for the same reason the backup's are:
// the rollback is the safety property here, and the only way to prove it is to
// make a step fail — the real set is pg_ctl and the archiver through pkg/embedpg,
// the test set records the order and writes a fixture tree.
type embeddedRestoreOps struct {
	// status reports whether the server is running, WITHOUT starting it. A
	// restore must know what it owes a restart, and Ensure is the wrong question:
	// it starts a server that was down.
	status func(context.Context, embedpg.Server) (bool, error)
	// stop shuts the server down before its directory is moved. It runs detached
	// from the restore's context and is bounded by embedpg.StopTimeout, because a
	// half-stopped server is worse than a slow one.
	stop func(context.Context, embedpg.Server) error
	// start brings the server back up: on the restored directory after a success,
	// on the untouched or replaced-back one after a failure. It is Server.Ensure,
	// because a restart must end up exactly where a normal start would — same
	// readiness check, same rafiki.conf written for THIS install's paths (an
	// archive restored from another install names that install's socket dir), and
	// the 0700 mode re-asserted on a directory this command created.
	start func(context.Context, embedpg.Server) error
	// unpack unpacks the physical archive at its first argument into the
	// directory at its second.
	unpack func(archive, dir string) error
}

// restoreStamp is when one restore began. The sibling names it builds carry it,
// so two restores of the same install a second apart cannot collide; a variable
// so a test can pin the names a refusal and a report mention.
var restoreStamp = time.Now

var restoreEmbedded = embeddedRestoreOps{
	status: func(ctx context.Context, s embedpg.Server) (bool, error) { return s.Status(ctx) },
	stop:   func(ctx context.Context, s embedpg.Server) error { return s.Stop(context.WithoutCancel(ctx)) },
	start: func(ctx context.Context, s embedpg.Server) error {
		_, err := s.Ensure(context.WithoutCancel(ctx))
		return err
	},
	unpack: unpackDataDirArchive,
}

// restorePhysicalArchive replaces the embedded server's data directory with the
// archive at path.
//
// The order is chosen so that every refusal happens before any destructive step,
// which for the bytes means: unpack and check a STAGING directory first, and only
// then stop the server and move the live directory aside. A corrupt, truncated,
// escaping or wrong-major-version archive therefore leaves the install exactly as
// it was, still running. The two renames at the end are on one filesystem, so the
// window in which no data directory exists is two rename(2) calls long.
func restorePhysicalArchive(ctx context.Context, configured, path string, stderr io.Writer) (err error) {
	if configured != embeddedDSN {
		return fmt.Errorf("rafikid db restore: %s is a physical archive, which only RAFIKI_DB=embedded "+
			"can restore", path)
	}
	s := newEmbeddedServer()
	stamp := restoreStamp().Unix()
	staging := fmt.Sprintf("%s.restoring-%d", s.DataDir, stamp)
	replaced := fmt.Sprintf("%s.replaced-%d", s.DataDir, stamp)

	// Refusals that need nothing done: a leftover from an earlier restore, and a
	// data directory that exists but is not one.
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

	stopped := false
	movedAside := false
	// The defer IS the rollback, and it runs however the body ended — a returned
	// error, a cancelled context, a panic in a helper. What it removes is only
	// ever this command's own work: the staging tree and the tree just unpacked.
	// The directory that was there before is renamed back into place, never
	// deleted, and the server is left up if it was up.
	defer func() {
		if err == nil {
			return
		}
		_ = os.RemoveAll(staging)
		if movedAside {
			_ = os.RemoveAll(s.DataDir)
			if rerr := os.Rename(replaced, s.DataDir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("rafikid db restore: the previous data directory is at %s: %w",
					replaced, rerr))
				return
			}
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
		}

		// The one move that takes the live directory out of service. It is a
		// rename, never a delete: <dir>.replaced-<unix> is where yesterday's
		// database stays until the operator deletes it.
		if merr := os.Rename(s.DataDir, replaced); merr != nil {
			return fmt.Errorf("rafikid db restore: moving %s aside: %w", s.DataDir, merr)
		}
		movedAside = true
	}

	if rerr := os.Rename(staging, s.DataDir); rerr != nil {
		return fmt.Errorf("rafikid db restore: moving the restored directory into place: %w", rerr)
	}

	if serr := restoreEmbedded.start(ctx, s); serr != nil {
		return fmt.Errorf("rafikid db restore: starting the restored database: %w", serr)
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
func unpackDataDirArchive(path, dir string) error {
	r, err := openDataDirArchive(path)
	if err != nil {
		return err
	}
	defer r.Close()
	return extractDataDirTar(tar.NewReader(r), dir)
}

// errEscapesDataDir is what every path check in the unpacker reports. A restore
// archive is untrusted input: it may have been written by a different install,
// copied by hand, or not written by rafikid at all.
var errEscapesDataDir = errors.New("escapes the restored data directory")

// pendingRestoreLink is a symlink entry held back until every directory and file
// has been written.
type pendingRestoreLink struct {
	name string
	dest string
	link string
}

// extractDataDirTar writes every entry of tr into root, the contract `db backup`
// writes: names relative to the data directory with no wrapper directory, and
// regular files, directories and symlinks only.
//
// The containment rules are the ones pkg/embedpg's binary extractor applies to a
// downloaded archive (extractTar, fetch.go) — kept in step with it deliberately,
// because the reason is the same: the archive is untrusted, and a write that
// follows a link lands outside the tree. An absolute name or a `..` component is
// refused lexically; mkdirRestoreParents refuses to walk through a symlink;
// writeRestoreEntry refuses a symlink as its final component with O_NOFOLLOW; a
// symlink is refused when it is absolute or climbs with `..`; and links are
// created only after the last file, so while a file is being written there is no
// link anywhere under root for the kernel to follow.
//
// A socket, fifo or device entry is REFUSED rather than skipped: `db backup`
// refuses to archive one (a skipped entry restores as a corrupt database), so an
// archive carrying one was not written by this rafikid.
func extractDataDirTar(tr *tar.Reader, root string) error {
	var links []pendingRestoreLink
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}

		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive entry %q %w", hdr.Name, errEscapesDataDir)
		}
		if name == "." {
			continue // the archive's own root is the directory itself
		}
		dest := filepath.Join(root, name)
		mode := os.FileMode(hdr.Mode) & os.ModePerm

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirRestoreParents(root, dest); err != nil {
				return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
			}
			if err := os.Chmod(dest, dirMode(mode)); err != nil {
				return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := mkdirRestoreParents(root, filepath.Dir(dest)); err != nil {
				return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
			}
			if err := writeRestoreEntry(dest, fileMode(mode), tr); err != nil {
				return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("archive symlink %q points at the absolute path %q", hdr.Name, hdr.Linkname)
			}
			if hasParentRestoreComponent(hdr.Linkname) {
				return fmt.Errorf("archive symlink %q %w: %q", hdr.Name, errEscapesDataDir, hdr.Linkname)
			}
			links = append(links, pendingRestoreLink{name: name, dest: dest, link: hdr.Linkname})
		default:
			return fmt.Errorf("archive entry %q is %s; a data directory archive holds regular files, "+
				"directories and symlinks only", hdr.Name, tarEntryKind(hdr.Typeflag))
		}
	}

	for _, l := range links {
		if err := mkdirRestoreParents(root, filepath.Dir(l.dest)); err != nil {
			return fmt.Errorf("archive symlink %q: %w", l.name, err)
		}
		if _, err := os.Lstat(l.dest); err == nil {
			return fmt.Errorf("archive symlink %q collides with an entry already extracted", l.name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("archive symlink %q: %w", l.name, err)
		}
		if err := os.Symlink(l.link, l.dest); err != nil {
			return fmt.Errorf("archive symlink %q: %w", l.name, err)
		}
	}
	return nil
}

// dirMode narrows an archive's directory mode to the data directory's own: a
// recording that lost the mode must not widen access inside a tree that is the
// server's access control.
func dirMode(mode os.FileMode) os.FileMode {
	if mode == 0 {
		return 0o700
	}
	return mode
}

// fileMode is dirMode for a regular file.
func fileMode(mode os.FileMode) os.FileMode {
	if mode == 0 {
		return 0o600
	}
	return mode
}

// tarEntryKind names an archive entry type the way a refusal should read.
func tarEntryKind(t byte) string {
	switch t {
	case tar.TypeFifo:
		return "a fifo"
	case tar.TypeChar:
		return "a character device"
	case tar.TypeBlock:
		return "a block device"
	case tar.TypeXHeader, tar.TypeXGlobalHeader:
		return "a pax header"
	case tar.TypeGNULongName, tar.TypeGNULongLink:
		return "a GNU long-name entry"
	}
	return fmt.Sprintf("an entry of unsupported type %q", string(rune(t)))
}

// mkdirRestoreParents creates every component of dir under root, refusing to walk
// through anything that is not a real directory. A component that already exists
// as a symlink is an error, never something to follow.
func mkdirRestoreParents(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s", errEscapesDataDir, dir)
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := os.Mkdir(cur, 0o700); err != nil {
				return err
			}
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", errEscapesDataDir, cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}

// hasParentRestoreComponent reports whether a relative link target climbs with
// `..`.
func hasParentRestoreComponent(link string) bool {
	for _, part := range strings.Split(link, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// writeRestoreEntry creates dest with mode and copies the entry's body into it.
// The final component must not be a symlink: O_NOFOLLOW makes the kernel refuse
// one even if it appeared between the check and the open.
func writeRestoreEntry(dest string, mode os.FileMode, r io.Reader) error {
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", errEscapesDataDir, dest)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
