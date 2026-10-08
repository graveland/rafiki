// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/sys/unix"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"

	"github.com/multigres/testkit/assert"
)

// treeTestServer builds a real Server and a client that talks to it over a
// unix socket with h2c, so streaming RPCs exercise the full handler path
// rather than a hand-rolled stream.
func treeTestServer(t *testing.T, opts Options) executorpbconnect.ExecutorServiceClient {
	t.Helper()
	c := assert.NewAborting(t)
	opts.NoLSP = true
	srv := NewServer(opts)

	// t.Name() carries "/" for every subtest, which turns the socket path into
	// a directory that does not exist.
	sockPath := filepath.Join("/tmp", "rafiki-tree-"+strings.ReplaceAll(t.Name(), "/", "_")+".sock")
	os.Remove(sockPath) // stale from a crashed run
	t.Cleanup(func() { os.Remove(sockPath) })

	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: mux, Protocols: protos}

	ln, err := net.Listen("unix", sockPath)
	c.NoError(err, "listen")
	c.NoError(os.Chmod(sockPath, 0o600), "chmod")
	t.Cleanup(func() { httpSrv.Close() })
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()

	return executorpbconnect.NewExecutorServiceClient(
		&http.Client{
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
		},
		"http://executor",
	)
}

type tarEntry struct {
	name     string
	mode     int64
	typeflag byte
	linkname string
	body     string
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Size:     int64(len(e.body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", e.name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func sendTree(
	t *testing.T,
	client executorpbconnect.ExecutorServiceClient,
	start *executorpb.WriteTreeStart,
	tarBytes []byte,
) (*connect.Response[executorpb.WriteTreeResponse], error) {
	t.Helper()
	stream := client.WriteTree(context.Background())
	if err := stream.Send(&executorpb.WriteTreeRequest{
		Msg: &executorpb.WriteTreeRequest_Start{Start: start},
	}); err != nil {
		return nil, err
	}
	for off := 0; off < len(tarBytes); off += treeChunkSize {
		end := off + treeChunkSize
		if end > len(tarBytes) {
			end = len(tarBytes)
		}
		if err := stream.Send(&executorpb.WriteTreeRequest{
			Msg: &executorpb.WriteTreeRequest_Chunk{Chunk: tarBytes[off:end]},
		}); err != nil {
			// The server aborted; CloseAndReceive surfaces its typed error.
			break
		}
	}
	return stream.CloseAndReceive()
}

func readTreeAll(
	t *testing.T,
	client executorpbconnect.ExecutorServiceClient,
	p string,
) (bool, []byte, error) {
	t.Helper()
	stream, err := client.ReadTree(context.Background(), connect.NewRequest(&executorpb.ReadTreeRequest{Path: p}))
	if err != nil {
		return false, nil, err
	}
	var isDir bool
	var buf bytes.Buffer
	for stream.Receive() {
		m := stream.Msg()
		if h := m.GetHeader(); h != nil {
			isDir = h.GetIsDir()
		}
		if chunk := m.GetChunk(); chunk != nil {
			buf.Write(chunk)
		}
	}
	if err := stream.Err(); err != nil {
		return isDir, nil, err
	}
	return isDir, buf.Bytes(), nil
}

func int64p(v int64) *int64 { return &v }

// assertNoLeftovers fails if any staging or aside directory survives in dir.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".rafiki-sync-") || strings.HasPrefix(e.Name(), ".rafiki-old-") {
			t.Errorf("leftover %q in %s", e.Name(), dir)
		}
	}
}

func TestReadTreeRefusesSymlinkPath(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	c.Require().NoError(os.WriteFile(target, []byte("x"), 0o644), "write target")
	link := filepath.Join(dir, "link")
	c.Require().NoError(os.Symlink(target, link), "symlink")

	_, _, err := readTreeAll(t, client, link)
	c.Error(err, "ReadTree of a symlink must fail")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "symlink path code")
}

