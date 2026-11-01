// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/embedpg"
)

// --- fixtures ---------------------------------------------------------------

// tarFixtureEntry is one entry of a synthetic archive, in the contract `db
// backup` writes: a file, a directory, a symlink, or — for the refusals — an
// entry a data directory archive must never carry.
type tarFixtureEntry struct {
	name string
	body string
	link string // non-empty: a symlink
	dir  bool
	fifo bool
	mode fs.FileMode
}

// writeTarArchive writes entries to path as a tar, compressed in format: "" is a
// plain tar, formatTGZ and formatTXZ are what `db backup` actually produces.
func writeTarArchive(t *testing.T, path, format string, entries ...tarFixtureEntry) string {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o600
		}
		hdr := &tar.Header{Name: e.name, Mode: int64(mode), Format: tar.FormatUSTAR}
		switch {
		case e.link != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.link
		case e.dir:
			hdr.Typeflag = tar.TypeDir
		case e.fifo:
			hdr.Typeflag = tar.TypeFifo
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	raw := buf.Bytes()
	switch format {
	case "":
	case formatTGZ:
		var gz bytes.Buffer
		gw := gzip.NewWriter(&gz)
		if _, err := gw.Write(raw); err != nil {
			t.Fatalf("gzip fixture: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Fatalf("gzip fixture: %v", err)
		}
		raw = gz.Bytes()
	case formatTXZ:
		var xzbuf bytes.Buffer
		xw, err := xz.NewWriter(&xzbuf)
		if err != nil {
			t.Fatalf("xz fixture: %v", err)
		}
		if _, err := xw.Write(raw); err != nil {
			t.Fatalf("xz fixture: %v", err)
		}
		if err := xw.Close(); err != nil {
			t.Fatalf("xz fixture: %v", err)
		}
		raw = xzbuf.Bytes()
	default:
		t.Fatalf("unknown fixture format %q", format)
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
	return path
}

// writeBytes writes one file, for the fixtures that are not archives.
func writeBytes(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// dataDirArchiveEntries is a minimal archive that unpacks as a data directory
// `checkRestoredDataDir` accepts.
func dataDirArchiveEntries() []tarFixtureEntry {
	return []tarFixtureEntry{
		{name: "PG_VERSION", body: embedpg.MajorVersion + "\n"},
		{name: "postgresql.conf", body: "# restored\n"},
		{name: "base/1/rel", body: "restored-relation"},
		{name: "link", link: "PG_VERSION"},
	}
}

// writeDataDirTree writes a data-directory-shaped tree on disk, whose recall is
// the marker argument: a restore is told apart from what it replaced by it.
func writeDataDirTree(t *testing.T, dir, marker string) {
	t.Helper()
	for _, d := range []string{dir, filepath.Join(dir, "base"), filepath.Join(dir, "base", "1")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatalf("chmod %s: %v", d, err)
		}
	}
	writeBytes(t, filepath.Join(dir, "PG_VERSION"), embedpg.MajorVersion+"\n")
	writeBytes(t, filepath.Join(dir, "postgresql.conf"), "# "+marker+"\n")
	writeBytes(t, filepath.Join(dir, "marker"), marker)
	writeBytes(t, filepath.Join(dir, "base", "1", "rel"), marker+"-relation")
}

// snapshotTree describes every path under dir — what it is, its mode, and its
// body or link target — so a test can prove a directory was left EXACTLY as it
// was, not merely still present.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		info, ierr := d.Info() // Lstat semantics
		if ierr != nil {
			return ierr
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			snap[rel] = "link -> " + target
		case info.IsDir():
			snap[rel] = fmt.Sprintf("dir %o", info.Mode().Perm())
		default:
			body, berr := os.ReadFile(path)
			if berr != nil {
				return berr
			}
			snap[rel] = fmt.Sprintf("file %o %s", info.Mode().Perm(), body)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return snap
}

// assertNoRestoreSiblings proves a run left no staging tree and no moved-aside
// directory behind: a refusal that created either did work it then had to undo.
func assertNoRestoreSiblings(c *assert.C, dataDir string) {
	for _, pattern := range []string{".restoring-*", ".replaced-*"} {
		matches, err := filepath.Glob(dataDir + pattern)
		c.NoError(err, "glob %s", pattern)
		c.Eq(0, len(matches), "no restore may leave a %s sibling behind, got: %v", pattern, matches)
	}
}

// restoreSiblings lists the data dir's siblings matching a suffix pattern.
func restoreSiblings(t *testing.T, dataDir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(dataDir + pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	return matches
}

// pinRestoreStamp pins the timestamp one restore's sibling names carry, so a test
// can name the exact paths a refusal and a report mention.
func pinRestoreStamp(t *testing.T, unix int64) {
	t.Helper()
	orig := restoreStamp
	t.Cleanup(func() { restoreStamp = orig })
	restoreStamp = func() time.Time { return time.Unix(unix, 0) }
}

// --- the four operations a physical restore performs -------------------------

// fakeRestore stands in for pg_ctl and the archiver. It records the ORDER of the
// operations because that order is the safety property: nothing may be stopped or
// moved before the archive has been unpacked and checked.
type fakeRestore struct {
	ops []string
	// running is what status reports, and what a stop/start flips.
	running bool
	// stopErr and startErr are returned by stop and start.
	stopErr, startErr error
	// failFirstStart makes the first start fail and later ones succeed, which is
	// how the rollback's restart is told apart from the restore's own start.
	failFirstStart bool
	starts         int
	// unpack replaces the archiver when a test wants a real or failing one.
	unpack func(archive, dir string) error
}

// stubRestoreOps points the physical path at dataDir and swaps the four operations
// for recorders.
func stubRestoreOps(t *testing.T, dataDir string, running bool) *fakeRestore {
	t.Helper()
	f := &fakeRestore{running: running}

	origServer := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = origServer })
	newEmbeddedServer = func() embedpg.Server {
		return embedpg.Server{BinDir: "/fake/bin", DataDir: dataDir,
			LockPath: filepath.Join(filepath.Dir(dataDir), "pg.lock")}
	}

	orig := restoreEmbedded
	t.Cleanup(func() { restoreEmbedded = orig })
	restoreEmbedded = embeddedRestoreOps{
		status: func(context.Context, embedpg.Server) (bool, error) {
			f.ops = append(f.ops, "status")
			return f.running, nil
		},
		stop: func(context.Context, embedpg.Server) error {
			f.ops = append(f.ops, "stop")
			if f.stopErr != nil {
				return f.stopErr
			}
			f.running = false
			return nil
		},
		start: func(context.Context, embedpg.Server) error {
			f.ops = append(f.ops, "start")
			f.starts++
			if f.failFirstStart && f.starts == 1 {
				return errors.New("pg_ctl: the restored server did not start")
			}
			if f.startErr != nil {
				return f.startErr
			}
			f.running = true
			return nil
		},
		unpack: func(archive, dir string) error {
			f.ops = append(f.ops, "unpack")
			if f.unpack != nil {
				return f.unpack(archive, dir)
			}
			// The stub writes a tree checkRestoredDataDir accepts, from the same
			// fixture a real archive would carry.
			if err := os.MkdirAll(filepath.Join(dir, "base"), 0o700); err != nil {
				return err
			}
			writeBytes(t, filepath.Join(dir, "PG_VERSION"), embedpg.MajorVersion+"\n")
			writeBytes(t, filepath.Join(dir, "postgresql.conf"), "# restored\n")
			return os.WriteFile(filepath.Join(dir, "marker"), []byte("restored"), 0o600)
		},
	}
	return f
}

// --- sniffing ---------------------------------------------------------------

// The file is classified by what it IS. The extension never decides, which is why
// a dump renamed to .txz still restores as a dump rather than over a data
// directory, and why every refusal names the two shapes it accepts.
func TestRestoreSniff(t *testing.T) {
	dir := t.TempDir()
	entries := dataDirArchiveEntries()

	dumpBytes := "PGDMP\x01\x0e\x00a custom archive, magic first"
	plain := writeTarArchive(t, filepath.Join(dir, "plain.tar"), "", entries...)
	tgz := writeTarArchive(t, filepath.Join(dir, "plain.tgz"), formatTGZ, entries...)
	txz := writeTarArchive(t, filepath.Join(dir, "plain.txz"), formatTXZ, entries...)
	// The extension lies in both directions.
	namedTar := writeBytes(t, filepath.Join(dir, "actually-a-dump.txz"), dumpBytes)
	namedDump := writeBytes(t, filepath.Join(dir, "actually-a-tar.dump"),
		string(mustRead(t, plain)))

	cases := []struct {
		name string
		path string
		want restoreKind
	}{
		{name: "custom dump", path: writeBytes(t, filepath.Join(dir, "app.dump"), dumpBytes), want: restoreDump},
		{name: "plain tar", path: plain, want: restorePhysical},
		{name: "gzip tar", path: tgz, want: restorePhysical},
		{name: "xz tar", path: txz, want: restorePhysical},
		{name: "dump named .txz", path: namedTar, want: restoreDump},
		{name: "tar named .dump", path: namedDump, want: restorePhysical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			got, err := sniffRestoreFile(tc.path)
			c.NoError(err, "sniffRestoreFile(%s) returned error", tc.path)
			c.Eq(tc.want, got, "kind, got")
		})
	}

	// Refusals: each names both shapes it accepts, and none of them is a guess.
	refusals := []struct {
		name string
		path string
		want string
	}{
		{name: "not a backup at all", path: writeBytes(t, filepath.Join(dir, "notes.txt"),
			"these are my notes about the database\n"), want: "is not a rafikid backup"},
		{name: "empty file", path: writeBytes(t, filepath.Join(dir, "empty"), ""),
			want: "is not a rafikid backup"},
		{name: "gzip of something else", path: gzipOf(t, filepath.Join(dir, "text.gz"),
			"not a tar at all"), want: "not a tar archive"},
		{name: "an empty gzip stream", path: gzipOf(t, filepath.Join(dir, "empty.gz"), ""),
			want: "empty archive"},
		{name: "a truncated tar", path: writeBytes(t, filepath.Join(dir, "short.tar"),
			"not far enough in for a header"), want: "is not a rafikid backup"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			kind, err := sniffRestoreFile(tc.path)
			c.Error(err, "want a refusal for %s, got nil", tc.name)
			c.Eq(restoreUnknown, kind, "a refused sniff must not name a kind, got")
			c.ErrorContains(err, tc.want, "refusal, got: %v", err)
			c.ErrorContains(err, "pg_dump custom archive",
				"the refusal must name the dump shape, got: %v", err)
			c.ErrorContains(err, "physical archive", "the refusal must name the physical shape, got: %v", err)
		})
	}

	// A directory is not a backup either; opening it fails on read.
	t.Run("a directory", func(t *testing.T) {
		c := assert.NewAborting(t)
		_, err := sniffRestoreFile(t.TempDir())
		c.Error(err, "want a refusal for a directory, got nil")
	})
}

