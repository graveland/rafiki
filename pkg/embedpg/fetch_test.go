package embedpg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multigres/testkit/assert"
	"github.com/ulikunitz/xz"
)

// tarEntry is one member of a synthetic archive. body is ignored unless the
// type is tar.TypeReg.
type tarEntry struct {
	name     string
	mode     int64
	typeflag byte
	linkname string
	body     string
}

// buildTar packs entries as a plain tar stream, which is what ExtractTar reads:
// no jar, no compression — the extraction guards are the subject.
func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar.WriteHeader(%q): %v", e.name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar.Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Close: %v", err)
	}
	return buf.Bytes()
}

// buildJar packs entries as a tar.xz and wraps that as the single entry of a
// zip, which is exactly the shape of the zonky embedded-postgres jar: a zip
// whose only interesting member is a .txz.
func buildJar(t *testing.T, entryName string, entries []tarEntry) []byte {
	t.Helper()

	var txz bytes.Buffer
	xw, err := xz.NewWriter(&txz)
	if err != nil {
		t.Fatalf("xz.NewWriter: %v", err)
	}
	if _, err := xw.Write(buildTar(t, entries)); err != nil {
		t.Fatalf("xz.Write: %v", err)
	}
	if err := xw.Close(); err != nil {
		t.Fatalf("xz.Close: %v", err)
	}

	var jar bytes.Buffer
	zw := zip.NewWriter(&jar)
	w, err := zw.Create(entryName)
	if err != nil {
		t.Fatalf("zip.Create: %v", err)
	}
	if _, err := w.Write(txz.Bytes()); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	return jar.Bytes()
}

// sha256Hex is the digest the artifact table would carry for these bytes.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// overrideArtifact replaces the pinned build for the running platform, which is
// the one ensureBinaries looks up, and restores it afterwards.
func overrideArtifact(t *testing.T, a artifact) {
	t.Helper()
	key := runtime.GOOS + "/" + runtime.GOARCH
	old, ok := artifacts[key]
	artifacts[key] = a
	t.Cleanup(func() {
		if ok {
			artifacts[key] = old
			return
		}
		delete(artifacts, key)
	})
}

// serveJar answers every request with jar and counts them.
func serveJar(t *testing.T, jar []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if _, err := w.Write(jar); err != nil {
			t.Logf("write jar: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// testServer is a Server rooted entirely in a temporary directory: no test may
// touch the real cache, data or state directories.
func testServer(t *testing.T) Server {
	t.Helper()
	dir := t.TempDir()
	return Server{
		BinDir:   filepath.Join(dir, "bin"),
		DataDir:  filepath.Join(dir, "data"),
		LogPath:  filepath.Join(dir, "pg.log"),
		LockPath: filepath.Join(dir, "pg.lock"),
	}
}

// assertNoDebris checks that a failed download or extraction left neither a jar
// nor a half-extracted tree beside BinDir. Both cleanups are error paths, so
// without this they can be deleted and every test still passes.
func assertNoDebris(t *testing.T, s Server) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(s.BinDir), "embedpg-*"))
	assert.NewCollecting(t).NoError(err)
	assert.NewCollecting(t).Len(leftovers, 0, "debris left behind: %v", leftovers)
}

func TestArtifactForUnsupportedPlatform(t *testing.T) {
	_, err := artifactFor("windows", "amd64")
	assert.NewCollecting(t).ErrorContains(err, "supported: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64")
}

func TestJarURL(t *testing.T) {
	got := jarURL(DefaultBaseURL, artifacts["darwin/arm64"])
	want := "https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-darwin-arm64v8/18.3.0/embedded-postgres-binaries-darwin-arm64v8-18.3.0.jar"
	assert.NewCollecting(t).Eq(want, got, "jarURL")
}

// A jar that does not match its pinned digest must be rejected before anything
// is unpacked, and must leave nothing behind: neither a BinDir that later looks
// complete nor a temporary download.
func TestEnsureBinariesRejectsHashMismatch(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "bin", mode: 0o755, typeflag: tar.TypeDir},
		{name: "bin/postgres", mode: 0o755, typeflag: tar.TypeReg, body: "binary"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", strings.Repeat("ab", 32)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "sha256 mismatch")
	c.NoDirExists(s.BinDir)

	leftovers, globErr := filepath.Glob(filepath.Join(filepath.Dir(s.BinDir), "embedpg-*"))
	c.NoError(globErr)
	c.Len(leftovers, 0, "temporary download left behind: %v", leftovers)
}

