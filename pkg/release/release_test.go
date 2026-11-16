// SPDX-License-Identifier: Apache-2.0

package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/relsign"
)

type testReleaseOpts struct {
	BadSignature bool              // sign checksums.txt with a key that is NOT in Keys
	ChecksumLies bool              // signed checksums.txt lists a different sha256 for the archive
	NoSignature  bool              // checksums.txt.sig answers 404
	MaxVersion   string            // set as Keys[0].MaxVersion
	ExtraMembers map[string]string // extra tar members, name -> body (names may contain "../")
	OmitRafikid  bool
}

type testRelease struct {
	Server   *httptest.Server
	Keys     []relsign.Key // the trusted list matching the signature
	Client   *Client
	Archive  string                  // asset name
	Requests func(suffix string) int // number of requests whose URL path ends with suffix
}

type releaseTarMember struct {
	Name     string
	Typeflag byte
	Body     string
	Linkname string
}

// releaseTarGz builds a gzip-compressed tar from members, mode 0755.
func releaseTarGz(t *testing.T, members []releaseTarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		tf := m.Typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     m.Name,
			Mode:     0o755,
			Size:     int64(len(m.Body)),
			Typeflag: tf,
			Linkname: m.Linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader %s: %v", m.Name, err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write([]byte(m.Body)); err != nil {
				t.Fatalf("tar Write %s: %v", m.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func buildArchive(t *testing.T, tag string, o testReleaseOpts) []byte {
	t.Helper()
	members := []releaseTarMember{
		{Name: "rafiki", Body: "#!/bin/sh\necho \"rafiki version " + tag + "\"\n"},
	}
	if !o.OmitRafikid {
		members = append(members, releaseTarMember{Name: "rafikid", Body: "#!/bin/sh\necho \"rafikid " + tag + "\"\n"})
	}
	for name, body := range o.ExtraMembers {
		members = append(members, releaseTarMember{Name: name, Body: body})
	}
	return releaseTarGz(t, members)
}

func newTestRelease(t *testing.T, tag, goos, goarch string, o testReleaseOpts) *testRelease {
	t.Helper()

	seed, pub, err := relsign.GenerateKey()
	if err != nil {
		t.Fatalf("relsign.GenerateKey: %v", err)
	}
	priv, err := relsign.PrivateFromSeed(seed)
	if err != nil {
		t.Fatalf("relsign.PrivateFromSeed: %v", err)
	}
	signKey := priv
	if o.BadSignature {
		badSeed, _, err := relsign.GenerateKey()
		if err != nil {
			t.Fatalf("relsign.GenerateKey (bad): %v", err)
		}
		bad, err := relsign.PrivateFromSeed(badSeed)
		if err != nil {
			t.Fatalf("relsign.PrivateFromSeed (bad): %v", err)
		}
		signKey = bad
	}
	keys := []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub, MaxVersion: o.MaxVersion}}

	archive := buildArchive(t, tag, o)
	asset := AssetName(tag, goos, goarch)
	sum := sha256.Sum256(archive)
	if o.ChecksumLies {
		sum = sha256.Sum256([]byte("not the archive"))
	}
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	sig := relsign.Sign(signKey, checksums)

	var mu sync.Mutex
	counts := map[string]int{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/latest":
			w.Header().Set("Location", srv.URL+"/tag/"+tag)
			w.WriteHeader(http.StatusFound)
		case "/" + tag + "/" + asset:
			_, _ = w.Write(archive)
		case "/" + tag + "/checksums.txt":
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				w.Header().Set("Content-Encoding", "gzip")
				gz := gzip.NewWriter(w)
				_, _ = gz.Write(checksums)
				_ = gz.Close()
				return
			}
			_, _ = w.Write(checksums)
		case "/" + tag + "/checksums.txt.sig":
			if o.NoSignature {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	requests := func(suffix string) int {
		mu.Lock()
		defer mu.Unlock()
		total := 0
		for p, n := range counts {
			if strings.HasSuffix(p, suffix) {
				total += n
			}
		}
		return total
	}

	return &testRelease{
		Server:   srv,
		Keys:     keys,
		Client:   &Client{LatestURL: srv.URL + "/latest", DownloadBase: srv.URL, HTTP: srv.Client()},
		Archive:  asset,
		Requests: requests,
	}
}

func TestLatestTagParsesRedirect(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})

	got, err := tr.Client.LatestTag(context.Background())
	c.Require().NoError(err, "LatestTag")
	c.Eq("v0.2.0", got, "LatestTag")
}

func TestLatestTagRejectsNonTag(t *testing.T) {
	c := assert.NewCollecting(t)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", srv.URL+"/tag/nightly")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	client := &Client{LatestURL: srv.URL + "/latest", DownloadBase: srv.URL, HTTP: srv.Client()}

	_, err := client.LatestTag(context.Background())
	c.Error(err, "LatestTag on a non-tag redirect")
}

