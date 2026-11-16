// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/release"
	"go.graveland.dev/rafiki/pkg/relsign"
)

// depotRelease is a fake release host: a signed archive holding one `rafiki`
// member, plus a count of how many times the archive itself was requested. The
// verification logic is pkg/release's and is tested there; what these tests
// need is a host that can be signed correctly or wrongly, and that records
// whether the archive was fetched at all.
type depotRelease struct {
	Server     *httptest.Server
	Keys       []relsign.Key
	Archive    string
	archiveGet func() int
}

func newDepotRelease(t *testing.T, tag, goos, goarch string, badSignature bool) *depotRelease {
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
	if badSignature {
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

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho \"rafiki " + tag + "\"\n")
	if err := tw.WriteHeader(&tar.Header{Name: "rafiki", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar WriteHeader: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	archive := buf.Bytes()

	asset := release.AssetName(tag, goos, goarch)
	sum := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	sig := relsign.Sign(signKey, checksums)

	var mu sync.Mutex
	archiveGets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tag + "/" + asset:
			mu.Lock()
			archiveGets++
			mu.Unlock()
			_, _ = w.Write(archive)
		case "/" + tag + "/checksums.txt":
			_, _ = w.Write(checksums)
		case "/" + tag + "/checksums.txt.sig":
			_, _ = w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return &depotRelease{
		Server:  srv,
		Keys:    []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub}},
		Archive: asset,
		archiveGet: func() int {
			mu.Lock()
			defer mu.Unlock()
			return archiveGets
		},
	}
}

// depotFiles lists every regular file under root.
func depotFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestExecutorDepotOverrideWins(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()

	dir := t.TempDir()
	bin := filepath.Join(dir, "linux-amd64", "rafiki")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := []byte("#!/bin/sh\necho local build\n")
	if err := os.WriteFile(bin, body, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want := sha256.Sum256(body)

	// A dev tag with no release to fetch from: the override must be the only
	// reason this succeeds, and the release host must never be consulted.
	rel := newDepotRelease(t, "v0.2.0", "linux", "amd64", false)
	d := &executorDepot{
		tag:      "e6d0e789",
		override: dir,
		cacheDir: filepath.Join(t.TempDir(), "executor-binaries"),
		release:  release.NewClient(rel.Server.URL),
		keys:     rel.Keys,
	}

	got, err := d.Binary(ctx, "linux", "amd64")
	c.Require().NoError(err, "Binary")
	c.EqDeep(depotBinary{Path: bin, SHA256: hex.EncodeToString(want[:]), Tag: "e6d0e789"}, got, "Binary")
	c.Eq(0, rel.archiveGet(), "the release host must not be consulted when the override has the binary")

	// A platform the override does not carry falls through, and on a dev build
	// that is the refusal rather than a download.
	_, err = d.Binary(ctx, "linux", "arm64")
	c.Require().Error(err, "Binary for a platform the override lacks")
	c.True(strings.Contains(err.Error(), "not a release build"),
		"dev build with no override binary must refuse, got %q", err)
}

func TestExecutorDepotReleasePathFetchesOnceThenCaches(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()

	rel := newDepotRelease(t, "v0.2.0", "linux", "amd64", false)
	cacheDir := filepath.Join(t.TempDir(), "executor-binaries")
	d := &executorDepot{
		tag:      "v0.2.0",
		cacheDir: cacheDir,
		release:  release.NewClient(rel.Server.URL),
		keys:     rel.Keys,
	}

	first, err := d.Binary(ctx, "linux", "amd64")
	c.Require().NoError(err, "first Binary")
	c.Eq(filepath.Join(cacheDir, "v0.2.0", "linux-amd64", "rafiki"), first.Path, "staged path")
	c.Eq("v0.2.0", first.Tag, "tag")
	c.Eq(1, rel.archiveGet(), "the archive must be fetched exactly once")

	fi, err := os.Stat(first.Path)
	c.Require().NoError(err, "staged binary must exist")
	c.Eq(os.FileMode(0o755), fi.Mode().Perm(), "staged binary mode")
	sum, err := os.ReadFile(first.Path + ".sha256")
	c.Require().NoError(err, "sha file beside the staged binary")
	c.Eq(first.SHA256+"\n", string(sum), "sha file contents")

	second, err := d.Binary(ctx, "linux", "amd64")
	c.Require().NoError(err, "second Binary")
	c.EqDeep(first, second, "a cached binary must resolve to the same path and digest")
	c.Eq(1, rel.archiveGet(), "a cached binary must not be fetched again")

	// The sha file is the cache marker: losing it makes the cached bytes
	// untrusted, so they are staged again rather than shipped unverified.
	c.Require().NoError(os.Remove(first.Path+".sha256"), "remove sha file")
	third, err := d.Binary(ctx, "linux", "amd64")
	c.Require().NoError(err, "Binary after the sha file is gone")
	c.EqDeep(first, third, "re-staging must produce the same binary")
	c.Eq(2, rel.archiveGet(), "a missing sha file must force exactly one re-fetch")
}

func TestExecutorDepotBadSignatureInstallsNothing(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()

	rel := newDepotRelease(t, "v0.2.0", "linux", "amd64", true)
	cacheDir := filepath.Join(t.TempDir(), "executor-binaries")
	d := &executorDepot{
		tag:      "v0.2.0",
		cacheDir: cacheDir,
		release:  release.NewClient(rel.Server.URL),
		keys:     rel.Keys,
	}

	_, err := d.Binary(ctx, "linux", "amd64")
	c.Require().Error(err, "Binary against a bad signature")
	c.True(strings.Contains(err.Error(), "verify checksums.txt"),
		"the refusal must name the failed verification, got %q", err)
	c.Eq(0, rel.archiveGet(), "the archive must not be requested before the signature verifies")
	c.EqDeep([]string(nil), depotFiles(t, cacheDir), "nothing may be left under the cache dir")
}

func TestExecutorDepotDevBuildRefusesWithTheBuildCommand(t *testing.T) {
	ctx := context.Background()

	cacheDir := "/tmp/depot-cache/executor-binaries"
	cases := []struct {
		name     string
		override string
		dir      string
	}{
		{
			name: "no override suggests the cache dir",
			dir:  filepath.Join(cacheDir, "dev"),
		},
		{
			name:     "an override suggests its own dir",
			override: "/opt/rafiki-bins",
			dir:      "/opt/rafiki-bins",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			d := &executorDepot{tag: "e6d0e789", override: tc.override, cacheDir: cacheDir}
			_, err := d.Binary(ctx, "linux", "amd64")
			c.Require().Error(err, "Binary on a dev build")
			want := "rafikid e6d0e789 is not a release build, so it cannot fetch an executor for linux/amd64; " +
				"build one with: GOOS=linux GOARCH=amd64 go build -o " +
				filepath.Join(tc.dir, "linux-amd64", "rafiki") +
				" ./cmd/rafiki (and set RAFIKI_EXECUTOR_BINARIES=" + tc.dir + ")"
			c.Eq(want, err.Error(), "the refusal must hand over the command that fixes it")
		})
	}
}