// The happy path: the tree lands where it is expected, the executable bit and
// the symlink survive, and a second call is a no-op that never touches the
// network again.
func TestEnsureBinariesExtracts(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "bin", mode: 0o755, typeflag: tar.TypeDir},
		{name: "bin/postgres", mode: 0o755, typeflag: tar.TypeReg, body: "#!/bin/sh\n"},
		{name: "lib", mode: 0o755, typeflag: tar.TypeDir},
		{name: "lib/libx.so.1", mode: 0o644, typeflag: tar.TypeReg, body: "shared object"},
		{name: "lib/libx.so", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "libx.so.1"},
	})
	srv, hits := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	c := assert.NewCollecting(t)
	c.NoError(s.ensureBinaries(context.Background()))

	postgres := filepath.Join(s.BinDir, "bin", "postgres")
	info, err := os.Stat(postgres)
	c.NoError(err)
	c.NotZero(info.Mode().Perm()&0o111, "bin/postgres is not executable: %v", info.Mode())

	// os.Stat follows the link, so this fails unless the symlink resolves.
	body, err := os.ReadFile(filepath.Join(s.BinDir, "lib", "libx.so"))
	c.NoError(err)
	c.Eq("shared object", string(body))

	c.Eq(int64(1), hits.Load(), "requests after the first ensure")

	c.NoError(s.ensureBinaries(context.Background()))
	c.Eq(int64(1), hits.Load(), "requests after a second ensure")
}

// An entry that climbs out of the extraction root aborts the whole extraction,
// and nothing it produced survives: no BinDir, no jar, no temporary tree.
func TestExtractRejectsPathTraversal(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "../evil", mode: 0o644, typeflag: tar.TypeReg, body: "pwned"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "escapes the extraction root")
	c.NoDirExists(s.BinDir)
	c.NoFileExists(filepath.Join(filepath.Dir(s.BinDir), "evil"))
	assertNoDebris(t, s)
}

// A symlink chain is the case a lexical name check cannot see: `a -> .` and
// `a/b -> ..` are individually harmless names, and together they aim the next
// entry at the parent of the extraction root. The chain has to be resolved, not
// read.
func TestExtractRejectsSymlinkChain(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "a", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "."},
		{name: "a/b", mode: 0o777, typeflag: tar.TypeSymlink, linkname: ".."},
		{name: "a/b/evil", mode: 0o644, typeflag: tar.TypeReg, body: "pwned"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "escapes the extraction root")
	c.NoDirExists(s.BinDir)
	c.NoFileExists(filepath.Join(filepath.Dir(s.BinDir), "evil"))
	assertNoDebris(t, s)
}