func TestLatestTagRejectsNoRedirect(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	client := &Client{LatestURL: srv.URL + "/latest", DownloadBase: srv.URL, HTTP: srv.Client()}

	_, err := client.LatestTag(context.Background())
	c.Error(err, "LatestTag on a 200")
}

func TestFetchSmallDecodesGzipContentEncoding(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})

	got, err := tr.Client.FetchSmall(context.Background(), "v0.2.0", "checksums.txt", maxChecksumsBytes)
	c.Require().NoError(err, "FetchSmall")
	c.StrContains(string(got), tr.Archive, "FetchSmall must return the decompressed checksums")
	c.StrContains(string(got), "  ", "FetchSmall must return the plaintext checksums, not gzip bytes")
}

func TestFetchSmallMissingAsset(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})

	_, err := tr.Client.FetchSmall(context.Background(), "v0.2.0", "nope.txt", maxChecksumsBytes)
	c.Require().Error(err, "FetchSmall of a missing asset")
	var missing ErrAssetMissing
	c.True(errors.As(err, &missing), "error %v must be an ErrAssetMissing", err)
}

func TestFetchSmallRejectsOversize(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})

	_, err := tr.Client.FetchSmall(context.Background(), "v0.2.0", "checksums.txt", 4)
	c.Error(err, "FetchSmall past the cap")
}

func TestChecksumForParsesGoreleaserFormat(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(
		"1111111111111111111111111111111111111111111111111111111111111111  other.tar.gz\n" +
			"\n" +
			"2222222222222222222222222222222222222222222222222222222222222222  rafiki_0.2.0_linux_amd64.tar.gz\n")

	got, err := ChecksumFor(body, "rafiki_0.2.0_linux_amd64.tar.gz")
	c.Require().NoError(err, "ChecksumFor")
	c.Eq("2222222222222222222222222222222222222222222222222222222222222222", got, "ChecksumFor")

	_, err = ChecksumFor(body, "absent.tar.gz")
	c.Error(err, "ChecksumFor of an absent name")

	short := []byte("333333333333333333333333333333333333333333333333333333333333333  rafiki_0.2.0_linux_amd64.tar.gz\n")
	_, err = ChecksumFor(short, "rafiki_0.2.0_linux_amd64.tar.gz")
	c.Error(err, "ChecksumFor of a 63-character hash")
}

func TestStageReleaseHappyPath(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})
	dest := t.TempDir()

	staged, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().NoError(err, "StageRelease")
	defer staged.Cleanup()

	entries, err := os.ReadDir(dest)
	c.Require().NoError(err, "ReadDir")
	c.Len(entries, 2, "destination must hold exactly the two binaries")

	byName := map[string]string{}
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "rafiki."):
			byName["rafiki"] = e.Name()
		case strings.HasPrefix(e.Name(), "rafikid."):
			byName["rafikid"] = e.Name()
		}
	}
	c.StrContains(filepath.Base(staged.Rafiki), "rafiki.", "staged rafiki path")
	c.StrContains(filepath.Base(staged.Rafikid), "rafikid.", "staged rafikid path")

	for base, want := range map[string]string{
		"rafiki":  "#!/bin/sh\necho \"rafiki version v0.2.0\"\n",
		"rafikid": "#!/bin/sh\necho \"rafikid v0.2.0\"\n",
	} {
		name, ok := byName[base]
		c.True(ok, "destination must hold a %s.<random> file", base)
		p := filepath.Join(dest, name)
		fi, err := os.Stat(p)
		c.Require().NoError(err, "Stat %s", name)
		c.Eq(os.FileMode(0o755), fi.Mode().Perm(), "%s mode", name)
		got, err := os.ReadFile(p)
		c.Require().NoError(err, "ReadFile %s", name)
		c.Eq(want, string(got), "%s body", name)
	}
}

func TestStageReleaseRejectsBadSignature(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{BadSignature: true})
	dest := t.TempDir()

	_, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().Error(err, "StageRelease with a bad signature")
	c.ErrorIs(err, relsign.ErrUnknownKey, "StageRelease error")
	c.Len(dirEntries(t, dest), 0, "nothing extracted")
	c.Eq(0, tr.Requests(".tar.gz"), "the archive must never be requested when the signature fails")
}

func TestStageReleaseRejectsKeyBoundBelowTag(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{MaxVersion: "v0.1.0"})
	dest := t.TempDir()

	_, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().Error(err, "StageRelease with a key bound below the tag")
	c.ErrorIs(err, relsign.ErrKeyNotValidForTag, "StageRelease error")
	c.Eq(0, tr.Requests(".tar.gz"), "the archive must never be requested when the key is not valid for the tag")
}