// gzipOf writes body gzip-compressed to path.
func gzipOf(t *testing.T, path, body string) string {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(body)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// mustRead is the fixture copier's one primitive.
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// The zero value of restoreKind is not a shape: a plan that was never sniffed
// must be refused, never defaulted to "restore a dump". Both real kinds are
// explicit and distinct.
func TestRestoreKindsAreExplicit(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(restoreUnknown, restoreKind(0), "the zero value must be the explicit unknown kind, got")
	c.NotEq(restoreUnknown, restoreDump, "restoreDump must not be the zero value")
	c.NotEq(restoreUnknown, restorePhysical, "restorePhysical must not be the zero value")
	c.NotEq(restoreDump, restorePhysical, "the two shapes must be distinguishable")
	c.Eq("an unrecognised backup", restoreUnknown.String(), "unknown kind's String, got")
}

// --- the --force gate -------------------------------------------------------

// Without --force nothing is touched, for either shape: no unpack, no status, no
// stop, no rename, no subprocess. The run names exactly what it would destroy and
// exits non-zero.
func TestRestoreRefusesWithoutForce(t *testing.T) {
	t.Run("physical", func(t *testing.T) {
		c := assert.NewAborting(t)
		parent := t.TempDir()
		dataDir := filepath.Join(parent, "pg")
		writeDataDirTree(t, dataDir, "original")
		before := snapshotTree(t, dataDir)

		archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
			dataDirArchiveEntries()...)
		f := stubRestoreOps(t, dataDir, true)
		var stderr bytes.Buffer

		err := runRestore(context.Background(), embeddedDSN, archive, false, &stderr)

		c.Error(err, "want a refusal without --force, got nil")
		c.ErrorContains(err, "without --force", "refusal, got: %v", err)
		c.ErrorContains(err, dataDir, "the refusal must name the data directory, got: %v", err)
		c.ErrorContains(err, ".replaced-", "the refusal must say what happens to it, got: %v", err)

		c.Eq(0, len(f.ops), "a refused restore must perform no operation, got: %v", f.ops)
		c.EqDeep(before, snapshotTree(t, dataDir), "the data directory must be untouched, got")
		assertNoRestoreSiblings(c, dataDir)
		c.Eq("", stderr.String(), "a refused restore must not report work, got stderr")
	})

	t.Run("dump", func(t *testing.T) {
		c := assert.NewAborting(t)
		argvFile, envFile := fakePgRestoreDir(t)
		dir := t.TempDir()
		archive := writeBytes(t, filepath.Join(dir, "app.dump"), "PGDMP\x01\x0e\x00archive")
		var stderr bytes.Buffer

		err := runRestore(context.Background(),
			"postgres://alice:s3cret@db.example:6543/app", archive, false, &stderr)

		c.Error(err, "want a refusal without --force, got nil")
		c.ErrorContains(err, "without --force", "refusal, got: %v", err)
		c.ErrorContains(err, `"app"`, "the refusal must name the database, got: %v", err)
		c.ErrorContains(err, "db.example:6543", "the refusal must name the host, got: %v", err)
		c.NotStrContains(err.Error(), "s3cret", "the refusal must not echo the DSN, got: %v", err)

		c.NoFileExists(argvFile, "a refused restore must not run pg_restore")
		c.NoFileExists(envFile, "a refused restore must not run pg_restore")
		c.Eq("", stderr.String(), "a refused restore must not report work, got stderr")
	})
}