// The dangling-symlink chain that escaped before the containment rework.
// `p/q/s -> ../..` is refused here because a link target may not climb; if that
// rule were dropped, `a` would still fail closed at the write, because
// writeEntry's Lstat and O_NOFOLLOW refuse to write through a link. So this test
// does NOT isolate the phase ordering — that is defence in depth and not
// observable from outside ExtractTar. What it does pin: nothing may appear
// beside the extraction root, and the extraction must fail rather than
// half-succeed.
func TestExtractRejectsDanglingSymlinkChain(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "p", mode: 0o755, typeflag: tar.TypeDir},
		{name: "p/q", mode: 0o755, typeflag: tar.TypeDir},
		{name: "p/q/s", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "../.."},
		{name: "a", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "d/x"},
		{name: "d", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "p/q/s/.."},
		{name: "a", mode: 0o644, typeflag: tar.TypeReg, body: "pwned"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.Error(err)
	// The escape this archive aims at: <root>/../x, i.e. beside the root.
	c.NoFileExists(filepath.Join(filepath.Dir(s.BinDir), "x"))
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// The containment guards live in ExtractTar and are shared with `rafikid db
// restore`, so they are pinned here against the exported function itself — not
// only through a download — covering the same table the restore's own copy of
// the extractor used to carry. Each shape aborts the extraction, and nothing may
// land beside the root: refusing after writing is not refusing.
func TestExtractTarRefusesAnEscapingArchive(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{
			name:    "a parent-relative name",
			entries: []tarEntry{{name: "../evil", mode: 0o644, typeflag: tar.TypeReg, body: "pwned"}},
			want:    "escapes the extraction root",
		},
		{
			name:    "an absolute name",
			entries: []tarEntry{{name: "/etc/evil", mode: 0o644, typeflag: tar.TypeReg, body: "pwned"}},
			want:    "escapes the extraction root",
		},
		{
			name:    "a symlink to an absolute path",
			entries: []tarEntry{{name: "passwd", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
			want:    "points at the absolute path",
		},
		{
			name:    "a symlink that climbs out",
			entries: []tarEntry{{name: "up", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "../../etc"}},
			want:    "escapes the extraction root",
		},
		{
			name: "a symlink a later entry walks through",
			entries: []tarEntry{
				{name: "sub", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "."},
				{name: "sub/evil", mode: 0o644, typeflag: tar.TypeReg, body: "pwned"},
			},
			want: "symlink",
		},
		{
			name:    "a fifo",
			entries: []tarEntry{{name: ".s.PGSQL.5432", mode: 0o644, typeflag: tar.TypeFifo}},
			want:    "a fifo",
		},
		{
			name:    "a character device",
			entries: []tarEntry{{name: "dev0", mode: 0o644, typeflag: tar.TypeChar}},
			want:    "a character device",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", root, err)
			}

			err := ExtractTar(bytes.NewReader(buildTar(t, tc.entries)), root)

			c := assert.NewCollecting(t)
			c.Error(err, "want a refusal for %s, got nil", tc.name)
			c.ErrorContains(err, tc.want, "refusal, got: %v", err)
			// The escape every one of these shapes aims at: something created
			// beside the extraction root.
			left, rerr := os.ReadDir(parent)
			c.NoError(rerr)
			c.Len(left, 1, "the extraction created something beside the root: %v", left)
		})
	}
}

