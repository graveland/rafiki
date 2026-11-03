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
	"strings"
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
	// failSecondStop makes the rollback's stop fail (the second one), which is the
	// state where a postmaster is still on the restored tree and the rollback must
	// not delete the files under it.
	failSecondStop bool
	stops          int
	// unpack replaces the archiver when a test wants a real or failing one.
	unpack func(archive, dir string) error
	// probe, when set, runs inside stop and start: the only hook in the middle of
	// the sequence, and where a test asks what must be true at that moment (the
	// lock held, the marker written).
	probe   func(stage string, s embedpg.Server)
	syncErr error
}

// stubRestoreOps points the physical path at dataDir and swaps the operations
// that touch the server for recorders. The LOCK is the real one and stays real:
// it costs nothing, and it is what lets a test ask whether it is held at a point
// in the middle of the swap.
func stubRestoreOps(t *testing.T, dataDir string, running bool) *fakeRestore {
	t.Helper()
	f := &fakeRestore{running: running}
	// A lock outside the data directory's parent, so it never shows up in a test
	// that lists what the restore created beside the data directory.
	lockDir := t.TempDir()

	origServer := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = origServer })
	newEmbeddedServer = func() embedpg.Server {
		return embedpg.Server{BinDir: "/fake/bin", DataDir: dataDir,
			LockPath: filepath.Join(lockDir, "pg.lock")}
	}

	orig := restoreEmbedded
	t.Cleanup(func() { restoreEmbedded = orig })
	restoreEmbedded = embeddedRestoreOps{
		lock: func(ctx context.Context, s embedpg.Server) (func(), error) { return s.Lock(ctx) },
		status: func(context.Context, embedpg.Server) (bool, error) {
			f.ops = append(f.ops, "status")
			return f.running, nil
		},
		stop: func(_ context.Context, s embedpg.Server) error {
			f.ops = append(f.ops, "stop")
			f.stops++
			if f.probe != nil {
				f.probe("stop", s)
			}
			if f.stopErr != nil {
				return f.stopErr
			}
			if f.failSecondStop && f.stops == 2 {
				return errors.New("pg_ctl: server does not shut down")
			}
			f.running = false
			return nil
		},
		start: func(_ context.Context, s embedpg.Server) error {
			f.ops = append(f.ops, "start")
			if f.probe != nil {
				f.probe("start", s)
			}
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
		sync: func(context.Context, string) error {
			// Not recorded: the tree-flush is asserted by its own test, and
			// every other test's expected op sequence predates it.
			return f.syncErr
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

		err := runRestore(context.Background(), embeddedDSN, archive, false, false, &stderr)

		c.Error(err, "want a refusal without --force, got nil")
		c.ErrorContains(err, "without --force", "refusal, got: %v", err)
		c.ErrorContains(err, dataDir, "the refusal must name the data directory, got: %v", err)
		c.ErrorContains(err, ".replaced-", "the refusal must say what happens to it, got: %v", err)
		c.ErrorContains(err, "stop the embedded server if it is running",
			"the refusal must say the server is stopped if it is up, got: %v", err)

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
			"postgres://alice:s3cret@db.example:6543/app", archive, false, false, &stderr)

		c.Error(err, "want a refusal without --force, got nil")
		c.ErrorContains(err, "without --force", "refusal, got: %v", err)
		c.ErrorContains(err, `"app"`, "the refusal must name the database, got: %v", err)
		c.ErrorContains(err, "db.example:6543", "the refusal must name the host, got: %v", err)
		c.NotStrContains(err.Error(), "s3cret", "the refusal must not echo the DSN, got: %v", err)
		// --clean --if-exists drops what the DUMP contains, not the whole
		// database: saying otherwise would overstate what this destroys.
		c.ErrorContains(err, "every object the ARCHIVE contains",
			"the refusal must bound what is dropped to the archive, got: %v", err)
		c.ErrorContains(err, "left alone", "the refusal must say the rest survives, got: %v", err)
		c.NotStrContains(err.Error(), "every object in it",
			"the refusal must not claim the whole database is replaced, got: %v", err)

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
	err := runRestore(context.Background(), embeddedDSN, archive, false, false, &stderr)

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

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, &stderr)

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

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, &stderr)

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

	err := runRestore(context.Background(), "postgres://alice:s3cret@db.example:6543/app", archive, true, false, &stderr)

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

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

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

			err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

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

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

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

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

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
	rollbackStoppedOnRestoredTree := false
	f := stubRestoreOps(t, dataDir, true)
	f.failFirstStart = true
	// The rollback must stop the postmaster the failed start may have left BEFORE
	// it removes the tree that postmaster is using. The restored tree's marker
	// says "restored"; the original one says "original".
	f.probe = func(stage string, _ embedpg.Server) {
		if stage != "stop" {
			return
		}
		if body, rerr := os.ReadFile(filepath.Join(dataDir, "marker")); rerr == nil && string(body) == "restored" {
			rollbackStoppedOnRestoredTree = true
		}
	}

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

	c.Error(err, "want the start failure, got nil")
	c.ErrorContains(err, "did not start", "error, got: %v", err)

	// unpack, status, stop, start (fails), then the rollback: stop (the postmaster
	// the failed start may have left), remove the new tree, put the old one back,
	// start (the restart the restore owed).
	c.EqDeep([]string{"unpack", "status", "stop", "start", "stop", "start"}, f.ops, "ops, got")
	c.EqDeep(original, snapshotTree(t, dataDir), "the rollback must put the previous directory back, got")
	assertNoRestoreSiblings(c, dataDir)
	c.True(rollbackStoppedOnRestoredTree,
		"the rollback must stop the server the failed start may have left BEFORE removing its directory")
	c.True(f.running, "the rollback must leave the server running what it was running")
	c.NoFileExists(dataDir+embedpg.RestoreInProgressSuffix, "a rolled-back restore must remove its marker")
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

