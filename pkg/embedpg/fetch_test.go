package embedpg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	tw := tar.NewWriter(xw)
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
// observable from outside extractTar. What it does pin: nothing may appear
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

// A regular entry named after an earlier symlink must fail closed rather than
// resolve through the link. It fails at the write: writeEntry's Lstat sees the
// symlink. The name describes the mechanism, not what this test isolates —
// creating the links inline instead, the Lstat refuses the same archive and this
// test still passes. The ordering is defence in depth (see extractTar); nothing
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