func TestReadTreeRefusesRelativePath(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	for _, p := range []string{"relative/path", "foo", "/tmp/../tmp", "tmp/"} {
		_, _, err := readTreeAll(t, client, p)
		c.Error(err, "ReadTree(%q) must fail", p)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "ReadTree(%q) code", p)
	}
}

func TestReadTreeSkipsSocketsAndFifos(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	src := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(src, "regular.txt"), []byte("keep"), 0o644), "regular file")
	c.Require().NoError(unix.Mkfifo(filepath.Join(src, "pipe"), 0o644), "mkfifo")

	sockPath := filepath.Join(src, "sock")
	ln, err := net.Listen("unix", sockPath)
	c.Require().NoError(err, "unix socket")
	t.Cleanup(func() { ln.Close() })

	isDir, data, err := readTreeAll(t, client, src)
	c.Require().NoError(err, "ReadTree")
	c.True(isDir, "is_dir")
	c.EqDeep([]string{"regular.txt"}, tarNames(t, data), "only the regular file is streamed")
}

func TestWriteTreeRejectsDotDotEntry(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	payload := buildTar(t, tarEntry{name: "../evil", typeflag: tar.TypeReg, mode: 0o644, body: "pwned"})

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Error(err, "a .. entry must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")

	if _, serr := os.Lstat(filepath.Join(parent, "evil")); !os.IsNotExist(serr) {
		t.Errorf("the .. entry escaped the staging directory: %s exists", filepath.Join(parent, "evil"))
	}
	if _, serr := os.Lstat(dest); !os.IsNotExist(serr) {
		t.Errorf("destination %s must not be created on failure", dest)
	}
	assertNoLeftovers(t, parent)
}

func TestWriteTreeRejectsAbsoluteEntry(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	abs := filepath.Join(t.TempDir(), "pwned-absolute")
	dest := filepath.Join(t.TempDir(), "dest")
	payload := buildTar(t, tarEntry{name: abs, typeflag: tar.TypeReg, mode: 0o644, body: "pwned"})

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Error(err, "an absolute entry name must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	if _, serr := os.Lstat(abs); !os.IsNotExist(serr) {
		t.Errorf("the absolute entry was created at %s", abs)
	}
}

func TestWriteTreeRefusesWriteThroughSymlink(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	outside := t.TempDir()
	dest := filepath.Join(t.TempDir(), "dest")
	payload := buildTar(t,
		tarEntry{name: "a", typeflag: tar.TypeSymlink, mode: 0o777, linkname: outside},
		tarEntry{name: "a/pwned", typeflag: tar.TypeReg, mode: 0o644, body: "pwned"},
	)

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Error(err, "writing through a symlinked ancestor must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")

	ents, rerr := os.ReadDir(outside)
	c.Require().NoError(rerr, "read outside dir")
	c.Empty(ents, "the symlink target directory must stay empty")
}

func TestWriteTreeRejectsSpecialEntries(t *testing.T) {
	cases := map[string]tarEntry{
		"hardlink": {name: "hard", typeflag: tar.TypeLink, linkname: "target", mode: 0o644},
		"char":     {name: "chr", typeflag: tar.TypeChar, mode: 0o644},
		"block":    {name: "blk", typeflag: tar.TypeBlock, mode: 0o644},
		"fifo":     {name: "fifo", typeflag: tar.TypeFifo, mode: 0o644},
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			payload := buildTar(t, entry)

			_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
			c.Error(err, "%s entry must be refused", name)
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s code", name)
			if _, serr := os.Lstat(dest); !os.IsNotExist(serr) {
				t.Errorf("destination created for refused %s entry", name)
			}
			assertNoLeftovers(t, parent)
		})
	}
}