// fakeBlockingPgRestore puts a pg_restore on PATH that says it has started and
// then blocks, so a test can cancel the context WHILE the restore is in flight —
// the case exec.CommandContext exists for, and the one a fake that exits at once
// cannot express. The marker is the proof it really started.
func fakeBlockingPgRestore(t *testing.T) (started string) {
	t.Helper()
	dir := t.TempDir()
	started = filepath.Join(dir, "started")
	// `exec` replaces the shell with sleep, so the kill lands on the process that
	// would otherwise keep running rather than on its parent.
	script := `#!/bin/sh
: > "$FAKE_PGRESTORE_STARTED"
exec sleep 30
`
	if err := os.WriteFile(filepath.Join(dir, "pg_restore"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pg_restore: %v", err)
	}
	// The real PATH stays behind the fake's directory: the script needs `sleep`,
	// and pg_restore is still found here first.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PGRESTORE_STARTED", started)
	return started
}

// waitForPath blocks until path exists, or fails the test at the deadline.
func waitForPath(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %s", path, within)
}

// The dump path against a real subprocess: the archive path is on argv (it is what
// the operator typed), the connection is not, and the flags are the ones the
// command documents — --single-transaction included, which is what makes a failed
// restore leave the database as it was.
func TestRestoreDumpPutsArchiveInArgvNotTheDSN(t *testing.T) {
	c := assert.NewAborting(t)
	argvFile, envFile := fakePgRestoreDir(t)
	stubRestoreGates(t)

	dir := t.TempDir()
	archive := writeBytes(t, filepath.Join(dir, "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer

	err := runRestore(context.Background(),
		"postgres://alice:s3cret@db.example:6543/app", archive, true, false, &stderr)

	c.NoError(err, "runRestore returned error: %s", stderr.String())

	argv := string(mustRead(t, argvFile))
	for _, want := range []string{"-d", "app", "--clean", "--if-exists", "--no-owner",
		"--single-transaction", archive} {
		c.StrContains(argv, want, "pg_restore argv must carry %q, got: %s", want, argv)
	}
	for _, bad := range []string{"s3cret", "db.example", "alice"} {
		c.NotStrContains(argv, bad, "pg_restore argv must not carry %q, got: %s", bad, argv)
	}
	// The archive path comes last, behind `--`: a filename is not a flag.
	args := strings.Split(strings.TrimRight(argv, "\n"), "\n")
	c.True(len(args) >= 2, "the fake pg_restore recorded %d arguments, got: %v", len(args), args)
	c.Eq(archive, args[len(args)-1], "the archive path must be the last argument, got: %v", args)
	c.Eq("--", args[len(args)-2], "the path must follow `--`, got: %v", args)

	env := string(mustRead(t, envFile))
	for _, want := range []string{"PGHOST=db.example", "PGDATABASE=app", "PGUSER=alice", "PGPASSWORD=s3cret"} {
		c.StrContains(env, want, "pg_restore environment must carry %q, got: %s", want, env)
	}

	c.StrContains(stderr.String(), archive, "the report must name the archive, got: %s", stderr.String())
	c.StrContains(stderr.String(), "format "+formatDump, "the report must name the format, got: %s", stderr.String())
	c.NotStrContains(stderr.String(), "s3cret", "the report must not echo the DSN, got: %s", stderr.String())
}

// A restore of a REMOTE database with no pg_restore on PATH refuses in a
// millisecond, before anything is resolved. The check is exercised on the dump
// path itself, because the --force gate legitimately names an embedded target's
// data directory first (which builds paths and starts nothing).
func TestRestoreDumpChecksTheClientBeforeConnecting(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("PATH", t.TempDir()) // no pg_restore anywhere
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		t.Fatal("the embedded server was consulted for a remote database")
		return embedpg.Server{}
	}

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := restoreDumpArchive(context.Background(), "postgres://u@h/app", archive, false, &stderr)

	c.Error(err, "want a refusal for a missing pg_restore, got nil")
	c.ErrorContains(err, "pg_restore not found on PATH", "refusal, got: %v", err)
	c.StrContains(err.Error(), "apt install postgresql-client", "the refusal must carry the remedy, got: %v", err)
}

// The embedded server's own tree ships pg_restore, so an embedded restore with
// nothing on PATH is NOT refused up front: it goes on to resolve the DSN, which
// is what extracts the tree.
func TestRestoreDumpEmbeddedNeedsNoClientOnPath(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("PATH", t.TempDir())
	resolved := false
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		resolved = true
		return embedpg.Server{} // not a usable server: resolving it fails, which is enough
	}

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := restoreDumpArchive(context.Background(), embeddedDSN, archive, false, &stderr)

	c.True(resolved, "the embedded DSN was never resolved")
	c.Error(err, "an unusable embedded server must fail to resolve")
	c.False(strings.Contains(err.Error(), "pg_restore not found"),
		"a missing PATH pg_restore must not be reported for an embedded target: %v", err)
}

// A DSN that names no database cannot be restored into: pg_restore will not infer
// one, and guessing (psql's fall-back to the user name) would restore into a
// database the operator did not name. The refusal names neither the DSN nor a guess.
func TestRestoreDumpRefusesADSNWithNoDatabase(t *testing.T) {
	c := assert.NewAborting(t)
	argvFile, _ := fakePgRestoreDir(t)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := runRestore(context.Background(), "postgres://alice:s3cret@db.example:6543", archive, true, false, &stderr)

	c.Error(err, "want a refusal for a DSN with no database, got nil")
	c.ErrorContains(err, "names no database", "refusal, got: %v", err)
	c.NotStrContains(err.Error(), "s3cret", "the refusal must not echo the DSN, got: %v", err)
	c.NoFileExists(argvFile, "a refused restore must not run pg_restore")
}

// A pg_restore that fails is reported with its own stderr passed through: an
// unsupported archive version is only explained there.
func TestRestoreDumpReportsAFailure(t *testing.T) {
	c := assert.NewAborting(t)
	stubRestoreGates(t)
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'pg_restore: error: unsupported version (1.15) in file header' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_restore"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pg_restore: %v", err)
	}
	t.Setenv("PATH", dir)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := runRestore(context.Background(), "postgres://u@h/app", archive, true, false, &stderr)

	c.Error(err, "want the pg_restore failure, got nil")
	c.ErrorContains(err, "pg_restore", "error, got: %v", err)
	c.StrContains(stderr.String(), "unsupported version", "pg_restore's own diagnosis must reach the operator, got: %s",
		stderr.String())
}