// The gate also holds for an embedded dump target, which names the database and
// the directory that serves it — without starting that server.
func TestRestoreRefusesEmbeddedDumpWithoutForce(t *testing.T) {
	c := assert.NewAborting(t)
	origServer := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = origServer })
	dataDir := filepath.Join(t.TempDir(), "pg")
	newEmbeddedServer = func() embedpg.Server { return embedpg.Server{BinDir: "/fake/bin", DataDir: dataDir} }

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := runRestore(context.Background(), embeddedDSN, archive, false, &stderr)

	c.Error(err, "want a refusal without --force, got nil")
	c.ErrorContains(err, embedpg.Database, "the refusal must name the embedded database, got: %v", err)
	c.ErrorContains(err, dataDir, "the refusal must name the directory that serves it, got: %v", err)
	c.NoDirExists(dataDir, "the refusal must not create or start anything")
}

// --- physical restore -------------------------------------------------------

// A successful physical restore stops the running server, moves the existing
// directory aside — never deleting it — unpacks, and starts the server. The
// ordering matters as much as the outcome: nothing is stopped before the archive
// has been unpacked into the staging directory.
func TestRestorePhysicalMovesTheDataDirAside(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	original := snapshotTree(t, dataDir)

	pinRestoreStamp(t, 1_700_000_000)
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)
	var stderr bytes.Buffer

	err := runRestore(context.Background(), embeddedDSN, archive, true, &stderr)

	c.NoError(err, "runRestore returned error: %s", stderr.String())
	c.EqDeep([]string{"unpack", "status", "stop", "start"}, f.ops,
		"the archive must be unpacked before anything is stopped, got ops")

	// The live directory is the restored tree...
	c.Eq("restored", string(mustRead(t, filepath.Join(dataDir, "marker"))), "live marker, got")

	// ...and what it replaced is beside it, complete and untouched.
	replaced := restoreSiblings(t, dataDir, ".replaced-*")
	c.Eq(1, len(replaced), "exactly one replaced directory must be kept, got: %v", replaced)
	c.EqDeep(original, snapshotTree(t, replaced[0]), "the replaced directory must be byte-identical, got")
	c.StrContains(filepath.Base(replaced[0]), "1700000000", "the replaced name must carry the stamp, got: %s",
		replaced[0])

	// The report names both, so an operator can find yesterday's database.
	c.StrContains(stderr.String(), dataDir, "the report must name the live directory, got: %s", stderr.String())
	c.StrContains(stderr.String(), replaced[0], "the report must name the replaced directory, got: %s",
		stderr.String())
}