// A regular entry named after an earlier symlink must fail closed rather than
// resolve through the link. It fails at the write: writeEntry's Lstat sees the
// symlink. The name describes the mechanism, not what this test isolates —
// creating the links inline instead, the Lstat refuses the same archive and this
// test still passes. The ordering is defence in depth (see ExtractTar); nothing
// here would notice its absence.
func TestExtractCreatesSymlinksLast(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "f", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "t"},
		{name: "f", mode: 0o644, typeflag: tar.TypeReg, body: "body"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.Error(err, "a symlink colliding with an extracted file must fail, not resolve through it")
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// A symlink whose target climbs out of the extraction root is refused just like
// a directory-traversing name: writing the link is harmless, following it later
// is not.
func TestExtractRejectsEscapingSymlink(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "lib", mode: 0o755, typeflag: tar.TypeDir},
		{name: "lib/x", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "../../outside"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "escapes the extraction root")
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// A jar whose layout does not match the pinned artifact is an error naming the
// entry that was looked for — not a silent empty tree.
func TestEnsureBinariesMissingTxz(t *testing.T) {
	jar := buildJar(t, "postgres-other.txz", []tarEntry{
		{name: "bin/postgres", mode: 0o755, typeflag: tar.TypeReg, body: "binary"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "postgres-test.txz")
	c.NoDirExists(s.BinDir)
}

// An absolute entry name is refused by the extractor's own check. Without it,
// filepath.Join would quietly anchor the name under the extraction root: the
// archive would appear to succeed, which is why the assertion is on the error
// and on BinDir, not on a file beside it.
func TestExtractRejectsAbsoluteEntryName(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: filepath.Join(t.TempDir(), "evil"), mode: 0o644, typeflag: tar.TypeReg, body: "pwned"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "escapes the extraction root")
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// A symlink inside the tree may point at anything except an absolute path: the
// link itself is harmless to create, but following it later is not.
func TestExtractRejectsAbsoluteSymlinkTarget(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "lib", mode: 0o755, typeflag: tar.TypeDir},
		{name: "lib/x", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "points at the absolute path")
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// The extractor handles directories, regular files and symlinks, and refuses
// everything else rather than guessing: a device node or a fifo in the archive
// is not something this package can make.
func TestExtractRejectsUnsupportedEntryType(t *testing.T) {
	jar := buildJar(t, "postgres-test.txz", []tarEntry{
		{name: "bin", mode: 0o755, typeflag: tar.TypeDir},
		{name: "bin/pipe", mode: 0o644, typeflag: tar.TypeFifo},
	})
	srv, _ := serveJar(t, jar)
	overrideArtifact(t, artifact{"test-platform", "postgres-test.txz", sha256Hex(jar)})

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "a fifo")
	c.ErrorContains(err, "only regular files, directories and symlinks")
	c.ErrorContains(err, "bin/pipe")
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// A mirror answering anything but 200 is an error naming the status, and it
// leaves nothing behind: no jar, no tree.
func TestEnsureBinariesRejectsNon200(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "unexpected status")
	c.ErrorContains(err, "404")
	c.Eq(int64(1), hits.Load(), "requests")
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// A declared length past the cap is refused before a byte is written, so a
// broken or hostile mirror cannot fill the cache disk on the way to a digest
// check that would have refused the bytes anyway.
func TestDownloadRejectsDeclaredLengthOverLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1099511627776") // 1 TiB
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := testServer(t)
	s.BaseURL = srv.URL
	s.Client = srv.Client()

	err := s.ensureBinaries(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "download limit")
	c.ErrorContains(err, strconv.FormatInt(maxDownloadBytes, 10))
	c.NoDirExists(s.BinDir)
	assertNoDebris(t, s)
}

// countingBody records how much of a response the client actually read, which
// is what tells a bounded copy apart from an unbounded one that only complains
// afterwards.
type countingBody struct {
	r    io.Reader
	read *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return nil }

// countingTransport answers every request with body and no declared length, so
// only the copy's own bound can stop it.
type countingTransport struct {
	body []byte
	read *atomic.Int64
}

func (t countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		ContentLength: -1,
		Body:          &countingBody{r: bytes.NewReader(t.body), read: t.read},
	}, nil
}

// The declared length is the mirror's claim, so the copy is bounded
// independently: a body streamed without a Content-Length stops at the cap
// instead of landing on disk first and being refused afterwards. The assertion
// is on how much was READ, not only on the error — a copy that read the whole
// body and then checked the size would report the same error.
func TestDownloadStopsAtLimitWhenLengthIsNotDeclared(t *testing.T) {
	old := maxDownloadBytes
	maxDownloadBytes = 64
	t.Cleanup(func() { maxDownloadBytes = old })

	var read atomic.Int64
	s := testServer(t)
	s.Client = &http.Client{Transport: countingTransport{
		body: bytes.Repeat([]byte("x"), 1<<20),
		read: &read,
	}}

	_, _, err := s.download(context.Background(), "http://example.invalid/jar", filepath.Dir(s.BinDir))
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "exceeds the")
	c.ErrorContains(err, "limit")
	c.LessOrEqual(maxDownloadBytes+1, read.Load(), "bytes read from the response")
	assertNoDebris(t, s)
}

// A directory the archive records as read-only must be made read-only only
// AFTER its entries are written: applying the mode where the directory is
// created makes the very next write fail with EACCES, and the tree the caller
// gets is a half-extracted one. This is the behaviour the extractor's mode
// deferral exists for, so reverting it to "chmod at mkdir time" must fail here.
func TestExtractTarAppliesDirectoryModesAfterTheirEntries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the 0555 mode this test needs to be a wall")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	locked := filepath.Join(root, "locked")
	// LIFO: registered after t.TempDir's own cleanup, so this runs first and the
	// read-only directory can be removed at all.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	err := ExtractTar(bytes.NewReader(buildTar(t, []tarEntry{
		{name: "locked", mode: 0o555, typeflag: tar.TypeDir},
		{name: "locked/child", mode: 0o640, typeflag: tar.TypeReg, body: "kept"},
	})), root)

	c := assert.NewAborting(t)
	c.NoError(err, "ExtractTar returned error")
	body, rerr := os.ReadFile(filepath.Join(locked, "child"))
	c.NoError(rerr, "the entry under a read-only directory must be written")
	c.Eq("kept", string(body), "body, got")

	info, serr := os.Stat(locked)
	c.NoError(serr, "stat the extracted directory")
	c.Eq(os.FileMode(0o555), info.Mode().Perm(), "the archived directory mode, got")
	child, cerr := os.Stat(filepath.Join(locked, "child"))
	c.NoError(cerr, "stat the extracted file")
	c.Eq(os.FileMode(0o640), child.Mode().Perm(), "the archived file mode, got")
}