// A context cancelled while pg_restore is RUNNING stops it, and the run reports
// the cancellation instead of a restore. The command's own help text promises
// this ("a dump restore stops pg_restore, whose single transaction rolls back"),
// and nothing else pins it.
//
// What this proves: pg_restore is really started, the cancellation reaches the
// child — runRestore returns in the moment after the cancel, not the 30 seconds
// the fake would otherwise run for — the error names the cancellation rather
// than an archive problem, and no success line is printed. What it does NOT
// prove: the server-side rollback, which is `--single-transaction` on argv
// (TestRestoreDumpPutsArchiveInArgvNotTheDSN) and the server's own behaviour.
func TestRestoreDumpStopsPgRestoreWhenTheContextIsCancelled(t *testing.T) {
	c := assert.NewAborting(t)
	started := fakeBlockingPgRestore(t)
	stubRestoreGates(t)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runRestore(ctx, "postgres://u@h/app", archive, true, false, &stderr) }()

	// Cancelled only once pg_restore is known to be RUNNING: a context cancelled
	// beforehand would prove the weaker property that it never starts at all.
	waitForPath(t, started, 10*time.Second)
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runRestore did not return after the context was cancelled: pg_restore was not stopped")
	}

	c.Error(err, "a cancelled restore must report the cancellation, got nil")
	c.True(errors.Is(err, context.Canceled), "the error must be the context's own, got: %v", err)
	c.StrContains(err.Error(), "pg_restore", "the error must name what was stopped, got: %v", err)
	c.NotStrContains(stderr.String(), "restored ", "a cancelled restore must not report success, got: %s",
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
			c.NoError(writeDataDirArchive(context.Background(), f, source, format), "writeDataDirArchive returned error")
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

// --- the lock, the marker, and the ways a physical restore can fail -----------

// The embedpg flock must be held across the whole swap. Without it a concurrent
// `rafikid db psql` (which resolves the DSN, so it runs Ensure) can start a
// postmaster on the directory about to be renamed away, or initdb a fresh
// cluster in the gap between the two renames. The probe asks the REAL lock, from
// inside stop and start, whether it is free — an unlocked stop is the bug.
func TestRestorePhysicalHoldsTheServerLockAcrossTheSwap(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")

	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)
	var held []string
	f.probe = func(stage string, s embedpg.Server) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		release, err := s.Lock(ctx)
		if err == nil {
			release()
			t.Errorf("the embedpg lock was FREE during %s: another rafikid can change the server there", stage)
			return
		}
		held = append(held, stage)
	}

	c.NoError(runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard), "runRestore returned error")
	c.EqDeep([]string{"stop", "start"}, held, "the lock must be held at each stage probed, got")
}

