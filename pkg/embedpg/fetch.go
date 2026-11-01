package embedpg

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

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
	return os.Remove(jar)
}

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

	f, err := os.CreateTemp(dir, "embedpg-*.jar")
	if err != nil {
		return "", "", fmt.Errorf("embedpg: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", "", fmt.Errorf("embedpg: %s: %w", url, err)
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

// extractTar writes every entry of tr under root. Anything that could land
// outside root — an absolute name, a parent-relative name, a symlink pointing
// out — aborts the extraction: the archive is the only untrusted input in this
// package, and a partial tree is discarded by the caller.
func extractTar(tr *tar.Reader, root string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("embedpg: reading archive: %w", err)
		}

		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("embedpg: archive entry %q escapes the extraction root", hdr.Name)
		}
		if name == "." {
			continue
		}
		dest := filepath.Join(root, name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
			if err := writeEntry(dest, os.FileMode(hdr.Mode)&0o777, tr); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("embedpg: archive symlink %q points at the absolute path %q", hdr.Name, hdr.Linkname)
			}
			rel := filepath.Clean(filepath.Join(filepath.Dir(name), hdr.Linkname))
			if rel == ".." || strings.HasPrefix(rel, "../") {
				return fmt.Errorf("embedpg: archive symlink %q escapes the extraction root: %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return fmt.Errorf("embedpg: archive entry %q: %w", hdr.Name, err)
			}
		default:
			return fmt.Errorf("embedpg: archive entry %q has unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
	}
}

// writeEntry creates dest with mode and copies the entry's body into it.
func writeEntry(dest string, mode os.FileMode, r io.Reader) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