func TestStageReleaseRejectsChecksumMismatch(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{ChecksumLies: true})
	dest := t.TempDir()

	_, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().Error(err, "StageRelease with a lying checksum")
	c.StrContains(err.Error(), "sha256", "the error must name the hashes")
	c.Len(dirEntries(t, dest), 0, "nothing extracted")
}

func TestStageReleaseMissingSignatureSaysSo(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{NoSignature: true})
	dest := t.TempDir()

	_, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().Error(err, "StageRelease without a signature")
	c.StrContains(err.Error(), "no checksums.txt.sig", "the error must explain the missing signature")
	c.Eq(0, tr.Requests(".tar.gz"), "the archive must never be requested without a signature")
}

func TestStageReleaseUnwritableDestStopsBeforeNetwork(t *testing.T) {
	c := assert.NewCollecting(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})
	dest := t.TempDir()
	c.Require().NoError(os.Chmod(dest, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dest, 0o700) })

	_, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().Error(err, "StageRelease into an unwritable destination")
	c.StrContains(err.Error(), "can't write to", "the error must name the destination")
	c.Eq(0, tr.Requests(""), "nothing may be requested before the destination is proven writable")
}

// TestStageBinaryVerifiesBeforeDownloadingTheArchive pins the verify-first
// order on the one-binary path: a signature that does not verify must stop
// before the archive is requested, exactly as StageRelease does.
func TestStageBinaryVerifiesBeforeDownloadingTheArchive(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{BadSignature: true})
	dir := t.TempDir()
	dest := filepath.Join(dir, "rafikid")

	_, err := tr.Client.StageBinary(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", "rafikid", dest)
	c.Require().Error(err, "StageBinary with a bad signature")
	c.StrContains(err.Error(), "verify checksums.txt", "the error must name the verification that failed")
	c.Eq(0, tr.Requests(".tar.gz"), "the archive must never be requested when the signature fails")
	c.Eq(0, len(dirEntries(t, dir)), "nothing may be written")
}

// TestStageBinaryExtractsOnlyTheNamedMember pins the narrow extraction: one
// member, at the caller's path, with the archive's mode, and the sha256 of the
// bytes actually written.
func TestStageBinaryExtractsOnlyTheNamedMember(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})
	dir := t.TempDir()
	dest := filepath.Join(dir, "bin", "rafikid")
	c.Require().NoError(os.MkdirAll(filepath.Dir(dest), 0o755))

	sum, err := tr.Client.StageBinary(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", "rafikid", dest)
	c.Require().NoError(err, "StageBinary")

	body := "#!/bin/sh\necho \"rafikid v0.2.0\"\n"
	got, err := os.ReadFile(dest)
	c.Require().NoError(err, "ReadFile")
	c.Eq(body, string(got), "the extracted member's body")

	want := sha256.Sum256([]byte(body))
	c.Eq(hex.EncodeToString(want[:]), sum, "StageBinary must return the extracted binary's sha256")

	fi, err := os.Stat(dest)
	c.Require().NoError(err, "Stat")
	c.Eq(os.FileMode(0o755), fi.Mode().Perm(), "the extracted binary's mode")

	c.EqDiff([]string{"rafikid"}, dirNames(t, filepath.Dir(dest)),
		"only the named member may be written, and no temp file may remain")
	_, err = os.Stat(filepath.Join(dir, "rafiki"))
	c.True(errors.Is(err, os.ErrNotExist), "the other member must not be extracted")
}

func TestStageBinaryMissingMember(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{})
	dir := t.TempDir()

	_, err := tr.Client.StageBinary(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", "rafiki-hooks", filepath.Join(dir, "rafiki-hooks"))
	c.Require().Error(err, "StageBinary of an absent member")
	c.Eq("release v0.2.0 linux/amd64 has no rafiki-hooks binary", err.Error(), "the missing-member error")
	c.Len(dirEntries(t, dir), 0, "nothing may be written")
}

func TestIsReleaseTag(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, v := range []string{"v0.5.16", "v0.5.16-rc.1", "v1.0.0-alpha.2"} {
		c.True(IsReleaseTag(v), "%q must be a release tag", v)
	}
	for _, v := range []string{"bc8d0c3", "bc8d0c3-dirty", "unknown", "0.5.16", "v0.5", "v0.5.16.1", "", "vv0.5.16"} {
		c.False(IsReleaseTag(v), "%q must not be a release tag", v)
	}
}

func TestExtractIgnoresOtherMembersAndTraversal(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{
		ExtraMembers: map[string]string{"../evil": "x", "etc/passwd": "y"},
	})
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "dest")
	c.Require().NoError(os.MkdirAll(dest, 0o755))

	staged, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().NoError(err, "StageRelease")
	defer staged.Cleanup()

	top, err := os.ReadDir(tmp)
	c.Require().NoError(err, "ReadDir tmp")
	c.Len(top, 1, "the parent must hold only dest")
	c.Eq("dest", top[0].Name(), "the only parent entry")
	c.Len(dirEntries(t, dest), 2, "dest must hold only the two binaries")
}