// The marker is what covers the window no lock and no rollback can: a SIGKILL
// between the two renames. It has to exist while the directory is being moved,
// and be gone once the server is back up — a leftover one would make the next
// start refuse for no reason.
func TestRestorePhysicalWritesTheMarkerBeforeMovingTheDirectoryAside(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")

	pinRestoreStamp(t, 1_700_000_000)
	marker := dataDir + embedpg.RestoreInProgressSuffix
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	stubRestoreOps(t, dataDir, true)

	origRename := renamePath
	t.Cleanup(func() { renamePath = origRename })
	renames, secondSawIt := 0, false
	renamePath = func(from, to string) error {
		renames++
		switch renames {
		case 1:
			body, rerr := os.ReadFile(marker)
			c.NoError(rerr, "the marker must be written BEFORE the data directory moves")
			c.StrContains(string(body), filepath.Base(to),
				"the marker must name where the previous directory went, got: %s", body)
		case 2:
			_, rerr := os.Stat(marker)
			secondSawIt = rerr == nil
		}
		return os.Rename(from, to)
	}

	c.NoError(runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard), "runRestore returned error")
	c.Eq(2, renames, "a restore with a data directory in place makes exactly two renames, got")
	c.True(secondSawIt, "the marker must survive between the two renames: that window is what it is for")
	c.NoFileExists(marker, "a completed restore must remove its marker")
}

// The rollback must never delete a directory this command did not create. If the
// staging tree cannot be put in place — something else created the data
// directory in the gap, exactly what a concurrent Ensure used to do — the
// rollback leaves it alone and reports where the preserved directory is.
func TestRestorePhysicalRollsBackAFailedSecondRename(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	original := snapshotTree(t, dataDir)

	pinRestoreStamp(t, 1_700_000_000)
	marker := dataDir + embedpg.RestoreInProgressSuffix
	replaced := dataDir + embedpg.ReplacedDataDirPrefix + "1700000000"
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)

	origRename := renamePath
	t.Cleanup(func() { renamePath = origRename })
	renames := 0
	renamePath = func(from, to string) error {
		renames++
		if renames != 2 {
			return os.Rename(from, to)
		}
		// The gap: someone else's fresh cluster is now where the restored tree was
		// going, and this rename is the one that fails.
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Errorf("plant the concurrent data directory: %v", err)
		}
		writeBytes(t, filepath.Join(dataDir, "notice"), "someone else's cluster")
		return errors.New("rename: directory not empty")
	}

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

	c.Error(err, "want the second-rename failure, got nil")
	c.ErrorContains(err, "moving the restored directory into place", "error, got: %v", err)
	c.Eq(3, renames, "the rollback must still attempt to put the previous directory back, got %d renames", renames)

	// The directory this command did NOT create survives untouched...
	c.Eq("someone else's cluster", string(mustRead(t, filepath.Join(dataDir, "notice"))),
		"the rollback deleted a directory the restore did not create, got")
	// ...and the database that was there is still preserved, by name.
	c.DirExists(replaced, "the previous data directory must be preserved")
	c.EqDeep(original, snapshotTree(t, replaced), "the preserved directory must be untouched, got")
	c.ErrorContains(err, replaced, "the refusal must say where the previous directory is, got: %v", err)

	// The marker stays: the install is NOT back where it was, so the next start
	// must refuse to initialise an empty cluster over it.
	c.FileExists(marker, "a rollback that could not restore the directory must keep the marker")
	c.EqDeep([]string{"unpack", "status", "stop", "start"}, f.ops,
		"the restart is still owed: the server was stopped, got ops")
}

