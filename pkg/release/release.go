// SPDX-License-Identifier: Apache-2.0

// Package release fetches and verifies rafiki release assets: it resolves a
// tag, verifies checksums.txt against the embedded trusted keys BEFORE it
// requests any archive, and extracts binaries only from an archive whose
// sha256 matches the signed checksum.
//
// It is deliberately dependency-free beyond the standard library and
// pkg/relsign: both the CLI (rafiki upgrade) and the daemon (installing an
// executor on another machine) stage the same verified bytes, and neither may
// drag a database driver into the client.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/relsign"
)

const (
	// DefaultLatestURL is the page whose redirect names the newest tag.
	DefaultLatestURL = "https://github.com/graveland/rafiki/releases/latest"
	// DefaultDownloadBase is the release host; an asset URL is
	// <DownloadBase>/<tag>/<name>.
	DefaultDownloadBase = "https://github.com/graveland/rafiki/releases/download"

	maxChecksumsBytes = 1 << 20
	maxSigBytes       = 4 << 10
	maxBinaryBytes    = 256 << 20
)

// Client resolves a release tag and fetches its assets.
type Client struct {
	LatestURL    string
	DownloadBase string // asset URL is <DownloadBase>/<tag>/<name>
	HTTP         *http.Client
}

// NewClient returns a client for the public release host, or for downloadBase
// when it is non-empty (LatestURL stays the default; callers that set a base
// must pass an explicit tag instead of resolving latest).
func NewClient(downloadBase string) *Client {
	c := &Client{
		LatestURL:    DefaultLatestURL,
		DownloadBase: DefaultDownloadBase,
		HTTP:         &http.Client{Timeout: 10 * time.Minute},
	}
	if downloadBase != "" {
		c.DownloadBase = downloadBase
	}
	return c
}

// ErrAssetMissing is returned by FetchSmall for an HTTP 404.
type ErrAssetMissing struct{ Name string }

func (e ErrAssetMissing) Error() string { return "release asset " + e.Name + " not found" }

// LatestTag resolves the tag of the latest release from the /releases/latest
// redirect, which it does not follow.
func (c *Client) LatestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.LatestURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout:       10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", fmt.Errorf("latest release: unexpected status %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("latest release: redirect has no Location")
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("latest release: bad Location %q: %w", loc, err)
	}
	tag := path.Base(u.Path)
	if _, err := relsign.ParseTag(tag); err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	return tag, nil
}

// FetchSmall GETs <DownloadBase>/<tag>/<name>, reading at most max bytes.
func (c *Client) FetchSmall(ctx context.Context, tag, name string, max int64) ([]byte, error) {
	u := c.DownloadBase + "/" + tag + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrAssetMissing{Name: name}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("GET %s: asset exceeds %d bytes", u, max)
	}
	return data, nil
}

// AssetName is the goreleaser asset name for a tag and target.
func AssetName(tag, goos, goarch string) string {
	return "rafiki_" + strings.TrimPrefix(tag, "v") + "_" + goos + "_" + goarch + ".tar.gz"
}

// ChecksumFor returns the sha256 for name from goreleaser's
// "<64 lowercase hex>  <name>" lines.
func ChecksumFor(checksums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		hash, lineName, ok := strings.Cut(line, "  ")
		if !ok || lineName != name {
			continue
		}
		if !isSHA256Hex(hash) {
			return "", fmt.Errorf("checksums.txt: %s has a malformed sha256 %q", name, hash)
		}
		return hash, nil
	}
	return "", fmt.Errorf("checksums.txt: no checksum for %s", name)
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// releaseTagRE is goreleaser's {{.Tag}} vocabulary: a v-prefixed semver, with
// an optional pre-release suffix.
var releaseTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// IsReleaseTag reports whether v is a release tag as goreleaser names it, so a
// caller can tell a built release from a source build's commit hash.
func IsReleaseTag(v string) bool { return releaseTagRE.MatchString(v) }

