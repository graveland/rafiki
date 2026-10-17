// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/installstate"
)

// versionRunner answers any "<path> --version" Run with a scripted tag and
// delegates everything else to the embedded fake.
type versionRunner struct {
	*fakeRunner
	versionTag string
}

func (v versionRunner) Run(ctx context.Context, env []string, argv ...string) ([]byte, []byte, error) {
	if len(argv) == 2 && argv[1] == "--version" {
		return []byte("rafiki version " + v.versionTag + "\n"), nil, nil
	}
	return v.fakeRunner.Run(ctx, env, argv...)
}

var _ runner = versionRunner{}

// oldInstall is a tarball-shaped install: a directory holding rafiki and
// rafikid as plain files.
func oldInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"rafiki": "old-rafiki", "rafikid": "old-rafikid"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	return dir
}

func assertOldInstall(t *testing.T, dir string) {
	t.Helper()
	for name, want := range map[string]string{"rafiki": "old-rafiki", "rafikid": "old-rafikid"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func assertNewInstall(t *testing.T, dir, tag string) {
	t.Helper()
	for name, want := range map[string]string{
		"rafiki":  "#!/bin/sh\necho \"rafiki version " + tag + "\"\n",
		"rafikid": "#!/bin/sh\necho \"rafikid " + tag + "\"\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
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

func sawRun(r *fakeRunner, argv ...string) bool {
	want := strings.Join(argv, " ")
	for _, c := range r.runCalls {
		if strings.Join(c.argv, " ") == want {
			return true
		}
	}
	return false
}

func upgradeDepsFor(t *testing.T, tr *testRelease, vr versionRunner, exe, running, home string) (upgradeDeps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	return upgradeDeps{
		Out:     &out,
		Err:     &errOut,
		Runner:  vr,
		Client:  tr.Client,
		Keys:    tr.Keys,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Exe:     exe,
		Version: running,
		Home:    home,
	}, &out, &errOut
}

func TestUpgradeBrewRefusesWithoutForce(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}

	deps, _, _ := upgradeDepsFor(t, tr, vr, "/opt/homebrew/Caskroom/rafiki/0.1.4/rafiki", "v0.1.4", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().Error(err, "brew without --force must be refused")
	c.StrContains(err.Error(), "brew upgrade rafiki", "the error must name the Homebrew remedy")
	c.Eq(0, tr.Requests(".tar.gz"), "no archive may be downloaded")
	c.False(sawRun(r, "/opt/homebrew/Caskroom/rafiki/0.1.4/rafiki", "service", "refresh"), "no service refresh may run")
}

func TestUpgradeSourceRefusesWithoutForce(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}

	deps, _, _ := upgradeDepsFor(t, tr, vr, "/tmp/x/rafiki", "abc1234-dirty", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().Error(err, "a source build without --force must be refused")
	c.StrContains(err.Error(), "built from source", "the error must explain the source build")
	c.Eq(0, tr.Requests(""), "the refusal must happen before any network call")
}

func TestUpgradeTarballAlreadyUpToDate(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, out, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.2.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().NoError(err, "an up-to-date tarball install")
	c.StrContains(out.String(), "already up to date (v0.2.0)", "the up-to-date message")
	c.Eq(0, tr.Requests(".tar.gz"), "no archive may be downloaded")
	assertOldInstall(t, dir)
}

func TestUpgradeTarballNeverDowngradesByDefault(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, out, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.3.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().NoError(err, "a newer running version")
	c.StrContains(out.String(), "already up to date (v0.3.0)", "a newer running version must not be downgraded")
	c.Eq(0, tr.Requests(".tar.gz"), "no archive may be downloaded")
	assertOldInstall(t, dir)
}

func TestUpgradeTarballInstallsInPlace(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, out, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.1.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().NoError(err, "an in-place upgrade")
	c.StrContains(out.String(), "upgraded to v0.2.0", "the completion message")
	assertNewInstall(t, dir, "v0.2.0")
	c.True(sawRun(r, filepath.Join(dir, "rafiki"), "service", "refresh"), "the new binary must refresh services")
	c.EqDiff([]string{"rafiki", "rafikid"}, dirNames(t, dir), "the directory must hold only the two binaries")
}

func TestUpgradeCheckChangesNothing(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, out, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.1.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{Check: true})

	c.Require().NoError(err, "--check")
	c.StrContains(out.String(), "update available: run rafiki upgrade", "the availability line")
	c.Eq(0, tr.Requests(".tar.gz"), "--check must not download anything")
	assertOldInstall(t, dir)
}

func TestUpgradeExplicitVersionAllowsDowngrade(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, out, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.3.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{Version: "v0.2.0"})

	c.Require().NoError(err, "an explicit older --version")
	c.StrContains(out.String(), "upgraded to v0.2.0", "the completion message")
	assertNewInstall(t, dir, "v0.2.0")
}

func TestUpgradeExplicitVersionEqualToRunningSaysSo(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, out, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.2.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{Version: "v0.2.0"})

	c.Require().NoError(err, "--version equal to the running one")
	c.StrContains(out.String(), "already at v0.2.0", "the no-op message")
	c.Eq(0, tr.Requests(".tar.gz"), "no download for an already-installed version")
	assertOldInstall(t, dir)
}

func TestUpgradeForceReinstallsSameVersion(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, out, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.2.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{Force: true})

	c.Require().NoError(err, "--force reinstalling the current release")
	c.StrContains(out.String(), "upgraded to v0.2.0", "the completion message")
	assertNewInstall(t, dir, "v0.2.0")
	c.EqDiff([]string{"rafiki", "rafikid"}, dirNames(t, dir), "the directory must hold only the two binaries")
}

