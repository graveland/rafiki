// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.graveland.dev/rafiki/pkg/release"
	"go.graveland.dev/rafiki/pkg/relsign"
)

// depotBinary is one verified `rafiki` binary the daemon can ship to another
// machine, plus what the target needs to prove it received those exact bytes.
type depotBinary struct {
	Path   string // absolute path to a regular file, mode 0755
	SHA256 string // hex sha256 of that file's bytes
	Tag    string // the version this daemon runs; the remote must match it
}

// executorDepot is the daemon's single source of the `rafiki` binary it
// installs on a remote machine.
//
// A remote executor runs exactly the DAEMON's version: the daemon is the epoch
// authority (protocol.Epoch), so a remote built from anything else could fail
// the epoch check on every connection, or — worse — pass it while behaving
// differently. The depot therefore never resolves "latest": Tag is this
// process's own version, fixed at construction.
//
// Two sources, in order:
//
//   - $RAFIKI_EXECUTOR_BINARIES/<goos>-<goarch>/rafiki, an operator's own build.
//     This is the only source for an untagged (dev) daemon, which has no
//     release to fetch from.
//   - the signed release for this tag, staged once into
//     <CacheDir>/executor-binaries/<tag>/<goos>-<goarch>/ and reused.
//
// Nothing is ever shipped unverified: an override binary is hashed here, and a
// staged one was checked against the signed checksums.txt by pkg/release
// (relsign.Verify) before the archive was even requested.
type executorDepot struct {
	tag      string // version.String() at construction
	override string // $RAFIKI_EXECUTOR_BINARIES, may be ""
	cacheDir string // paths.CacheDir()/executor-binaries
	release  *release.Client
	keys     []relsign.Key
	mu       sync.Mutex // one fetch per (goos, goarch) at a time
}

// Binary returns a verified `rafiki` binary for goos/goarch.
//
// The mutex is held across the whole resolution, which is stricter than "one
// fetch per (goos, goarch) at a time": it also keeps two platforms from racing
// on the cache directory, at the cost of serialising a first-time install of
// several platforms. Installs are rare and operator-driven; correctness of the
// staged bytes is not.
func (d *executorDepot) Binary(ctx context.Context, goos, goarch string) (depotBinary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if b, ok, err := d.fromOverride(goos, goarch); err != nil {
		return depotBinary{}, err
	} else if ok {
		return b, nil
	}

	if !release.IsReleaseTag(d.tag) {
		return depotBinary{}, d.notARelease(goos, goarch)
	}

	dir := filepath.Join(d.cacheDir, d.tag, goos+"-"+goarch)
	bin := filepath.Join(dir, "rafiki")
	sumPath := bin + ".sha256"

	if sum, ok := cachedDepotBinary(bin, sumPath); ok {
		return depotBinary{Path: bin, SHA256: sum, Tag: d.tag}, nil
	}

	// The staging code writes a temp file into destPath's directory before it
	// downloads anything, so the directory has to exist first.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return depotBinary{}, fmt.Errorf("stage %s executor: %w", d.tag, err)
	}
	sum, err := d.release.StageBinary(ctx, d.keys, d.tag, goos, goarch, "rafiki", bin)
	if err != nil {
		return depotBinary{}, fmt.Errorf("stage %s executor for %s/%s: %w", d.tag, goos, goarch, err)
	}
	if err := writeDepotSHA(sumPath, sum); err != nil {
		return depotBinary{}, err
	}
	return depotBinary{Path: bin, SHA256: sum, Tag: d.tag}, nil
}

// fromOverride returns the operator's own build for this platform, if there is
// one. A missing file is not an error: it just means the release path (or the
// dev-build refusal) decides.
func (d *executorDepot) fromOverride(goos, goarch string) (depotBinary, bool, error) {
	if d.override == "" {
		return depotBinary{}, false, nil
	}
	p := filepath.Join(d.override, goos+"-"+goarch, "rafiki")
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return depotBinary{}, false, nil
	}
	sum, err := hashDepotFile(p)
	if err != nil {
		return depotBinary{}, false, err
	}
	return depotBinary{Path: p, SHA256: sum, Tag: d.tag}, true, nil
}

// notARelease is the dev-build refusal. It names the exact command that would
// put a binary where Binary looks, because the alternative — fetching the
// latest release — would install a version this daemon is not, and "which
// binary did the remote get" must never be a guess.
func (d *executorDepot) notARelease(goos, goarch string) error {
	dir := d.override
	if dir == "" {
		dir = filepath.Join(d.cacheDir, "dev")
	}
	return fmt.Errorf(
		"rafikid %s is not a release build, so it cannot fetch an executor for %s/%s; "+
			"build one with: GOOS=%s GOARCH=%s go build -o %s ./cmd/rafiki (and set RAFIKI_EXECUTOR_BINARIES=%s)",
		d.tag, goos, goarch, goos, goarch, filepath.Join(dir, goos+"-"+goarch, "rafiki"), dir,
	)
}

// cachedDepotBinary returns the cached binary's sha256 when the cached bytes
// still match the sibling sha file. A missing, unreadable or mismatched pair is
// a cache MISS rather than an error: the cache is disposable, so the answer is
// to stage it again. Re-hashing on every call is deliberate — a truncated file
// from an interrupted process would otherwise be shipped to a remote machine
// as this daemon's version.
func cachedDepotBinary(bin, sumPath string) (string, bool) {
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return "", false
	}
	sum := strings.TrimSpace(string(data))
	if !isHexSHA256(sum) {
		return "", false
	}
	fi, err := os.Stat(bin)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	got, err := hashDepotFile(bin)
	if err != nil || got != sum {
		return "", false
	}
	return sum, true
}

// writeDepotSHA records the sha256 of a staged binary beside it, through a temp
// file and a rename so a reader never sees a half-written digest.
func writeDepotSHA(path, sum string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.WriteString(sum + "\n"); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// hashDepotFile returns the hex sha256 of a file's bytes.
func hashDepotFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isHexSHA256 reports whether s is a bare lowercase sha256 digest.
func isHexSHA256(s string) bool {
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

// platformFromUname maps the target's `uname -s -m` (or `-srm`) output to a
// Go GOOS/GOARCH pair.
//
// Only the platforms a release archive exists for are accepted, and only the
// exact spellings those unames print: a guess here installs a binary that
// cannot exec, and the failure surfaces on the remote machine. The machine is
// taken from the LAST field so a three-field `uname -srm` works; a bare
// `uname -a` (many fields, ending in "GNU/Linux") is refused rather than
// misread.
func platformFromUname(uname string) (goos, goarch string, err error) {
	fields := strings.Fields(uname)
	if len(fields) == 2 || len(fields) == 3 {
		switch fields[0] + " " + fields[len(fields)-1] {
		case "Linux x86_64":
			return "linux", "amd64", nil
		case "Linux aarch64":
			return "linux", "arm64", nil
		case "Darwin x86_64":
			return "darwin", "amd64", nil
		case "Darwin arm64":
			return "darwin", "arm64", nil
		case "OpenBSD amd64":
			return "openbsd", "amd64", nil
		case "OpenBSD arm64":
			return "openbsd", "arm64", nil
		}
	}
	return "", "", fmt.Errorf("unsupported platform %q", strings.TrimSpace(uname))
}