func TestWriteTreeStripsSpecialModeBits(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	// The empty entry is what makes the mask observable: the kernel clears
	// setuid when a file is written, so a setuid file WITH content would end up
	// 0o755 whatever the mask does. An unwritten one keeps whatever mode it was
	// created with.
	dest := filepath.Join(t.TempDir(), "dest")
	payload := buildTar(t,
		tarEntry{name: "empty", typeflag: tar.TypeReg, mode: 0o4755},
		tarEntry{name: "script", typeflag: tar.TypeReg, mode: 0o4755, body: "#!/bin/sh\n"},
	)

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Require().NoError(err, "WriteTree")

	for _, name := range []string{"empty", "script"} {
		fi, serr := os.Stat(filepath.Join(dest, name))
		c.Require().NoError(serr, "stat %s", name)
		c.Eq(os.FileMode(0o755), fi.Mode().Perm(), "%s perm", name)
		c.Zero(fi.Mode()&os.ModeSetuid, "%s setuid must be stripped", name)
		c.NotZero(fi.Mode().Perm()&0o100, "%s exec bit must be kept", name)
	}
}

func TestWriteTreeCreatesSymlinkVerbatim(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	dest := filepath.Join(t.TempDir(), "dest")
	target := "/nonexistent/absolute/target"
	payload := buildTar(t, tarEntry{name: "link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: target})

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Require().NoError(err, "WriteTree")

	link := filepath.Join(dest, "link")
	fi, serr := os.Lstat(link)
	c.Require().NoError(serr, "lstat link")
	c.NotZero(fi.Mode()&os.ModeSymlink, "must be a symlink")
	got, rerr := os.Readlink(link)
	c.Require().NoError(rerr, "readlink")
	c.Eq(target, got, "link target must be verbatim")
}

func TestWriteTreeFailureLeavesDestinationUntouched(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	c.Require().NoError(os.MkdirAll(dest, 0o755), "mkdir dest")
	c.Require().NoError(os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("old"), 0o644), "seed dest")

	payload := buildTar(t,
		tarEntry{name: "good.txt", typeflag: tar.TypeReg, mode: 0o644, body: "new"},
		tarEntry{name: "../evil", typeflag: tar.TypeReg, mode: 0o644, body: "pwned"},
	)
	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true, Overwrite: true}, payload)
	c.Error(err, "the mid-stream .. entry must abort the transfer")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")

	got, rerr := os.ReadFile(filepath.Join(dest, "keep.txt"))
	c.Require().NoError(rerr, "read keep.txt")
	c.Eq("old", string(got), "existing destination content must be unchanged")
	if _, serr := os.Lstat(filepath.Join(dest, "good.txt")); !os.IsNotExist(serr) {
		t.Errorf("a partially extracted file leaked into the destination")
	}
	assertNoLeftovers(t, parent)
}

func TestWriteTreeRefusesNonEmptyDestination(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	dest := filepath.Join(t.TempDir(), "dest")
	c.Require().NoError(os.MkdirAll(dest, 0o755), "mkdir dest")
	c.Require().NoError(os.WriteFile(filepath.Join(dest, "old.txt"), []byte("old"), 0o644), "seed dest")

	payload := buildTar(t, tarEntry{name: "new.txt", typeflag: tar.TypeReg, mode: 0o644, body: "new"})
	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Error(err, "a non-empty destination without overwrite must be refused")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")

	if _, serr := os.Lstat(filepath.Join(dest, "new.txt")); !os.IsNotExist(serr) {
		t.Errorf("nothing may be written into a refused destination")
	}
	if _, serr := os.Lstat(filepath.Join(dest, "old.txt")); serr != nil {
		t.Errorf("the existing destination content must be left alone: %v", serr)
	}
}

func TestWriteTreeAcceptsEmptyDirDestination(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	dest := filepath.Join(t.TempDir(), "dest")
	c.Require().NoError(os.MkdirAll(dest, 0o755), "mkdir dest")

	payload := buildTar(t, tarEntry{name: "new.txt", typeflag: tar.TypeReg, mode: 0o644, body: "new"})
	resp, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Require().NoError(err, "an empty directory destination is replaceable")
	c.Eq(int64(1), resp.Msg.GetFiles(), "files")
	c.Eq(int64(3), resp.Msg.GetBytes(), "bytes")

	got, rerr := os.ReadFile(filepath.Join(dest, "new.txt"))
	c.Require().NoError(rerr, "read new.txt")
	c.Eq("new", string(got), "content")
}