// checkWritable proves dir can be written to before anything is downloaded.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".rafiki-write-check.*")
	if err != nil {
		return fmt.Errorf("can't write to %s: %w", dir, err)
	}
	_ = f.Close()
	if err := os.Remove(f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("can't write to %s: %w", dir, err)
	}
	return nil
}

// Staged holds the two binaries a verified release archive was extracted into.
type Staged struct{ Rafiki, Rafikid string }

// Cleanup removes both staged binaries, ignoring a missing file.
func (s Staged) Cleanup() {
	_ = os.Remove(s.Rafiki)
	_ = os.Remove(s.Rafikid)
}

// ExtractBinaries writes the archive's rafiki and rafikid members into destDir
// as rafiki.<random> and rafikid.<random>, mode 0755. Only the members' base
// names are used, so a hostile archive cannot steer a write elsewhere.
func ExtractBinaries(archivePath, destDir string) (Staged, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return Staged{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Staged{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var created []string
	paths := map[string]string{}
	seen := map[string]bool{}
	fail := func(err error) (Staged, error) {
		for _, p := range created {
			_ = os.Remove(p)
		}
		return Staged{}, err
	}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := path.Base(hdr.Name)
		if base != "rafiki" && base != "rafikid" {
			continue
		}
		if seen[base] {
			return fail(fmt.Errorf("duplicate %s in archive", base))
		}
		seen[base] = true

		out, err := os.CreateTemp(destDir, base+".*")
		if err != nil {
			return fail(err)
		}
		created = append(created, out.Name())
		n, err := io.CopyN(out, tr, maxBinaryBytes+1)
		if err != nil && !errors.Is(err, io.EOF) {
			_ = out.Close()
			return fail(err)
		}
		if n > maxBinaryBytes {
			_ = out.Close()
			return fail(fmt.Errorf("archive member %s exceeds %d bytes", base, int64(maxBinaryBytes)))
		}
		if err := out.Chmod(0o755); err != nil {
			_ = out.Close()
			return fail(err)
		}
		if err := out.Close(); err != nil {
			return fail(err)
		}
		paths[base] = out.Name()
	}

	for _, base := range []string{"rafiki", "rafikid"} {
		if !seen[base] {
			return fail(fmt.Errorf("archive lacks %s", base))
		}
	}
	rafikiAbs, err := filepath.Abs(paths["rafiki"])
	if err != nil {
		return fail(err)
	}
	rafikidAbs, err := filepath.Abs(paths["rafikid"])
	if err != nil {
		return fail(err)
	}
	return Staged{Rafiki: rafikiAbs, Rafikid: rafikidAbs}, nil
}

// StageRelease verifies a release's checksums.txt before downloading anything,
// then downloads, hashes and extracts the two binaries. The archive is never
// requested before the signature verifies.
func (c *Client) StageRelease(ctx context.Context, keys []relsign.Key, tag, goos, goarch, destDir string) (Staged, error) {
	if err := checkWritable(destDir); err != nil {
		return Staged{}, err
	}

	name, want, err := c.verifyChecksums(ctx, keys, tag, goos, goarch)
	if err != nil {
		return Staged{}, err
	}

	archivePath, cleanup, err := c.fetchArchive(ctx, tag, name, want, destDir)
	if err != nil {
		return Staged{}, err
	}
	defer cleanup()

	return ExtractBinaries(archivePath, destDir)
}

// StageBinary verifies a release exactly as StageRelease does — checksums.txt
// and its signature are fetched and verified BEFORE the archive is requested,
// and the archive's sha256 must match the signed checksum — then extracts only
// the archive member whose base name is name to destPath, mode 0755, and
// returns that binary's sha256. It is what installs one binary on a machine
// that does not need the rest of the release.
func (c *Client) StageBinary(ctx context.Context, keys []relsign.Key, tag, goos, goarch, name, destPath string) (string, error) {
	dir := filepath.Dir(destPath)
	if err := checkWritable(dir); err != nil {
		return "", err
	}

	asset, want, err := c.verifyChecksums(ctx, keys, tag, goos, goarch)
	if err != nil {
		return "", err
	}

	archivePath, cleanup, err := c.fetchArchive(ctx, tag, asset, want, dir)
	if err != nil {
		return "", err
	}
	defer cleanup()

	sum, found, err := extractMember(archivePath, name, destPath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("release %s %s/%s has no %s binary", tag, goos, goarch, name)
	}
	return sum, nil
}

// verifyChecksums fetches checksums.txt and its signature and verifies the
// signature against keys for tag. It returns the platform archive's name and
// the sha256 the signed checksums list for it. Nothing else is requested until
// this succeeds.
func (c *Client) verifyChecksums(ctx context.Context, keys []relsign.Key, tag, goos, goarch string) (name, want string, err error) {
	checksums, err := c.FetchSmall(ctx, tag, "checksums.txt", maxChecksumsBytes)
	if err != nil {
		return "", "", err
	}
	sig, err := c.FetchSmall(ctx, tag, "checksums.txt.sig", maxSigBytes)
	if err != nil {
		var missing ErrAssetMissing
		if errors.As(err, &missing) {
			return "", "", errors.New("this release has no checksums.txt.sig; it predates signed releases, so install it manually (see docs/guide/install.md)")
		}
		return "", "", err
	}
	if err := relsign.Verify(keys, checksums, sig, tag); err != nil {
		return "", "", fmt.Errorf("verify checksums.txt: %w", err)
	}

	name = AssetName(tag, goos, goarch)
	want, err = ChecksumFor(checksums, name)
	if err != nil {
		return "", "", err
	}
	return name, want, nil
}

// fetchArchive downloads the verified release asset into a temp file in dir and
// checks its sha256 against want. It returns the temp path and a cleanup that
// removes it.
func (c *Client) fetchArchive(ctx context.Context, tag, name, want, dir string) (string, func(), error) {
	archive, err := os.CreateTemp(dir, ".rafiki-archive.*")
	if err != nil {
		return "", nil, err
	}
	archivePath := archive.Name()
	cleanup := func() { _ = os.Remove(archivePath) }

	assetURL := c.DownloadBase + "/" + tag + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		_ = archive.Close()
		cleanup()
		return "", nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		_ = archive.Close()
		cleanup()
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = archive.Close()
		cleanup()
		return "", nil, fmt.Errorf("GET %s: %s", assetURL, resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(archive, h), resp.Body); err != nil {
		_ = archive.Close()
		cleanup()
		return "", nil, err
	}
	if err := archive.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		cleanup()
		return "", nil, fmt.Errorf("archive %s sha256 = %s, want %s", name, got, want)
	}
	return archivePath, cleanup, nil
}

