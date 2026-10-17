// SPDX-License-Identifier: Apache-2.0

package main

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
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/relsign"
)

const (
	defaultLatestURL    = "https://github.com/graveland/rafiki/releases/latest"
	defaultDownloadBase = "https://github.com/graveland/rafiki/releases/download"
	maxChecksumsBytes   = 1 << 20
	maxSigBytes         = 4 << 10
	maxBinaryBytes      = 256 << 20
)

// releaseClient resolves a release tag and fetches its assets.
type releaseClient struct {
	LatestURL    string
	DownloadBase string // asset URL is <DownloadBase>/<tag>/<name>
	HTTP         *http.Client
}

// newReleaseClient returns a client for the public release host, or for
// downloadBase when it is non-empty (LatestURL stays the default; callers that
// set a base must pass an explicit tag instead of resolving latest).
func newReleaseClient(downloadBase string) *releaseClient {
	c := &releaseClient{
		LatestURL:    defaultLatestURL,
		DownloadBase: defaultDownloadBase,
		HTTP:         &http.Client{Timeout: 10 * time.Minute},
	}
	if downloadBase != "" {
		c.DownloadBase = downloadBase
	}
	return c
}

// errAssetMissing is returned by fetchSmall for an HTTP 404.
type errAssetMissing struct{ Name string }

func (e errAssetMissing) Error() string { return "release asset " + e.Name + " not found" }

// latestTag resolves the tag of the latest release from the /releases/latest
// redirect, which it does not follow.
func (c *releaseClient) latestTag(ctx context.Context) (string, error) {
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

// fetchSmall GETs <DownloadBase>/<tag>/<name>, reading at most max bytes.
func (c *releaseClient) fetchSmall(ctx context.Context, tag, name string, max int64) ([]byte, error) {
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
		return nil, errAssetMissing{Name: name}
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

// assetName is the goreleaser asset name for a tag and target.
func assetName(tag, goos, goarch string) string {
	return "rafiki_" + strings.TrimPrefix(tag, "v") + "_" + goos + "_" + goarch + ".tar.gz"
}

// checksumFor returns the sha256 for name from goreleaser's
// "<64 lowercase hex>  <name>" lines.
func checksumFor(checksums []byte, name string) (string, error) {
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

// extractBinaries writes the archive's rafiki and rafikid members into destDir
// as rafiki.<random> and rafikid.<random>, mode 0755. Only the members' base
// names are used, so a hostile archive cannot steer a write elsewhere.
func extractBinaries(archivePath, destDir string) (stagedRelease, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return stagedRelease{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return stagedRelease{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var created []string
	paths := map[string]string{}
	seen := map[string]bool{}
	fail := func(err error) (stagedRelease, error) {
		for _, p := range created {
			_ = os.Remove(p)
		}
		return stagedRelease{}, err
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
	return stagedRelease{Rafiki: rafikiAbs, Rafikid: rafikidAbs}, nil
}

// smokeCheck runs the staged binary and requires it to print wantTag.
func smokeCheck(ctx context.Context, r runner, binary, wantTag string) error {
	stdout, _, err := r.Run(ctx, nil, binary, "--version")
	if err == nil {
		for _, field := range strings.Fields(string(stdout)) {
			if field == wantTag {
				return nil
			}
		}
	}
	reports := fmt.Errorf("%s reports %q, want %s", binary, strings.TrimSpace(string(stdout)), wantTag)
	if err != nil {
		return fmt.Errorf("%w: %w", reports, err)
	}
	return reports
}

type stagedRelease struct{ Rafiki, Rafikid string }

// cleanup removes both staged binaries, ignoring a missing file.
func (s stagedRelease) cleanup() {
	_ = os.Remove(s.Rafiki)
	_ = os.Remove(s.Rafikid)
}

// stageRelease verifies a release's checksums.txt before downloading anything,
// then downloads, hashes and extracts the two binaries. The archive is never
// requested before the signature verifies.
func (c *releaseClient) stageRelease(ctx context.Context, keys []relsign.Key, tag, goos, goarch, destDir string) (stagedRelease, error) {
	if err := checkWritable(destDir); err != nil {
		return stagedRelease{}, err
	}

	checksums, err := c.fetchSmall(ctx, tag, "checksums.txt", maxChecksumsBytes)
	if err != nil {
		return stagedRelease{}, err
	}
	sig, err := c.fetchSmall(ctx, tag, "checksums.txt.sig", maxSigBytes)
	if err != nil {
		var missing errAssetMissing
		if errors.As(err, &missing) {
			return stagedRelease{}, errors.New("this release has no checksums.txt.sig; it predates signed releases, so install it manually (see the README)")
		}
		return stagedRelease{}, err
	}
	if err := relsign.Verify(keys, checksums, sig, tag); err != nil {
		return stagedRelease{}, fmt.Errorf("verify checksums.txt: %w", err)
	}

	name := assetName(tag, goos, goarch)
	want, err := checksumFor(checksums, name)
	if err != nil {
		return stagedRelease{}, err
	}

	archive, err := os.CreateTemp(destDir, ".rafiki-archive.*")
	if err != nil {
		return stagedRelease{}, err
	}
	archivePath := archive.Name()
	defer func() { _ = os.Remove(archivePath) }()

	assetURL := c.DownloadBase + "/" + tag + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		_ = archive.Close()
		return stagedRelease{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		_ = archive.Close()
		return stagedRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = archive.Close()
		return stagedRelease{}, fmt.Errorf("GET %s: %s", assetURL, resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(archive, h), resp.Body); err != nil {
		_ = archive.Close()
		return stagedRelease{}, err
	}
	if err := archive.Close(); err != nil {
		return stagedRelease{}, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return stagedRelease{}, fmt.Errorf("archive %s sha256 = %s, want %s", name, got, want)
	}

	return extractBinaries(archivePath, destDir)
}
