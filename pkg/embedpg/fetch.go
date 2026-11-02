package embedpg

import (
	"archive/tar"
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
	// Version names the artifact build: the PostgreSQL release and the build
	// revision (build/embedpg/versions.env: PG_VERSION-BUILD_REV). The revision
	// moves when only the extensions or bundled libraries change, because Default
	// keys the extracted tree by this string. It moves only together with the
	// sha256 digests below: a bump without re-pinned digests fails every
	// download closed.
	Version = "18.6-1"
	// MajorVersion is what a data directory must report in PG_VERSION. A data
	// directory from another major cannot be opened by these binaries, and
	// pg_upgrade is not something this package does.
	MajorVersion = "18"
	// DefaultBaseURL is where the embedpg-v<Version> release assets are
	// published (.github/workflows/embedpg.yml).
	DefaultBaseURL = "https://github.com/graveland/rafiki/releases/download"
	// Port is the TCP port the server is configured for. It never listens on
	// TCP (listen_addresses is empty); the port only names the unix socket
	// file, .s.PGSQL.<Port>, inside the data directory.
	Port = 5432
)

// artifact is one platform's build of the server tree: the release asset's file
// name and the digest of that file.
type artifact struct {
	file   string // release asset name, a .txz of the extracted tree
	sha256 string // hex sha256 of the downloaded file
}

// darwin ships one universal (x86_64 + arm64) tree for both architectures.
var artifacts = map[string]artifact{
	"darwin/arm64": {"postgres-darwin-universal.txz", "cfdb27871e5ad87e5a69be1e4d62c15a6158dad553e634bbbcb9451d00ec70ed"},
	"darwin/amd64": {"postgres-darwin-universal.txz", "cfdb27871e5ad87e5a69be1e4d62c15a6158dad553e634bbbcb9451d00ec70ed"},
	"linux/amd64":  {"postgres-linux-amd64.txz", "14daf3936b0bc2ee00e220cc750647e96eaf3b3b04665b1f6f9d1dae24386c14"},
	"linux/arm64":  {"postgres-linux-arm64.txz", "519d4417a4a8d65ac2bfba34a4679798704cbcd7589e3161bb9f04e0be5b7abe"},
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

// assetURL is the download URL of a's release asset under base.
func assetURL(base string, a artifact) string {
	return base + "/embedpg-v" + Version + "/" + a.file
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
	// partial download nor a partial tree can be resumed, and nothing else ever
	// removes them.
	if stale, err := filepath.Glob(filepath.Join(dir, "embedpg-*")); err == nil {
		for _, path := range stale {
			os.RemoveAll(path)
		}
	}

	url := assetURL(s.baseURL(), a)
	if s.Progress != nil {
		fmt.Fprintf(s.Progress, "downloading PostgreSQL %s (%s)...\n", Version, a.file)
	}
	archive, digest, err := s.download(ctx, url, dir)
	if err != nil {
		return err
	}
	if digest != a.sha256 {
		os.Remove(archive)
		return fmt.Errorf("embedpg: %s: sha256 mismatch: want %s, got %s", url, a.sha256, digest)
	}
	if s.Progress != nil {
		fmt.Fprintf(s.Progress, "verified sha256 %s\n", digest)
	}
	if err := s.extract(archive, url); err != nil {
		os.Remove(archive)
		return err
	}
	// The tree is what this function is for, and it is complete: an archive that
	// will not go away is debris the next run's embedpg-* sweep collects, not a
	// failed install.
	_ = os.Remove(archive)
	return nil
}

// maxDownloadBytes bounds one archive download. The pinned artifacts are 17-70
// MB; the limit is generous enough for a future build and small enough that a
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

	f, err := os.CreateTemp(dir, "embedpg-*.txz")
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

// extract unpacks the .txz at archivePath into BinDir, atomically: the tree is
// built in a sibling temporary directory and renamed into place, so an
// interrupted extraction leaves no half-populated BinDir to be mistaken for a
// complete one.
func (s Server) extract(archivePath, url string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		return fmt.Errorf("embedpg: %s: %w", url, err)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(s.BinDir), "embedpg-extract-*")
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	if err := ExtractTar(xr, tmp); err != nil {
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
// is the only untrusted input in this package — and the reason ExtractTar is
// exported, because `rafikid db restore` unpacks an archive just as untrusted.
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
// Lstat below separates them. (ExtractTar's phase ordering means no link exists
// while files are written at all; this is the second, independent layer.)
//
// root must already be resolved; ExtractTar does that once, because the
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
// `rafikid db backup` applies the same test before it writes a link, so an
// archive it produces never carries one this refuses.
func hasParentComponent(link string) bool {
	for _, part := range strings.Split(link, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// entryMode narrows an entry's recorded permission bits to def when it recorded
// none. A zero-mode file is unreadable and a zero-mode directory unusable, so an
// archive that lost the bits (or never had them) gets the private default rather
// than a tree nothing can use.
func entryMode(mode int64, def os.FileMode) os.FileMode {
	if m := os.FileMode(mode) & os.ModePerm; m != 0 {
		return m
	}
	return def
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

// pendingSymlink is a symlink entry held back until every directory and file has
// been written.
type pendingSymlink struct {
	name string
	dest string
	link string
}

// pendingDir is a directory whose recorded mode is applied once the whole
// archive has been written. Directories are created with a mode anything under
// them can be written into, so an entry recorded as read-only does not make the
// entries under it unwritable — the same reason the links wait.
type pendingDir struct {
	name string
	dest string
	mode os.FileMode
}

// ExtractTar writes every entry of the tar stream r under root, keeping each
// entry's file type, symlink target and permission bits. Anything that could
// land outside root — an absolute name, a parent-relative name, a symlink chain
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
//
// Directory modes are held back the same way and applied after the links, so a
// directory the archive recorded as read-only is not made read-only before the
// entries under it have been written.
//
// It is EXPORTED because every archive this project unpacks is untrusted: the
// downloaded build above is one, and `rafikid db restore`'s physical archive is
// another. One implementation is what keeps the second from being a copy that
// drifts from the first.
func ExtractTar(r io.Reader, root string) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	tr := tar.NewReader(r)
	var links []pendingSymlink
	var dirs []pendingDir
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
			dirs = append(dirs, pendingDir{name: name, dest: dest, mode: entryMode(hdr.Mode, 0o700)})
		case tar.TypeReg:
			if err := mkdirParents(root, filepath.Dir(dest)); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
			if err := writeEntry(dest, entryMode(hdr.Mode, 0o600), tr); err != nil {
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
			return fmt.Errorf("embedpg: archive entry %q is %s; only regular files, directories and "+
				"symlinks can be extracted", hdr.Name, tarEntryKind(hdr.Typeflag))
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

	for _, d := range dirs {
		if err := os.Chmod(d.dest, d.mode); err != nil {
			return fmt.Errorf("embedpg: archive directory %q: %w", d.name, err)
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