// A restore into an install that has no data directory yet creates one and owes
// no stop, no move and no replaced directory.
func TestRestorePhysicalIntoAMissingDataDir(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")

	archive := writeTarArchive(t, filepath.Join(parent, "app.tgz"), formatTGZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, false)
	var stderr bytes.Buffer

	err := runRestore(context.Background(), embeddedDSN, archive, true, &stderr)

	c.NoError(err, "runRestore returned error: %s", stderr.String())
	c.EqDeep([]string{"unpack", "start"}, f.ops,
		"a fresh install needs no status and no stop, got ops")
	c.Eq("restored", string(mustRead(t, filepath.Join(dataDir, "marker"))), "restored marker, got")
	assertNoRestoreSiblings(c, dataDir)
	c.NotStrContains(stderr.String(), ".replaced-", "nothing was replaced, got: %s", stderr.String())
}

// A physical archive is the embedded server's data directory. A configured DSN
// names a server on another host, which has no data directory here, so the refusal
// happens before anything is built, started, stopped or unpacked — and never
// echoes the DSN.
func TestRestorePhysicalRefusesConfiguredDSN(t *testing.T) {
	c := assert.NewAborting(t)
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		t.Fatal("an embedded server was built for a configured DSN")
		return embedpg.Server{}
	}
	origOps := restoreEmbedded
	t.Cleanup(func() { restoreEmbedded = origOps })
	restoreEmbedded = embeddedRestoreOps{
		status: func(context.Context, embedpg.Server) (bool, error) {
			t.Error("status was asked for a configured DSN")
			return false, nil
		},
		stop: func(context.Context, embedpg.Server) error {
			t.Error("stop was called for a configured DSN")
			return nil
		},
		start: func(context.Context, embedpg.Server) error {
			t.Error("start was called for a configured DSN")
			return nil
		},
		unpack: func(string, string) error {
			t.Error("unpack was called for a configured DSN")
			return nil
		},
	}

	dir := t.TempDir()
	archive := writeTarArchive(t, filepath.Join(dir, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	var stderr bytes.Buffer

	err := runRestore(context.Background(), "postgres://alice:s3cret@db.example:6543/app", archive, true, &stderr)

	c.Error(err, "want a refusal for a physical archive on a configured DSN, got nil")
	c.ErrorContains(err, "embedded", "the refusal must say why, got: %v", err)
	c.NotStrContains(err.Error(), "s3cret", "the refusal must not echo the DSN, got: %v", err)
	c.NotStrContains(err.Error(), "db.example", "the refusal must not echo the DSN, got: %v", err)
	c.Eq("", stderr.String(), "a refused restore must report nothing as done, got stderr")

	// Nothing was created beside the archive.
	c.EqDeep([]string{"app.txz"}, namesIn(t, dir), "a refused restore must leave the directory as it was, got")
}

// namesIn lists the entries of a directory.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// An existing <data dir>.replaced-<stamp> is never overwritten: a second restore in
// the same second is refused before anything happens, so no backup is lost.
func TestRestorePhysicalNeverOverwritesAReplacedDir(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	pinRestoreStamp(t, 1_700_000_000)

	earlier := filepath.Join(dataDir + ".replaced-1700000000")
	if err := os.Rename(dataDir, earlier); err != nil {
		t.Fatalf("stage the earlier replaced dir: %v", err)
	}
	writeDataDirTree(t, dataDir, "current")

	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)

	err := runRestore(context.Background(), embeddedDSN, archive, true, io.Discard)

	c.Error(err, "want a refusal when the replaced name is taken, got nil")
	c.ErrorContains(err, earlier, "the refusal must name the existing directory, got: %v", err)
	c.Eq(0, len(f.ops), "a refused restore must perform no operation, got: %v", f.ops)
	c.DirExists(earlier, "the earlier replaced directory must survive")
}