// A stop that fails means the server was never down, so no restart is owed — and
// nothing may have been moved. This is the backup's rule, and the restore owes
// the same one.
func TestRestorePhysicalDoesNotRestartAfterAFailedStop(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	before := snapshotTree(t, dataDir)

	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)
	f.stopErr = errors.New("pg_ctl: cannot stop the server")

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

	c.Error(err, "want the stop failure, got nil")
	c.ErrorContains(err, "cannot stop the server", "error, got: %v", err)
	c.EqDeep([]string{"unpack", "status", "stop"}, f.ops, "a failed stop must not be followed by a start, got ops")
	c.EqDeep(before, snapshotTree(t, dataDir), "the data directory must be untouched, got")
	c.NoFileExists(dataDir+embedpg.RestoreInProgressSuffix, "a refused restore must leave no marker")
	assertNoRestoreSiblings(c, dataDir)
	c.True(f.running, "the server was never stopped, so it must still be running")
}

// A start failure on a FRESH install has nothing to put back: the previous state
// was "no data directory at all", so the rollback removes the tree it unpacked,
// stops the postmaster the failed start may have left on it, and leaves no
// marker (nothing is displaced).
func TestRestorePhysicalRollsBackAFailedStartOnAFreshInstall(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")

	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, false)
	f.failFirstStart = true

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

	c.Error(err, "want the start failure, got nil")
	c.ErrorContains(err, "did not start", "error, got: %v", err)
	// unpack, start (fails), stop (the postmaster the failed start may have left).
	c.EqDeep([]string{"unpack", "start", "stop"}, f.ops, "ops, got")
	c.NoDirExists(dataDir, "the rollback must remove the tree it unpacked")
	c.NoFileExists(dataDir+embedpg.RestoreInProgressSuffix, "a fresh install has nothing to preserve")
	assertNoRestoreSiblings(c, dataDir)
	c.EqDeep([]string{"app.txz"}, namesIn(t, parent), "nothing may be left beside the data directory, got")
}

// A dump file whose name begins with "-" is a filename, not an option: the path
// goes to pg_restore behind `--`. Without it the archive is parsed as a flag and
// the restore fails on a file the operator can point at.
func TestRestoreDumpPassesAFlagShapedPathAfterADoubleDash(t *testing.T) {
	c := assert.NewAborting(t)
	argvFile, _ := fakePgRestoreDir(t)
	stubRestoreGates(t)

	dir := t.TempDir()
	archive := writeBytes(t, filepath.Join(dir, "-a-dump.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer

	err := runRestore(context.Background(), "postgres://u@h/app", archive, true, false, &stderr)

	c.NoError(err, "runRestore returned error: %s", stderr.String())
	argv := string(mustRead(t, argvFile))
	c.StrContains(argv, "--\n"+archive+"\n", "the flag-shaped path must follow `--`, got: %s", argv)
}

// The nastiest rollback state: the restored tree is in place, its start failed,
// and the postmaster it left will not shut down. Deleting the files under a live
// postmaster is how a rollback corrupts a cluster, so nothing is removed — both
// trees stay where they are, the error names both, and the marker stays so a
// later missing data directory refuses instead of initialising an empty cluster.
func TestRestorePhysicalLeavesBothTreesWhenTheRollbackCannotStop(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	original := snapshotTree(t, dataDir)

	pinRestoreStamp(t, 1_700_000_000)
	replaced := dataDir + embedpg.ReplacedDataDirPrefix + "1700000000"
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)
	f.failFirstStart = true
	f.failSecondStop = true

	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)

	c.Error(err, "want the rollback's stop failure, got nil")
	c.ErrorContains(err, "server does not shut down", "error, got: %v", err)
	c.ErrorContains(err, dataDir, "the error must say where the restored tree is, got: %v", err)
	c.ErrorContains(err, replaced, "the error must say where the previous tree is, got: %v", err)
	c.EqDeep([]string{"unpack", "status", "stop", "start", "stop"}, f.ops, "ops, got")

	// Nothing was removed: the restored tree is still at the live path (it is what
	// the postmaster that would not stop is using), and the original is preserved.
	c.Eq("restored", string(mustRead(t, filepath.Join(dataDir, "marker"))), "the restored tree must be left in place, got")
	c.EqDeep(original, snapshotTree(t, replaced), "the previous directory must be preserved, got")
	c.FileExists(dataDir+embedpg.RestoreInProgressSuffix, "the marker must stay while the install is displaced")
}