func TestExtractRequiresBothBinaries(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{OmitRafikid: true})
	dest := t.TempDir()

	_, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().Error(err, "StageRelease without rafikid")
	c.StrContains(err.Error(), "archive lacks rafikid", "the error must name the missing binary")
	c.Len(dirEntries(t, dest), 0, "nothing extracted")
}

func TestExtractRejectsDuplicateMember(t *testing.T) {
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", "linux", "amd64", testReleaseOpts{
		ExtraMembers: map[string]string{"x/rafiki": "x"},
	})
	dest := t.TempDir()

	_, err := tr.Client.StageRelease(context.Background(), tr.Keys, "v0.2.0", "linux", "amd64", dest)
	c.Require().Error(err, "StageRelease with a duplicate rafiki")
	c.StrContains(err.Error(), "duplicate rafiki", "the error must name the duplicate")
	c.Len(dirEntries(t, dest), 0, "nothing extracted")
}

func TestExtractSkipsSymlinkMember(t *testing.T) {
	c := assert.NewCollecting(t)
	archive := releaseTarGz(t, []releaseTarMember{
		{Name: "rafiki", Body: "#!/bin/sh\necho rafiki\n"},
		{Name: "rafikid", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	})
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	c.Require().NoError(os.WriteFile(archivePath, archive, 0o644))
	dest := filepath.Join(dir, "dest")
	c.Require().NoError(os.MkdirAll(dest, 0o755))

	_, err := ExtractBinaries(archivePath, dest)
	c.Require().Error(err, "ExtractBinaries whose only rafikid is a symlink")
	c.StrContains(err.Error(), "archive lacks rafikid", "the error must name the missing binary")
	c.Len(dirEntries(t, dest), 0, "nothing extracted")
}

func TestNewReleaseClientDefaults(t *testing.T) {
	c := assert.NewCollecting(t)

	def := NewClient("")
	c.Eq(DefaultLatestURL, def.LatestURL, "default latest URL")
	c.Eq(DefaultDownloadBase, def.DownloadBase, "default download base")
	c.NotNil(def.HTTP, "default HTTP client")

	override := NewClient("http://127.0.0.1:1/dl")
	c.Eq(DefaultLatestURL, override.LatestURL, "an override keeps the default latest URL")
	c.Eq("http://127.0.0.1:1/dl", override.DownloadBase, "override download base")
}

func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	return entries
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries := dirEntries(t, dir)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// TestReleaseStageBinary and TestReleaseIsReleaseTag are shims, not duplicate
// coverage: their subtests ARE the pinned test bodies above. The verify
// command's `-run` pattern selects `TestRelease`, and those bodies' names
// predate the pattern, so without these prefixes the pattern would silently
// skip everything this file exists to pin.
func TestReleaseStageBinary(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"verifies before downloading the archive", TestStageBinaryVerifiesBeforeDownloadingTheArchive},
		{"extracts only the named member", TestStageBinaryExtractsOnlyTheNamedMember},
		{"missing member", TestStageBinaryMissingMember},
	} {
		t.Run(tc.name, tc.fn)
	}
}

func TestReleaseIsReleaseTag(t *testing.T) {
	t.Run("tag vocabulary", TestIsReleaseTag)
}

// TestReleaseClient is the same shim for the helpers moved here from
// cmd/rafiki: the bodies are the originals, so the verify pattern exercises
// the moved code rather than trusting that it still compiles.
func TestReleaseClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"latest tag parses a redirect", TestLatestTagParsesRedirect},
		{"latest tag rejects a non-tag", TestLatestTagRejectsNonTag},
		{"latest tag rejects no redirect", TestLatestTagRejectsNoRedirect},
		{"fetch small decodes gzip", TestFetchSmallDecodesGzipContentEncoding},
		{"fetch small missing asset", TestFetchSmallMissingAsset},
		{"fetch small rejects oversize", TestFetchSmallRejectsOversize},
		{"checksum for goreleaser format", TestChecksumForParsesGoreleaserFormat},
		{"extract ignores other members and traversal", TestExtractIgnoresOtherMembersAndTraversal},
		{"extract requires both binaries", TestExtractRequiresBothBinaries},
		{"extract rejects a duplicate member", TestExtractRejectsDuplicateMember},
		{"extract skips a symlink member", TestExtractSkipsSymlinkMember},
		{"new client defaults", TestNewReleaseClientDefaults},
	} {
		t.Run(tc.name, tc.fn)
	}
}
