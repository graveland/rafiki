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

// The staging and verification logic itself lives in pkg/release and is tested
// there. What stays here is the fixture `rafiki upgrade`'s own tests drive it
// through: a fake release host serving a signed archive for any platform.
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
	Client   *release.Client
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
	asset := release.AssetName(tag, goos, goarch)
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
		Client:   &release.Client{LatestURL: srv.URL + "/latest", DownloadBase: srv.URL, HTTP: srv.Client()},
		Archive:  asset,
		Requests: requests,
	}
}

func TestSmokeCheckMatchesWholeField(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		c.Require().NoError(os.WriteFile(p, []byte(body), 0o755), "WriteFile %s", name)
		return p
	}
	ok := write("ok", "#!/bin/sh\necho \"rafiki version v0.2.0\"\n")
	bad := write("bad", "#!/bin/sh\nexit 1\n")

	c.NoError(smokeCheck(context.Background(), execRunner{}, ok, "v0.2.0"), "smokeCheck(v0.2.0)")
	c.Error(smokeCheck(context.Background(), execRunner{}, ok, "v0.2"), "smokeCheck(v0.2)")
	c.Error(smokeCheck(context.Background(), execRunner{}, ok, "v0.2.01"), "smokeCheck(v0.2.01)")
	c.Error(smokeCheck(context.Background(), execRunner{}, bad, "v0.2.0"), "smokeCheck of a failing script")
}

func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	return entries
}