// --- refusing before anything is destroyed ----------------------------------

// An archive that could unpack outside the tree — or into it by a route the
// containment rules forbid — is refused, and the refusal leaves the live
// directory, its server and the parent untouched. This is the brief's "every
// refusal happens before any destructive step" applied to the BYTES: the unpack
// runs into a staging directory first, and nothing has been stopped when it fails.
//
// The unpacker is pkg/embedpg's ExtractTar, so these cases are the restore path
// over the shared guards; pkg/embedpg's TestExtractTarRefusesAnEscapingArchive
// pins the same shapes against the function directly, without a restore around
// them.
func TestRestorePhysicalRefusesAnEscapingArchive(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarFixtureEntry
		want    string
	}{
		{
			name:    "a parent-relative name",
			entries: []tarFixtureEntry{{name: "../evil", body: "x"}},
			want:    "escapes the extraction root",
		},
		{
			name:    "an absolute name",
			entries: []tarFixtureEntry{{name: "/etc/evil", body: "x"}},
			want:    "escapes the extraction root",
		},
		{
			name:    "a symlink to an absolute path",
			entries: []tarFixtureEntry{{name: "passwd", link: "/etc/passwd"}},
			want:    "points at the absolute path",
		},
		{
			name:    "a symlink that climbs out",
			entries: []tarFixtureEntry{{name: "up", link: "../../etc"}},
			want:    "escapes the extraction root",
		},
		{
			name: "a symlink a later entry walks through",
			entries: []tarFixtureEntry{
				{name: "sub", link: "."},
				{name: "sub/evil", body: "x"},
			},
			want: "symlink",
		},
		{
			name:    "a fifo",
			entries: []tarFixtureEntry{{name: ".s.PGSQL.5432", fifo: true}},
			want:    "a fifo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			parent := t.TempDir()
			dataDir := filepath.Join(parent, "pg")
			writeDataDirTree(t, dataDir, "original")
			before := snapshotTree(t, dataDir)

			archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ, tc.entries...)
			f := stubRestoreOps(t, dataDir, true)
			f.unpack = unpackDataDirArchive // the real unpacker

			err := runRestore(context.Background(), embeddedDSN, archive, true, io.Discard)

			c.Error(err, "want a refusal for %s, got nil", tc.name)
			c.ErrorContains(err, tc.want, "refusal, got: %v", err)

			// Nothing was stopped, moved or created: the install is where it was.
			c.EqDeep([]string{"unpack"}, f.ops, "the refusal must come from the unpack, got ops")
			c.EqDeep(before, snapshotTree(t, dataDir), "the data directory must be untouched, got")
			assertNoRestoreSiblings(c, dataDir)
			// And nothing escaped beside the data directory.
			c.EqDeep([]string{"app.txz", "pg"}, namesIn(t, parent), "nothing may be created beside the data dir, got")
		})
	}
}

