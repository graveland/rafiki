package embedpg

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/ulikunitz/xz"
)

const (
	// Version is the PostgreSQL release the pinned artifacts below carry. It
	// moves only together with the four sha256 digests: a version bump without
	// re-pinned digests fails every download closed.
	Version = "18.3.0"
	// MajorVersion is what a data directory must report in PG_VERSION. A data
	// directory from another major cannot be opened by these binaries, and
	// pg_upgrade is not something this package does.
	MajorVersion = "18"
	// DefaultBaseURL is the Maven repository the zonky embedded-postgres
	// binaries are published to.
	DefaultBaseURL = "https://repo1.maven.org/maven2"
	// Port is the TCP port the server is configured for. It never listens on
	// TCP (listen_addresses is empty); the port only names the unix socket
	// file, .s.PGSQL.<Port>, inside the data directory.
	Port = 5432
)

// artifact is one platform's build of the server binaries: the Maven artifact
// suffix, the .txz entry inside the jar, and the digest of the jar itself.
type artifact struct {
	platform string // Maven artifact platform suffix
	txz      string // the .txz entry inside the jar
	sha256   string // hex sha256 of the downloaded .jar
}

var artifacts = map[string]artifact{
	"darwin/arm64": {"darwin-arm64v8", "postgres-darwin-arm_64.txz", "a18ec9ec6a905e04f5a08a8f7870bddadd635feb54dee332a3daf80fada3cb1a"},
	"darwin/amd64": {"darwin-amd64", "postgres-darwin-x86_64.txz", "cb68c57232945f5e74db946e6077d70cec56bfed35959b21acc750cc80ca0977"},
	"linux/amd64":  {"linux-amd64", "postgres-linux-x86_64.txz", "1974c3a59c3ae010e9818e81637e425aad94eaac26e4ef533b42e081fe7ca345"},
	"linux/arm64":  {"linux-arm64v8", "postgres-linux-arm_64.txz", "1fb555a6602c13a113ab1f0cc9224d599948e85e51ac5f001393605505eaf78e"},
}

// artifactFor returns the pinned build for goos/goarch. The platform list in
// the error is sorted so the message is stable across runs.
func artifactFor(goos, goarch string) (artifact, error) {
	if a, ok := artifacts[goos+"/"+goarch]; ok {
		return a, nil
	}
	return artifact{}, fmt.Errorf("embedpg: no PostgreSQL build for %s/%s; supported: %s",
		goos, goarch, strings.Join(supportedPlatforms(), ", "))
}

// supportedPlatforms lists the keys of artifacts, sorted.
func supportedPlatforms() []string {
	keys := make([]string, 0, len(artifacts))
	for k := range artifacts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jarURL is the Maven download URL of a's jar under base.
func jarURL(base string, a artifact) string {
	const prefix = "/io/zonky/test/postgres/embedded-postgres-binaries-"
	name := "embedded-postgres-binaries-" + a.platform + "-" + Version + ".jar"
	return base + prefix + a.platform + "/" + Version + "/" + name
}

// baseURL is BaseURL with the default filled in.
func (s Server) baseURL() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return DefaultBaseURL
}

// httpClient is Client with the default filled in.
func (s Server) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// ensureBinaries makes sure BinDir holds an extracted tree with a working
// bin/postgres. The presence of that one file is the completion marker: the
// extraction is renamed into place atomically, so a tree that has it is whole.
func (s Server) ensureBinaries(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.BinDir, "bin", "postgres")); err == nil {
		return nil
	}
	a, err := artifactFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.BinDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	// Debris from a run that was killed mid-download or mid-extract: neither a
	// partial jar nor a partial tree can be resumed, and nothing else ever
	// removes them.
	if stale, err := filepath.Glob(filepath.Join(dir, "embedpg-*")); err == nil {
		for _, path := range stale {
			os.RemoveAll(path)
		}
	}

	url := jarURL(s.baseURL(), a)
	if s.Progress != nil {
		fmt.Fprintf(s.Progress, "downloading PostgreSQL %s (%s)...\n", Version, a.platform)
	}
	jar, digest, err := s.download(ctx, url, dir)
	if err != nil {
		return err
	}
	if digest != a.sha256 {
		os.Remove(jar)
		return fmt.Errorf("embedpg: %s: sha256 mismatch: want %s, got %s", url, a.sha256, digest)
	}
	if s.Progress != nil {
		fmt.Fprintf(s.Progress, "verified sha256 %s\n", digest)
	}
	if err := s.extract(jar, url, a); err != nil {
		os.Remove(jar)
		return err
	}
	// The tree is what this function is for, and it is complete: a jar that will
	// not go away is debris the next run's embedpg-* sweep collects, not a failed
	// install.
	_ = os.Remove(jar)
	return nil
}