func TestUpgradeForceConvertsSourceToLocalBin(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	home := t.TempDir()

	deps, out, _ := upgradeDepsFor(t, tr, vr, "/tmp/x/rafiki", "abc1234-dirty", home)
	err := runUpgrade(context.Background(), deps, upgradeOpts{Force: true})

	c.Require().NoError(err, "--force converting a source build")
	bin := filepath.Join(home, ".local", "bin")
	assertNewInstall(t, bin, "v0.2.0")
	st, ok, err := installstate.Load()
	c.Require().NoError(err, "installstate.Load")
	c.True(ok, "the install state must exist after a conversion")
	c.Eq(installstate.ChannelTarball, st.Channel, "the recorded channel")
	c.NotStrContains(out.String(), "Homebrew", "a source conversion is not a Homebrew install")
}

func TestUpgradeForceConvertsBrewPrintsUninstallLine(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	r.paths["rafiki"] = "/opt/homebrew/bin/rafiki"
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	home := t.TempDir()

	caskroom := filepath.Join(t.TempDir(), "opt", "homebrew", "Caskroom", "rafiki", "0.1.4")
	c.Require().NoError(os.MkdirAll(caskroom, 0o755))
	for name, body := range map[string]string{"rafiki": "cask-rafiki", "rafikid": "cask-rafikid"} {
		c.Require().NoError(os.WriteFile(filepath.Join(caskroom, name), []byte(body), 0o755))
	}

	deps, out, errOut := upgradeDepsFor(t, tr, vr, filepath.Join(caskroom, "rafiki"), "v0.1.4", home)
	err := runUpgrade(context.Background(), deps, upgradeOpts{Force: true})

	c.Require().NoError(err, "--force converting a Homebrew install")
	c.StrContains(out.String(), "Homebrew still has a copy. Remove it with: brew uninstall rafiki", "the Homebrew notice")
	c.StrContains(errOut.String(), "warning: 'rafiki' on PATH resolves to /opt/homebrew/bin/rafiki", "the PATH warning")
	for name, want := range map[string]string{"rafiki": "cask-rafiki", "rafikid": "cask-rafikid"} {
		got, err := os.ReadFile(filepath.Join(caskroom, name))
		c.Require().NoError(err, "ReadFile %s", name)
		c.Eq(want, string(got), "the Caskroom %s must be untouched", name)
	}
}

func TestUpgradeBadSignatureLeavesInstallIntact(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{BadSignature: true})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)

	deps, _, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.1.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().Error(err, "a bad signature must fail the upgrade")
	assertOldInstall(t, dir)
	c.EqDiff([]string{"rafiki", "rafikid"}, dirNames(t, dir), "the directory must hold only the old binaries")
}

func TestUpgradeSmokeCheckMismatchLeavesInstallIntact(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.9.9"}
	dir := oldInstall(t)

	deps, _, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.1.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().Error(err, "a smoke-check mismatch must fail the upgrade")
	assertOldInstall(t, dir)
	c.EqDiff([]string{"rafiki", "rafikid"}, dirNames(t, dir), "no temp files may remain")
}

func TestUpgradeRefreshFailureIsReportedAfterSwap(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)
	r.scriptErr([]string{filepath.Join(dir, "rafiki"), "service", "refresh"}, errors.New("refresh boom"))

	deps, _, _ := upgradeDepsFor(t, tr, vr, filepath.Join(dir, "rafiki"), "v0.1.0", t.TempDir())
	err := runUpgrade(context.Background(), deps, upgradeOpts{})

	c.Require().Error(err, "a service refresh failure must be reported")
	c.StrContains(err.Error(), "service refresh failed", "the error must name the refresh failure")
	assertNewInstall(t, dir, "v0.2.0")
}

func TestUpgradeDownloadBaseRequiresVersion(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)
	t.Setenv("RAFIKI_DOWNLOAD_BASE", "http://127.0.0.1:1")

	cmd := newUpgradeCmd()
	cmd.SetArgs([]string{"--check"})
	err := cmd.Execute()

	c.Require().Error(err, "RAFIKI_DOWNLOAD_BASE without --version must be refused")
	c.StrContains(err.Error(), "requires --version", "the error must name the required flag")
}

func TestUpgradeCmdIsRegistered(t *testing.T) {
	isolateProfiles(t)
	c := assert.NewCollecting(t)

	cmd, _, err := newRootCmd().Find([]string{"upgrade"})
	c.Require().NoError(err, "Find(upgrade)")
	c.Eq("upgrade", cmd.Name(), "the command name")
	for _, flag := range []string{"check", "version", "force"} {
		c.NotNil(cmd.Flags().Lookup(flag), "the --%s flag must exist", flag)
	}
}