// extractMember writes the archive's first regular-file member whose base name
// is name to destPath: a temp file in destPath's directory, fsynced, mode 0755,
// renamed into place. It returns the written bytes' sha256 and whether the
// member was found. Only the member's base name is used, so a hostile archive
// cannot steer the write elsewhere, and the archive's own sha256 was already
// checked against the signed checksum before this is called.
func extractMember(archivePath, name, destPath string) (string, bool, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", false, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != name {
			continue
		}
		sum, err := writeMember(tr, destPath)
		if err != nil {
			return "", false, err
		}
		return sum, true, nil
	}
}

// writeMember copies one tar member to destPath through a temp file in the same
// directory, fsynced and renamed, and returns the sha256 of the bytes written.
func writeMember(r io.Reader, destPath string) (string, error) {
	out, err := os.CreateTemp(filepath.Dir(destPath), "."+filepath.Base(destPath)+".*")
	if err != nil {
		return "", err
	}
	tmp := out.Name()
	fail := func(err error) (string, error) {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", err
	}

	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(out, h), r, maxBinaryBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fail(err)
	}
	if n > maxBinaryBytes {
		return fail(fmt.Errorf("archive member %s exceeds %d bytes", filepath.Base(destPath), int64(maxBinaryBytes)))
	}
	if err := out.Chmod(0o755); err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