func TestWriteTreeOverwriteReplacesTree(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	c.Require().NoError(os.MkdirAll(dest, 0o755), "mkdir dest")
	c.Require().NoError(os.WriteFile(filepath.Join(dest, "stale.txt"), []byte("stale"), 0o644), "seed dest")

	payload := buildTar(t, tarEntry{name: "fresh.txt", typeflag: tar.TypeReg, mode: 0o644, body: "fresh"})
	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true, Overwrite: true}, payload)
	c.Require().NoError(err, "overwrite must replace the tree")

	if _, serr := os.Lstat(filepath.Join(dest, "stale.txt")); !os.IsNotExist(serr) {
		t.Errorf("the stale file must be gone after an overwrite")
	}
	got, rerr := os.ReadFile(filepath.Join(dest, "fresh.txt"))
	c.Require().NoError(rerr, "read fresh.txt")
	c.Eq("fresh", string(got), "content")
	assertNoLeftovers(t, parent)
}

func TestWriteTreeOverwriteRestoresOldOnRenameFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	c.Require().NoError(os.MkdirAll(dest, 0o755), "mkdir dest")
	c.Require().NoError(os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("old"), 0o644), "seed dest")

	orig := treeRename
	treeRename = func(oldpath, newpath string) error {
		if newpath == dest && strings.HasPrefix(filepath.Base(oldpath), ".rafiki-sync-") {
			return errors.New("simulated staging rename failure")
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { treeRename = orig })

	payload := buildTar(t, tarEntry{name: "fresh.txt", typeflag: tar.TypeReg, mode: 0o644, body: "fresh"})
	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true, Overwrite: true}, payload)
	c.Error(err, "a failed swap must surface an error")

	got, rerr := os.ReadFile(filepath.Join(dest, "keep.txt"))
	c.Require().NoError(rerr, "the old tree must be restored")
	c.Eq("old", string(got), "old content")
	if _, serr := os.Lstat(filepath.Join(dest, "fresh.txt")); !os.IsNotExist(serr) {
		t.Errorf("the new tree must not be in place after a failed swap")
	}
	assertNoLeftovers(t, parent)
}

func TestWriteTreeMaxBytesAborts(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	payload := buildTar(t, tarEntry{name: "big.bin", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("x", 100)})

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true, MaxBytes: int64p(10)}, payload)
	c.Error(err, "exceeding max_bytes must abort")
	c.Eq(connect.CodeResourceExhausted, connect.CodeOf(err), "code")
	if _, serr := os.Lstat(dest); !os.IsNotExist(serr) {
		t.Errorf("destination created despite exceeding max_bytes")
	}
	assertNoLeftovers(t, parent)
}

func TestWriteTreeZeroMaxBytesRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	payload := buildTar(t, tarEntry{name: "f.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"})

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true, MaxBytes: int64p(0)}, payload)
	c.Error(err, "an explicit zero max_bytes must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	if _, serr := os.Lstat(dest); !os.IsNotExist(serr) {
		t.Errorf("destination created despite a refused max_bytes")
	}
}

func TestWriteTreeFreeSpaceFloorAborts(t *testing.T) {
	c := assert.NewCollecting(t)
	client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

	orig := treeFreeBytes
	treeFreeBytes = func(string) (int64, error) { return 1, nil }
	t.Cleanup(func() { treeFreeBytes = orig })

	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	payload := buildTar(t, tarEntry{name: "f.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"})

	_, err := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
	c.Error(err, "a free-space reading below the floor must abort")
	c.Eq(connect.CodeResourceExhausted, connect.CodeOf(err), "code")
	if _, serr := os.Lstat(dest); !os.IsNotExist(serr) {
		t.Errorf("destination created despite a low free-space reading")
	}
	assertNoLeftovers(t, parent)
}

// TestWriteTreeOverwritePathGuard is a pattern-prefixed shim: the verify
// command matches TestReadTree|TestWriteTree, and TestCheckOverwritePath is
// the guard's pinned name, so the shim runs it under the verify pattern.
func TestWriteTreeOverwritePathGuard(t *testing.T) {
	t.Run("TestCheckOverwritePath", TestCheckOverwritePath)
}