// stubRestoreGates replaces the dump path's two other server-facing seams: the
// role probe (which would dial the DSN) and the local-daemon probe (which would
// find a DEVELOPER's rafikid on the real socket and refuse the test's restore).
func stubRestoreGates(t *testing.T) *restoreGates {
	t.Helper()
	g := &restoreGates{superuser: false, daemonSocket: "/tmp/fake-controller.sock"}
	origRole, origDaemon := restoreConnectedSuperuser, daemonListening
	t.Cleanup(func() { restoreConnectedSuperuser, daemonListening = origRole, origDaemon })
	restoreConnectedSuperuser = func(context.Context, string) (bool, error) {
		return g.superuser, g.roleErr
	}
	daemonListening = func() (bool, string) { return g.daemonAlive, g.daemonSocket }
	return g
}

// restoreGates is what stubRestoreGates answers with; fields can be changed
// between calls.
type restoreGates struct {
	superuser    bool
	roleErr      error
	daemonAlive  bool
	daemonSocket string
}

// A restore that has been interrupted BEFORE the swap does not perform it. The
// staging tree is this command's own, so the rollback removes it; the live data
// directory is never moved and the server is never stopped, which is what keeps
// a Ctrl-C from taking an install out of service for no reason.
func TestRestorePhysicalRefusesACancelledContext(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	original := snapshotTree(t, dataDir)

	pinRestoreStamp(t, 1_700_000_000)
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)

	ctx, cancel := context.WithCancel(context.Background())
	// The interrupt arrives while the archive is being unpacked — the last step
	// before the swap, and the one that takes the longest.
	f.unpack = func(_, dir string) error {
		writeBytes(t, filepath.Join(dir, "PG_VERSION"), embedpg.MajorVersion+"\n")
		writeBytes(t, filepath.Join(dir, "postgresql.conf"), "# restored\n")
		cancel()
		return nil
	}

	err := runRestore(ctx, embeddedDSN, archive, true, false, io.Discard)

	c.Error(err, "want the cancellation, got nil")
	c.True(errors.Is(err, context.Canceled), "error, got: %v", err)
	c.EqDeep([]string{"unpack"}, f.ops,
		"an interrupted restore must not stop the server or move any directory, got ops")
	c.EqDeep(original, snapshotTree(t, dataDir), "the live data directory must be untouched, got")
	assertNoRestoreSiblings(c, dataDir)
	c.NoFileExists(dataDir+embedpg.RestoreInProgressSuffix, "no marker may be left behind")
}

// --- ownership, the daemon gate, the interrupt and the flush ---------------