func TestExecutorDepotPlatformFromUname(t *testing.T) {
	c := assert.NewCollecting(t)

	cases := []struct {
		uname        string
		goos, goarch string
	}{
		{"Linux x86_64", "linux", "amd64"},
		{"Linux aarch64", "linux", "arm64"},
		{"Darwin x86_64", "darwin", "amd64"},
		{"Darwin arm64", "darwin", "arm64"},
		{"OpenBSD amd64", "openbsd", "amd64"},
		{"OpenBSD arm64", "openbsd", "arm64"},
		// ssh hands back the command's raw stdout, newline included.
		{"Linux x86_64\n", "linux", "amd64"},
		// `uname -srm`: the machine is the last field.
		{"Linux 6.1.0 x86_64", "linux", "amd64"},
		{"Darwin 23.4.0 arm64", "darwin", "arm64"},
	}
	for _, tc := range cases {
		goos, goarch, err := platformFromUname(tc.uname)
		c.Require().NoError(err, "platformFromUname(%q)", tc.uname)
		c.Eq(tc.goos, goos, "GOOS for %q", tc.uname)
		c.Eq(tc.goarch, goarch, "GOARCH for %q", tc.uname)
	}

	unknown := []string{
		"",
		"Linux",
		"FreeBSD amd64",
		"Linux riscv64",
		"Darwin aarch64",
		"Linux host 6.1.0 #1 SMP PREEMPT x86_64 GNU/Linux",
	}
	for _, uname := range unknown {
		_, _, err := platformFromUname(uname)
		c.Require().Error(err, "platformFromUname(%q)", uname)
		c.True(strings.Contains(err.Error(), "unsupported platform"),
			"platformFromUname(%q) must refuse with unsupported platform, got %q", uname, err)
	}
}
