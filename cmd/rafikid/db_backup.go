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
	"time"

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
	var format string

	cmd := &cobra.Command{
		Use:   "backup <path>",
		Short: "Write a backup of rafikid's database to <path>",
		Long: `Writes one backup file, choosing the format by what this host can actually do:

  dump  pg_dump -Fc, a logical copy that works against any server the DSN names.
  txz   a physical copy of the embedded server's data directory (xz).
  tgz   the same copy, gzip-compressed.

--format picks one explicitly; without it the priority is dump > txz > tgz
resolved at run time: when pg_dump is on PATH the dump is preferred (it is the
portable one), otherwise an embedded install gets a physical copy. tgz is never
chosen automatically because xz is linked into rafikid and cannot be missing —
it is there for a consumer that only speaks gzip.

The dump is taken over the wire with the connection in the PG* environment,
never on pg_dump's command line, exactly as psql receives it. A pg_dump older
than the server's major version is refused: it cannot dump a newer server.

A physical copy needs a quiet data directory, so the embedded server is stopped
(-m fast, bounded) and started again in a defer — a backup that fails, or is
interrupted, still leaves the install up. A configured RAFIKI_DB names a server
on some other host, which has no data directory here, so the physical formats
are refused for it.

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
			return runBackup(cmd.Context(), db, args[0], format, os.Stderr)
		},
	}

	cmd.Flags().StringVar(&format, "format", "",
		"dump (pg_dump -Fc), txz or tgz (physical, embedded only); default: dump when pg_dump is on PATH, else txz")

	return cmd
}

// runBackup resolves the format, gives the path its suffix and dispatches.
func runBackup(ctx context.Context, configured, path, format string, stderr io.Writer) error {
	if configured == "" {
		return errors.New("--db (or RAFIKI_DB) is required")
	}
	if format == "" {
		resolved, err := resolveBackupFormat(configured)
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
	return runPhysicalBackup(ctx, configured, path, format, stderr)
}

// resolveBackupFormat is the format priority, resolved against what this host
// can do right now: dump > txz > tgz, where a step is skipped when its tool is
// not there. tgz is unreachable by priority — xz is linked into rafikid, so the
// physical copy it guards is never unavailable — and is chosen only explicitly.
func resolveBackupFormat(configured string) (string, error) {
	if _, err := lookPgDump(); err == nil {
		return formatDump, nil
	}
	if configured == embeddedDSN {
		return formatTXZ, nil
	}
	return "", fmt.Errorf("rafikid db backup: %s; a physical format (txz, tgz) copies the embedded "+
		"server's data directory, and RAFIKI_DB names a server this host does not manage — install "+
		"the client, or set RAFIKI_DB=embedded", pgDumpMissing)
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

// lookPgDump finds pg_dump, or reports the install remedy.
func lookPgDump() (string, error) {
	return lookPgClient("pg_dump", pgDumpMissing)
}

// runDumpBackup takes a logical backup with pg_dump -Fc.
func runDumpBackup(ctx context.Context, configured, path string, stderr io.Writer) error {
	// The client is looked up BEFORE the DSN is resolved, as psql does it:
	// resolving an embedded DSN downloads and initdbs, and an operator without
	// pg_dump should learn that in a millisecond rather than after that.
	pgDump, err := lookPgDump()
	if err != nil {
		return fmt.Errorf("rafikid db backup: %w", err)
	}
	dsn, err := resolveDSN(ctx, configured)
	if err != nil {
		return err
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
// here, and the only way to prove it is to make packing fail: the real set is
// pg_ctl (through Server.Ensure for start), the test set records the order.
type embeddedBackupOps struct {
	// ensure makes the server ready, starting it if it is not running. It runs
	// before the stop (so the data directory exists and the stop is a stop) and
	// again from the defer (so a failed backup never leaves the install down).
	ensure func(context.Context, embedpg.Server) error
	// stop shuts the server down: a quiet cluster is what makes a physical copy
	// consistent. It is bounded, and it finishes even if the backup was
	// interrupted — a half-stopped server is worse than a slow one.
	stop func(context.Context, embedpg.Server) error
	// pack writes one archive of dir in format.
	pack func(io.Writer, string, string) error
}

var backupEmbedded = embeddedBackupOps{
	ensure: func(ctx context.Context, s embedpg.Server) error {
		_, err := s.Ensure(ctx)
		return err
	},
	stop: stopEmbeddedServer,
	pack: writeDataDirArchive,
}

// runPhysicalBackup copies the embedded data directory, stopped.
func runPhysicalBackup(ctx context.Context, configured, path, format string, stderr io.Writer) (err error) {
	if configured != embeddedDSN {
		return fmt.Errorf("rafikid db backup: --format %s copies the embedded server's data "+
			"directory, and RAFIKI_DB names a server this host does not manage; use --format dump, "+
			"or set RAFIKI_DB=embedded", format)
	}
	s := newEmbeddedServer()

	if err := backupEmbedded.ensure(ctx, s); err != nil {
		return fmt.Errorf("rafikid db backup: embedded database: %w", err)
	}
	if serr := backupEmbedded.stop(ctx, s); serr != nil {
		return fmt.Errorf("rafikid db backup: %w", serr)
	}
	// From here on the server is DOWN, so the restart is unconditional: it runs
	// on a packing failure and on a cancelled context alike. The context is
	// detached for exactly that reason — Ctrl-C must not be able to leave the
	// install down.
	defer func() {
		if serr := backupEmbedded.ensure(context.WithoutCancel(ctx), s); serr != nil {
			err = errors.Join(err, fmt.Errorf("rafikid db backup: restarting the embedded database: %w", serr))
		}
	}()

	size, perr := writeBackupFile(path, func(w io.Writer, _ string) error {
		return backupEmbedded.pack(w, s.DataDir, format)
	})
	if perr != nil {
		return fmt.Errorf("rafikid db backup: %w", perr)
	}
	fmt.Fprintf(stderr, "rafikid db backup: wrote %s (format %s, %s)\n",
		path, format, humanize.Bytes(uint64(size)))
	return nil
}

// stopEmbeddedTimeout bounds pg_ctl stop: "fast" waits for clients to leave, and
// one stuck client must not keep the install down (or this command running)
// indefinitely.
const stopEmbeddedTimeout = 60 * time.Second

// stopEmbeddedServer runs pg_ctl stop -m fast against the private server.
func stopEmbeddedServer(ctx context.Context, s embedpg.Server) error {
	pgctl := filepath.Join(s.BinDir, "bin", "pg_ctl")
	// Detached from the backup's context for the reason the restart is: this is
	// the command that puts the server into the state the restart has to undo.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopEmbeddedTimeout)
	defer cancel()

	out, err := embedpgCommand(ctx, pgctl, "stop", "-m", "fast", "-w",
		"-t", strconv.Itoa(int(stopEmbeddedTimeout.Seconds())), "-D", s.DataDir)
	if err != nil {
		// pg_ctl reports "no server running" as an ordinary failure. There is
		// nothing to stop, which is the state the caller wanted.
		if strings.Contains(string(out), "no server running") {
			return nil
		}
		return fmt.Errorf("%s stop failed: %w: %s", pgctl, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// embedpgCommand runs one pg_ctl command with the same minimal environment
// pkg/embedpg gives its subprocesses. pg_ctl is short-lived, but it is handed
// the daemon's data directory, and nothing about a backup needs the daemon's
// secrets in the environment of the process doing it.
func embedpgCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	env := make([]string, 0, 4)
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "TZ"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	return cmd.CombinedOutput()
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
	// the callback is what flushes what the kernel still holds.
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("writing the backup: %w", errors.Join(werr, cerr))
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("moving the backup into place: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("sizing the backup: %w", err)
	}
	return info.Size(), nil
}

// writeDataDirArchive writes dir as a tar archive to w, compressed in format.
// Entry names are relative to dir with no wrapper directory, so a restore
// unpacks the tree straight into place.
func writeDataDirArchive(w io.Writer, dir, format string) error {
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
	walkErr := tarDataDir(tw, dir)
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
func tarDataDir(tw *tar.Writer, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
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