func TestCheckOverwritePath(t *testing.T) {
	c := assert.NewCollecting(t)
	root := "/srv/exec-root"
	home := "/home/tester"
	ancestorOfRoot := "/srv"

	cases := []struct {
		path    string
		allowed bool
	}{
		{"/", false},
		{"/work", false},
		{"/usr", false},
		{root, false},
		{home, false},
		{ancestorOfRoot, false},
		{"/work/repo", true},
	}
	for _, tc := range cases {
		err := checkOverwritePath(tc.path, root, home)
		if tc.allowed {
			c.NoError(err, "checkOverwritePath(%q) must be allowed", tc.path)
			continue
		}
		c.Error(err, "checkOverwritePath(%q) must be refused", tc.path)
	}
}

func TestReadTreeWriteTreeRoundTripDirAndFile(t *testing.T) {
	t.Run("dir", func(t *testing.T) {
		c := assert.NewCollecting(t)
		client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

		src := t.TempDir()
		c.Require().NoError(os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644), "a.txt")
		c.Require().NoError(os.MkdirAll(filepath.Join(src, "sub"), 0o755), "sub")
		c.Require().NoError(os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o644), "b.txt")
		c.Require().NoError(os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755), "run.sh")
		c.Require().NoError(os.Symlink("a.txt", filepath.Join(src, "link")), "link")

		isDir, payload, err := readTreeAll(t, client, src)
		c.Require().NoError(err, "ReadTree")
		c.True(isDir, "is_dir")

		dest := filepath.Join(t.TempDir(), "dest")
		resp, werr := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: true}, payload)
		c.Require().NoError(werr, "WriteTree")
		c.Eq(int64(3), resp.Msg.GetFiles(), "regular files")
		c.Eq(int64(len("hello")+len("world")+len("#!/bin/sh\n")), resp.Msg.GetBytes(), "bytes")

		got, rerr := os.ReadFile(filepath.Join(dest, "a.txt"))
		c.Require().NoError(rerr, "read a.txt")
		c.Eq("hello", string(got), "a.txt content")
		got, rerr = os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
		c.Require().NoError(rerr, "read sub/b.txt")
		c.Eq("world", string(got), "sub/b.txt content")

		runInfo, serr := os.Stat(filepath.Join(dest, "run.sh"))
		c.Require().NoError(serr, "stat run.sh")
		c.NotZero(runInfo.Mode().Perm()&0o100, "the exec bit must survive the round trip")

		linkInfo, serr := os.Lstat(filepath.Join(dest, "link"))
		c.Require().NoError(serr, "lstat link")
		c.NotZero(linkInfo.Mode()&os.ModeSymlink, "the symlink must survive the round trip")
		target, rerr := os.Readlink(filepath.Join(dest, "link"))
		c.Require().NoError(rerr, "readlink")
		c.Eq("a.txt", target, "link target")
	})

	t.Run("file", func(t *testing.T) {
		c := assert.NewCollecting(t)
		client := treeTestServer(t, Options{Root: t.TempDir(), Version: "test"})

		src := filepath.Join(t.TempDir(), "single.txt")
		c.Require().NoError(os.WriteFile(src, []byte("one file"), 0o644), "src file")

		isDir, payload, err := readTreeAll(t, client, src)
		c.Require().NoError(err, "ReadTree")
		c.False(isDir, "is_dir for a single file")

		dest := filepath.Join(t.TempDir(), "out.txt")
		resp, werr := sendTree(t, client, &executorpb.WriteTreeStart{Path: dest, IsDir: false}, payload)
		c.Require().NoError(werr, "WriteTree")
		c.Eq(int64(1), resp.Msg.GetFiles(), "files")
		c.Eq(int64(len("one file")), resp.Msg.GetBytes(), "bytes")

		got, rerr := os.ReadFile(dest)
		c.Require().NoError(rerr, "read dest")
		c.Eq("one file", string(got), "content")
	})
}