// Ownership follows the role that connects. pg_restore --no-owner makes the
// CONNECTING user the owner of everything it creates: right for the daemon's
// unprivileged role, and catastrophic for a superuser, which would then own the
// whole schema and leave the daemon with permission denied. So a superuser
// restore must NOT pass it — the archive's OWNER TO statements are what put the
// original owners back.
func TestRestoreDumpPassesNoOwnerOnlyForANonSuperuser(t *testing.T) {
	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")

	for _, tc := range []struct {
		name      string
		superuser bool
		wantFlag  bool
	}{
		{name: "superuser keeps the archive's owners", superuser: true, wantFlag: false},
		{name: "unprivileged role owns what it creates", superuser: false, wantFlag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			argvFile, _ := fakePgRestoreDir(t)
			gates := stubRestoreGates(t)
			gates.superuser = tc.superuser

			var stderr bytes.Buffer
			c.NoError(runRestore(context.Background(), "postgres://u@h/app", archive, true, false, &stderr),
				"runRestore: %s", stderr.String())

			argv := string(mustRead(t, argvFile))
			has := false
			for _, arg := range strings.Split(argv, "\n") {
				if arg == "--no-owner" {
					has = true
				}
			}
			c.Eq(tc.wantFlag, has, "pg_restore argv for superuser=%v must %s --no-owner, got: %s",
				tc.superuser, map[bool]string{true: "carry", false: "omit"}[tc.wantFlag], argv)
			// Either way the target is named before anything runs, and ownership
			// is stated rather than implied.
			if tc.superuser {
				c.StrContains(stderr.String(), "ownership", "a superuser restore must say what it does about ownership, got: %s",
					stderr.String())
			}
		})
	}
}

// A role the archive names that does not exist here fails the restore; the flag
// that makes that possible is the one under test above, so the message at least
// has to be predictable. Pinned through the seam: pg_restore reports it.
func TestRestoreDumpPropagatesAMissingOwnerRole(t *testing.T) {
	c := assert.NewAborting(t)
	gates := stubRestoreGates(t)
	gates.superuser = true

	dir := t.TempDir()
	script := "#!/bin/sh\necho 'pg_restore: error: could not execute query: ERROR:  role \"rafiki\" does not exist' >&2\nexit 1\n"
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "pg_restore"), []byte(script), 0o755), "write fake pg_restore")
	t.Setenv("PATH", dir)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := runRestore(context.Background(), "postgres://u@h/app", archive, true, false, &stderr)

	c.Error(err, "want the restore failure, got nil")
	c.StrContains(stderr.String(), "does not exist", "pg_restore's own diagnosis must reach the operator, got: %s",
		stderr.String())
}

// A daemon left running while a dump restore runs is refused, not merely warned
// about: a daemon that keeps writing to the tables this restore replaces makes
// the run something other than the restore the operator meant to perform, and
// that is not a choice to make for them. The run is not corrupt for it —
// `pg_restore --clean --single-transaction` takes ACCESS EXCLUSIVE locks, so the
// daemon's writers block, and whatever it wrote before the drop is replaced by
// design — which is precisely why the refusal has to be stated rather than left
// to be noticed. --force is the escape hatch, and it says what it accepted.
func TestRestoreDumpRefusesUnderALiveDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	stubRestoreGates(t).daemonAlive = true
	argvFile, _ := fakePgRestoreDir(t)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	err := runRestore(context.Background(), "postgres://u@h/app", archive, false, false, &stderr)

	c.Error(err, "want a refusal while a local daemon is running, got nil")
	c.ErrorContains(err, "rafikid is running", "the refusal must name the daemon, got: %v", err)
	c.ErrorContains(err, "keeps writing to the tables this restore replaces",
		"the refusal must name the hazard, got: %v", err)
	c.ErrorContains(err, "--force", "the refusal must name the escape hatch, got: %v", err)
	c.NoFileExists(argvFile, "a refused restore must not run pg_restore")
}

// ...and with --force it proceeds, having said so. Same socket, same restore.
func TestRestoreDumpProceedsUnderALiveDaemonWithForce(t *testing.T) {
	c := assert.NewAborting(t)
	stubRestoreGates(t).daemonAlive = true
	argvFile, _ := fakePgRestoreDir(t)

	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")
	var stderr bytes.Buffer
	c.NoError(runRestore(context.Background(), "postgres://u@h/app", archive, true, false, &stderr),
		"runRestore: %s", stderr.String())

	c.FileExists(argvFile, "an accepted restore must run pg_restore")
	c.StrContains(stderr.String(), "warning", "the accepted run must say what it accepted, got: %s", stderr.String())
	c.StrContains(stderr.String(), "may write to the tables this restore replaces",
		"the warning must name the hazard it accepted, got: %s", stderr.String())
}