// A backup from another major version is refused while it is still in the staging
// directory: there is no upgrade path, so replacing the live directory with it
// would take the install down for nothing.
func TestRestorePhysicalRefusesAMismatchedMajor(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	before := snapshotTree(t, dataDir)

	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		tarFixtureEntry{name: "PG_VERSION", body: "17\n"},
		tarFixtureEntry{name: "postgresql.conf", body: "# older\n"},
	)
	f := stubRestoreOps(t, dataDir, true)
	f.unpack = unpackDataDirArchive

	err := runRestore(context.Background(), embeddedDSN, archive, true, io.Discard)

	c.Error(err, "want a refusal for a mismatched major version, got nil")
	c.ErrorContains(err, "PostgreSQL 17", "the refusal must name the archive's version, got: %v", err)
	c.ErrorContains(err, embedpg.MajorVersion, "the refusal must name these binaries' version, got: %v", err)
	c.EqDeep([]string{"unpack"}, f.ops, "the refusal must precede the stop, got ops")
	c.EqDeep(before, snapshotTree(t, dataDir), "the data directory must be untouched, got")
	assertNoRestoreSiblings(c, dataDir)
}

// A tar that is not a data directory at all is refused for the same reason: the
// archive must carry PG_VERSION and postgresql.conf, or there is nothing to start.
func TestRestorePhysicalRefusesANonDataDirArchive(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	before := snapshotTree(t, dataDir)

	// `db backup`'s contract snapshots a directory; an archive that is only files
	// can still be unpacked, but it is not a cluster.
	archive := writeTarArchive(t, filepath.Join(parent, "app.tar"), "",
		tarFixtureEntry{name: "holiday.jpg", body: "not a database"})
	f := stubRestoreOps(t, dataDir, true)
	f.unpack = unpackDataDirArchive

	err := runRestore(context.Background(), embeddedDSN, archive, true, io.Discard)

	c.Error(err, "want a refusal for a non-data-directory archive, got nil")
	c.ErrorContains(err, "no PG_VERSION", "the refusal must say what is missing, got: %v", err)
	c.EqDeep([]string{"unpack"}, f.ops, "the refusal must precede the stop, got ops")
	c.EqDeep(before, snapshotTree(t, dataDir), "the data directory must be untouched, got")
	assertNoRestoreSiblings(c, dataDir)
}