// maxDownloadBytes bounds one jar download. The pinned artifacts are 15-35 MB;
// the limit is generous enough for a future build and small enough that a
// broken or hostile mirror cannot fill the cache disk before the digest check
// refuses the bytes.
//
// A variable, not a constant, so the bound itself is testable without
// downloading 128 MB.
var maxDownloadBytes int64 = 128 << 20

// download fetches url into a temporary file in dir, returning its path and the
// hex sha256 of the bytes written. The digest covers exactly what landed on
// disk, so the caller verifies the file it will extract and not a second read
// of a URL that could have answered differently.
func (s Server) download(ctx context.Context, url, dir string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("embedpg: %s: %w", url, err)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("embedpg: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("embedpg: %s: unexpected status %s", url, resp.Status)
	}
	// A declared length over the bound is refused before a byte is written. The
	// length is the mirror's claim, so the copy below is bounded independently.
	if resp.ContentLength > maxDownloadBytes {
		return "", "", fmt.Errorf("embedpg: %s: %d bytes exceeds the %d-byte download limit",
			url, resp.ContentLength, maxDownloadBytes)
	}

	f, err := os.CreateTemp(dir, "embedpg-*.jar")
	if err != nil {
		return "", "", fmt.Errorf("embedpg: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", "", fmt.Errorf("embedpg: %s: %w", url, err)
	}
	// One byte past the bound is what the LimitReader allows through, so a body
	// that reached it is over the limit whether or not it declared a length.
	if n > maxDownloadBytes {
		f.Close()
		os.Remove(f.Name())
		return "", "", fmt.Errorf("embedpg: %s: download exceeds the %d-byte limit", url, maxDownloadBytes)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", "", fmt.Errorf("embedpg: %s: %w", url, err)
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// extract unpacks the .txz entry of the jar at jarPath into BinDir, atomically:
// the tree is built in a sibling temporary directory and renamed into place, so
// an interrupted extraction leaves no half-populated BinDir to be mistaken for
// a complete one.
func (s Server) extract(jarPath, url string, a artifact) error {
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return fmt.Errorf("embedpg: %s: %w", jarPath, err)
	}
	defer zr.Close()

	var entry *zip.File
	for _, f := range zr.File {
		if f.Name == a.txz {
			entry = f
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("embedpg: %s has no %s", url, a.txz)
	}

	rc, err := entry.Open()
	if err != nil {
		return fmt.Errorf("embedpg: %s: %w", url, err)
	}
	defer rc.Close()
	xr, err := xz.NewReader(rc)
	if err != nil {
		return fmt.Errorf("embedpg: %s: %s: %w", url, a.txz, err)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(s.BinDir), "embedpg-extract-*")
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	if err := extractTar(tar.NewReader(xr), tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	// BinDir can exist without bin/postgres — that is why we are here — and
	// rename(2) cannot replace a non-empty directory. Such a tree is unusable,
	// so it is cleared rather than left to fail every retry.
	if err := os.RemoveAll(s.BinDir); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("embedpg: %w", err)
	}
	if err := os.Rename(tmp, s.BinDir); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("embedpg: %w", err)
	}
	return nil
}

// errEscapesRoot is what every path check in the extractor reports. The archive
// is the only untrusted input in this package.
var errEscapesRoot = errors.New("escapes the extraction root")

// resolveWithin maps dest onto the real path creating it would touch, and
// refuses anything that lands outside root.
//
// The lexical check on an entry NAME is not enough on its own: an earlier entry
// can be a symlink and a later entry's path can walk through it. `a -> .`, then
// `a/b -> ..`, then `a/b/evil` passes every name check and still writes outside
// the tree, because creating a file follows the links. Resolving the deepest
// existing ancestor — and dest itself, when it exists — closes both halves: a
// chain leading out of the root is refused, and the returned path has no
// symlinks in its existing prefix, so a write cannot be redirected either.
//
// A DANGLING symlink is the subtle case: EvalSymlinks reports ErrNotExist for it
// exactly as it does for a component that does not exist yet, and treating the
// two alike hands the link back as a plain name for the kernel to follow. The
// Lstat below separates them. (extractTar's phase ordering means no link exists
// while files are written at all; this is the second, independent layer.)
//
// root must already be resolved; extractTar does that once, because the
// temporary directory is under a symlinked /tmp on macOS.
func resolveWithin(root, dest string) (string, error) {
	probe := dest
	var missing []string
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return "", errEscapesRoot
			}
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return real, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(probe); lerr == nil {
			return "", fmt.Errorf("%w: %s is a dangling symlink", errEscapesRoot, probe)
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return "", lerr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", errEscapesRoot
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

// mkdirParents creates every component of dir under root, refusing to walk
// through anything that is not a real directory. A component that already
// exists as a symlink is an error, never something to follow.
func mkdirParents(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%w: %s", errEscapesRoot, dir)
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", errEscapesRoot, cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("embedpg: %s is not a directory", cur)
		}
	}
	return nil
}

// hasParentComponent reports whether a relative link target climbs with `..`.
func hasParentComponent(link string) bool {
	for _, part := range strings.Split(link, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// pendingSymlink is a symlink entry held back until every directory and file has
// been written.
type pendingSymlink struct {
	name string
	dest string
	link string
}

// extractTar writes every entry of tr under root. Anything that could land
// outside root — an absolute name, a parent-relative name, a symlink chain
// pointing out — aborts the extraction: a partial tree is discarded by the
// caller.
//
// Directories and regular files are written as they arrive; symlinks are
// collected and created only after the archive ends, so while a file is being
// written there is no symlink anywhere under root for the kernel to follow. That
// ordering is DEFENCE IN DEPTH, not the guard a test proves: writeEntry's Lstat
// and O_NOFOLLOW already refuse to write through a link, so creating the links
// inline would leave every test in this package green. It earns its place by
// removing the class of escape rather than by being the check that catches one —
// keep it for that reason, not because some test would notice its absence. A
// link is also refused when it is absolute or when its target climbs with `..`,
// which leaves every link pointing strictly down inside its own directory — a
// chain of those can never ascend.
func extractTar(tr *tar.Reader, root string) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	var links []pendingSymlink
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("embedpg: reading archive: %w", err)
		}

		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("embedpg: archive entry %q %w", hdr.Name, errEscapesRoot)
		}
		if name == "." {
			continue
		}
		dest, err := resolveWithin(root, filepath.Join(root, name))
		if err != nil {
			return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirParents(root, dest); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := mkdirParents(root, filepath.Dir(dest)); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
			if err := writeEntry(dest, os.FileMode(hdr.Mode)&0o777, tr); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("embedpg: archive symlink %q points at the absolute path %q", hdr.Name, hdr.Linkname)
			}
			if hasParentComponent(hdr.Linkname) {
				return fmt.Errorf("embedpg: archive symlink %q %w: %q", hdr.Name, errEscapesRoot, hdr.Linkname)
			}
			links = append(links, pendingSymlink{name: name, dest: dest, link: hdr.Linkname})
		default:
			return fmt.Errorf("embedpg: archive entry %q has unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
	}

	for _, l := range links {
		if err := mkdirParents(root, filepath.Dir(l.dest)); err != nil {
			return fmt.Errorf("embedpg: archive symlink %q: %w", l.name, err)
		}
		if _, err := os.Lstat(l.dest); err == nil {
			return fmt.Errorf("embedpg: archive symlink %q collides with an entry already extracted", l.name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("embedpg: %w", err)
		}
		if err := os.Symlink(l.link, l.dest); err != nil {
			return fmt.Errorf("embedpg: archive symlink %q: %w", l.name, err)
		}
	}
	return nil
}

// writeEntry creates dest with mode and copies the entry's body into it. The
// final component must not be a symlink: O_NOFOLLOW makes the kernel refuse one
// even if it appeared between the check and the open.
func writeEntry(dest string, mode os.FileMode, r io.Reader) error {
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", errEscapesRoot, dest)
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