// A stop can take up to 90 seconds, so an interrupt during it lands after the
// pre-swap check. It must still be honoured: the rollback restarts the server
// this command stopped, and no directory is moved.
func TestRestorePhysicalHonoursAnInterruptDuringTheStop(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")
	original := snapshotTree(t, dataDir)

	pinRestoreStamp(t, 1_700_000_000)
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)

	ctx, cancel := context.WithCancel(context.Background())
	f.probe = func(stage string, _ embedpg.Server) {
		if stage == "stop" {
			cancel()
		}
	}

	err := runRestore(ctx, embeddedDSN, archive, true, false, io.Discard)

	c.Error(err, "want the cancellation, got nil")
	c.True(errors.Is(err, context.Canceled), "error, got: %v", err)
	c.StrContains(err.Error(), "was not replaced", "the error must say nothing was replaced, got: %v", err)
	c.EqDeep([]string{"unpack", "status", "stop", "start"}, f.ops,
		"an interrupt during the stop must restart the server it stopped and move nothing, got ops")
	c.EqDeep(original, snapshotTree(t, dataDir), "the live data directory must be untouched, got")
	c.True(f.running, "the server must be left running, as it was")
	assertNoRestoreSiblings(c, dataDir)
	c.NoFileExists(dataDir+embedpg.RestoreInProgressSuffix, "no marker may be written")
}

// The unpacked tree is flushed before it becomes the live data directory, and a
// flush that fails refuses the swap instead of installing a tree the kernel has
// not committed to disk.
func TestRestorePhysicalFlushesTheTreeBeforeTheSwap(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "pg")
	writeDataDirTree(t, dataDir, "original")

	pinRestoreStamp(t, 1_700_000_000)
	staging := dataDir + ".restoring-1700000000"
	archive := writeTarArchive(t, filepath.Join(parent, "app.txz"), formatTXZ,
		dataDirArchiveEntries()...)
	f := stubRestoreOps(t, dataDir, true)

	var flushed []string
	restoreEmbedded.sync = func(_ context.Context, dir string) error {
		flushed = append(flushed, dir)
		return nil
	}
	c.NoError(runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard), "runRestore")
	c.EqDeep([]string{staging}, flushed, "the STAGING tree must be flushed, before it is moved into place")
	c.EqDeep([]string{"unpack", "status", "stop"}, f.ops[:3], "the flush must happen before the server is stopped")
	c.FileExists(filepath.Join(dataDir, "PG_VERSION"), "the restored tree is in place")

	// And a flush that fails never reaches the destructive phase. A later stamp,
	// because the first restore left its own .replaced-<stamp> beside the tree.
	pinRestoreStamp(t, 1_700_000_100)
	f2 := stubRestoreOps(t, dataDir, true)
	restoreEmbedded.sync = func(context.Context, string) error { return errors.New("disk on fire") }
	writeDataDirTree(t, dataDir, "original")
	original := snapshotTree(t, dataDir)
	err := runRestore(context.Background(), embeddedDSN, archive, true, false, io.Discard)
	c.Error(err, "want the flush failure, got nil")
	c.StrContains(err.Error(), "disk on fire", "error, got: %v", err)
	c.EqDeep([]string{"unpack"}, f2.ops, "a failed flush must stop before the server is touched")
	c.EqDeep(original, snapshotTree(t, dataDir), "the live data directory must be untouched, got")
}

// A dump taken from ANOTHER cluster names roles this one does not have, and a
// superuser restore applies those OWNER TO statements — so the whole transaction
// fails and there is no way out. --no-owner is that way out, and it says what it
// costs; the default keeps the archive's owners.
func TestRestoreDumpNoOwnerFlagOverridesArchiveOwnership(t *testing.T) {
	archive := writeBytes(t, filepath.Join(t.TempDir(), "app.dump"), "PGDMP\x01\x0e\x00archive")

	for _, tc := range []struct {
		name    string
		noOwner bool
		want    bool
	}{
		{name: "default: a superuser keeps the archive's owners", noOwner: false, want: false},
		{name: "--no-owner: everything belongs to the connecting role", noOwner: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			argvFile, _ := fakePgRestoreDir(t)
			stubRestoreGates(t).superuser = true

			var stderr bytes.Buffer
			c.NoError(runRestore(context.Background(), "postgres://u@h/app", archive, true, tc.noOwner, &stderr),
				"runRestore: %s", stderr.String())

			has := false
			for _, arg := range strings.Split(string(mustRead(t, argvFile)), "\n") {
				if arg == "--no-owner" {
					has = true
				}
			}
			c.Eq(tc.want, has, "pg_restore argv with --no-owner=%v, got: %s", tc.noOwner, string(mustRead(t, argvFile)))
			if tc.noOwner {
				c.StrContains(stderr.String(), "fix ownership afterwards",
					"the flag must say what it costs, got: %s", stderr.String())
			}
		})
	}
}