// A failed start rolls the whole thing back: the previous directory is back in
// place and the server is running it again, so a decode this command cannot serve
// does not cost the operator their database.
func TestRestorePhysicalRollsBackWhenTheStartFails(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	original := snapshotTree(t, dataDir)

	pinRestoreStamp(t, 1_700_000_000)
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)
	f.failFirstStart = true

	err := runRestore(context.Background(), embeddedDSN, archive, true, io.Discard)

	c.Error(err, "want the start failure, got nil")
	c.ErrorContains(err, "did not start", "error, got: %v", err)

	// unpack, status, stop, start (fails), start (the rollback's restart).
	c.EqDeep([]string{"unpack", "status", "stop", "start", "start"}, f.ops, "ops, got")
	c.EqDeep(original, snapshotTree(t, dataDir), "the rollback must put the previous directory back, got")
	assertNoRestoreSiblings(c, dataDir)
	c.True(f.running, "the rollback must leave the server running what it was running")
}

// --- the dump path ----------------------------------------------------------

// fakePgRestoreDir puts a pg_restore on PATH that records its argv and the PG*
// variables it saw. That is how the two properties that matter — no DSN on argv,
// the connection in the environment — are checked against a real exec.
func fakePgRestoreDir(t *testing.T) (argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")
	envFile = filepath.Join(dir, "env.txt")

	script := `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_PGRESTORE_ARGV"
printf 'PGHOST=%s\nPGDATABASE=%s\nPGUSER=%s\nPGPASSWORD=%s\n' \
	"$PGHOST" "$PGDATABASE" "$PGUSER" "$PGPASSWORD" > "$FAKE_PGRESTORE_ENV"
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "pg_restore"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pg_restore: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FAKE_PGRESTORE_ARGV", argvFile)
	t.Setenv("FAKE_PGRESTORE_ENV", envFile)
	return argvFile, envFile
}

// The dump path against a real subprocess: the archive path is on argv (it is what
// the operator typed), the connection is not, and the flags are the ones the
// command documents — --single-transaction included, which is what makes a failed
// restore leave the database as it was.
func TestRestoreDumpPutsArchiveInArgvNotTheDSN(t *testing.T) {
	c := assert.NewAborting(t)
	argvFile, envFile := fakePgRestoreDir(t)

	dir := t.TempDir()
	archive := writeBytes(t, filepath.Join(dir, "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer

	err := runRestore(context.Background(),
		"postgres://alice:s3cret@db.example:6543/app", archive, true, &stderr)

	c.NoError(err, "runRestore returned error: %s", stderr.String())

	argv := string(mustRead(t, argvFile))
	for _, want := range []string{"-d", "app", "--clean", "--if-exists", "--no-owner",
		"--single-transaction", archive} {
		c.StrContains(argv, want, "pg_restore argv must carry %q, got: %s", want, argv)
	}
	for _, bad := range []string{"s3cret", "db.example", "alice"} {
		c.NotStrContains(argv, bad, "pg_restore argv must not carry %q, got: %s", bad, argv)
	}

	env := string(mustRead(t, envFile))
	for _, want := range []string{"PGHOST=db.example", "PGDATABASE=app", "PGUSER=alice", "PGPASSWORD=s3cret"} {
		c.StrContains(env, want, "pg_restore environment must carry %q, got: %s", want, env)
	}

	c.StrContains(stderr.String(), archive, "the report must name the archive, got: %s", stderr.String())
	c.StrContains(stderr.String(), "format "+formatDump, "the report must name the format, got: %s", stderr.String())
	c.NotStrContains(stderr.String(), "s3cret", "the report must not echo the DSN, got: %s", stderr.String())
}

// A restore with no pg_restore refuses in a millisecond, BEFORE the DSN is
// resolved: resolving an embedded one downloads and initdbs a server, and a
// missing client must not cost that. The check is exercised on the dump path
// itself, because the --force gate legitimately names an embedded target's data
// directory first (which builds paths and starts nothing).
func TestRestoreDumpChecksTheClientBeforeResolving(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("PATH", t.TempDir()) // no pg_restore anywhere
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		t.Fatal("the embedded server was resolved before pg_restore was looked up")
		return embedpg.Server{}
	}

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := restoreDumpArchive(context.Background(), embeddedDSN, archive, &stderr)

	c.Error(err, "want a refusal for a missing pg_restore, got nil")
	c.ErrorContains(err, "pg_restore not found on PATH", "refusal, got: %v", err)
	c.StrContains(err.Error(), "apt install postgresql-client", "the refusal must carry the remedy, got: %v", err)
}

// A DSN that names no database cannot be restored into: pg_restore will not infer
// one, and guessing (psql's fall-back to the user name) would restore into a
// database the operator did not name. The refusal names neither the DSN nor a guess.
func TestRestoreDumpRefusesADSNWithNoDatabase(t *testing.T) {
	c := assert.NewAborting(t)
	argvFile, _ := fakePgRestoreDir(t)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := runRestore(context.Background(), "postgres://alice:s3cret@db.example:6543", archive, true, &stderr)

	c.Error(err, "want a refusal for a DSN with no database, got nil")
	c.ErrorContains(err, "names no database", "refusal, got: %v", err)
	c.NotStrContains(err.Error(), "s3cret", "the refusal must not echo the DSN, got: %v", err)
	c.NoFileExists(argvFile, "a refused restore must not run pg_restore")
}

// A pg_restore that fails is reported with its own stderr passed through: an
// unsupported archive version is only explained there.
func TestRestoreDumpReportsAFailure(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'pg_restore: error: unsupported version (1.15) in file header' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_restore"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pg_restore: %v", err)
	}
	t.Setenv("PATH", dir)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := runRestore(context.Background(), "postgres://u@h/app", archive, true, &stderr)

	c.Error(err, "want the pg_restore failure, got nil")
	c.ErrorContains(err, "pg_restore", "error, got: %v", err)
	c.StrContains(stderr.String(), "unsupported version", "pg_restore's own diagnosis must reach the operator, got: %s",
		stderr.String())
}

// --- the unpacker, against the producer's output -----------------------------

// The unpacker is the inverse of `db backup`'s archiver: a real archive written by
// writeDataDirArchive unpacks into a tree that matches the original, modes, nested
// directories and symlinks included, for both compressions.
func TestRestoreUnpackRoundTrip(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pg")
	writeDataDirTree(t, source, "original")
	if err := os.Symlink("PG_VERSION", filepath.Join(source, "version-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	want := snapshotTree(t, source)

	for _, format := range []string{formatTXZ, formatTGZ} {
		t.Run(format, func(t *testing.T) {
			c := assert.NewAborting(t)
			dir := t.TempDir()
			archive := filepath.Join(dir, "app."+format)
			f, err := os.Create(archive)
			c.NoError(err, "create the archive")
			c.NoError(writeDataDirArchive(f, source, format), "writeDataDirArchive returned error")
			c.NoError(f.Close(), "close the archive")

			kind, serr := sniffRestoreFile(archive)
			c.NoError(serr, "the archive the backup wrote must sniff as a backup")
			c.Eq(restorePhysical, kind, "kind, got")

			restored := filepath.Join(dir, "restored")
			c.NoError(os.Mkdir(restored, 0o700), "mkdir the destination")
			c.NoError(unpackDataDirArchive(archive, restored), "unpackDataDirArchive returned error")
			c.EqDeep(want, snapshotTree(t, restored), "the unpacked tree, got")
		})
	}
}
