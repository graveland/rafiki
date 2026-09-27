// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// isolateProfiles points this test at its own config/state tree. Every test
// touching profiles needs it: TestMain's shared dir would let tests see each
// other's manifests.
//
// It also blanks every retired env var (paths.URL, paths.Token, etc. — the
// exact set profile.CheckRetiredEnv rejects) PLUS RAFIKI_PROFILE, which is
// live (it names the selection, not a retired setting) but just as capable of
// steering resolveProfile out from under a test that didn't ask for it: any
// test that reaches resolveProfile runs CheckRetiredEnv first and then
// consults RAFIKI_PROFILE, and a developer's own shell legitimately exports
// RAFIKI_URL/RAFIKI_TOKEN/RAFIKI_PROFILE for everyday use against a real
// daemon. Without this, a test's outcome would depend on who is running it —
// green in CI, a hard os.Exit(2) (via mustProfile) on a workstation with those
// set (RAFIKI_PROFILE=bogus fails resolution just as surely as a retired var
// does — "unknown profile", not "retired variable", but the same silent
// mid-binary os.Exit(2)). Blanking here, once, is cheaper than repeating it in
// every test that calls isolateProfiles.
func isolateProfiles(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	for _, v := range []string{paths.URL, paths.Token, paths.Socket, paths.DefaultModel, paths.DefaultPreset, paths.DefaultLabels, "RAFIKI_PROFILE"} {
		t.Setenv(v, "")
	}
}

// runProfileCmd executes `rafiki profile <args...>` and returns its output.
func runProfileCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newProfileCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestProfileAddThenListThenUse(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)

	if _, err := runProfileCmd(t, "add", "work", "--socket", "/tmp/work.sock"); err != nil {
		t.Fatalf("profile add: %v", err)
	}
	if _, err := runProfileCmd(t, "add", "personal", "--url", "https://rafiki.example.net", "--token", "sk-x"); err != nil {
		t.Fatalf("profile add personal: %v", err)
	}

	out, err := runProfileCmd(t, "list")
	c.Require().NoError(err, "profile list")
	for _, want := range []string{"work", "personal", "/tmp/work.sock", "https://rafiki.example.net"} {
		c.StrContains(out, want, "list output missing")
	}

	if _, err := runProfileCmd(t, "use", "work"); err != nil {
		t.Fatalf("profile use: %v", err)
	}
	c.Require().Eq("work", profile.LoadPointer(), "pointer")

	out, err = runProfileCmd(t, "current")
	c.Require().NoError(err, "profile current")
	c.Require().Eq("work", strings.TrimSpace(out), "current")
}

func TestProfileListOnABareMachineIsSilentAndSucceeds(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	out, err := runProfileCmd(t, "list")
	c.NoError(err, "profile list on a bare machine")
	c.NotStrContains(out, "default", "profile list bootstrapped a profile:\n")
}

func TestProfileAddRefusesARemoteWithNoToken(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	_, err := runProfileCmd(t, "add", "personal", "--url", "https://rafiki.example.net")
	c.Error(err, "profile add --url with no --token = nil error; a tokenless remote can only ever 401")
	c.StrContains(err.Error(), "token", "error %q does not mention the token", err)
}

func TestProfileAddRefusesBothEndpoints(t *testing.T) {
	isolateProfiles(t)

	_, err := runProfileCmd(t, "add", "x", "--url", "https://h", "--socket", "/s", "--token", "t")
	assert.NewAborting(t).Error(err, "profile add with both --url and --socket = nil error")
}

func TestProfileRemoveRefusesTheCurrentOneWithoutForce(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	if _, err := runProfileCmd(t, "add", "work", "--socket", "/tmp/work.sock"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := runProfileCmd(t, "use", "work"); err != nil {
		t.Fatalf("use: %v", err)
	}
	if _, err := runProfileCmd(t, "remove", "work"); err == nil {
		t.Fatal("remove of the current profile = nil error; that leaves every command unresolvable")
	}
	if _, err := runProfileCmd(t, "remove", "work", "--force"); err != nil {
		t.Fatalf("remove --force: %v", err)
	}
	set, err := profile.Load()
	c.NoError(err, "Load")
	_, ok := set.Get("work")
	c.False(ok, "work survived remove --force")
}

// TestProfileAddRejectsTraversalNames pins Fix 1: `rafiki profile add ..` (or
// any other name that could escape profile.Dir when joined) must be refused
// before anything is written or deleted. The regression this guards: without
// ValidName, `rafiki profile add ..` succeeded, and a later
// `rafiki profile remove .. --force` called os.RemoveAll(profile.Dir(".."))
// -- which filepath.Join cleans down to paths.ConfigDir() itself, deleting
// profiles.toml, current-profile and every other profile's token.
func TestProfileAddRejectsTraversalNames(t *testing.T) {
	for _, name := range []string{"..", "../etc", "a/b", "."} {
		t.Run("name="+name, func(t *testing.T) {
			c := assert.NewAborting(t)
			isolateProfiles(t)

			// A canary file inside the config dir stands in for
			// profiles.toml/current-profile/service.env/etc: if `add` (wrongly)
			// created a profile and a later `remove --force` (wrongly) deleted the
			// config dir, this file would vanish with it.
			c.NoError(os.MkdirAll(paths.ConfigDir(), 0o700), "MkdirAll")
			canary := filepath.Join(paths.ConfigDir(), "canary")
			c.NoError(os.WriteFile(canary, []byte("keep me"), 0o600), "write canary")

			_, err := runProfileCmd(t, "add", name, "--socket", "/tmp/x.sock")
			c.Error(err, "profile add %q = nil error, want a rejection", name)

			// Nothing should have been written: the manifest either doesn't exist
			// or, if it does (a prior subtest step), does not contain this name.
			if set, loadErr := profile.Load(); loadErr == nil {
				_, ok := set.Get(name)
				c.False(ok, "profile add %q was rejected but the profile still exists in the manifest", name)
			}

			// The canary must still be exactly what it was -- proving no
			// RemoveAll reached the config directory as a side effect of this
			// (rejected) add.
			b, err := os.ReadFile(canary)
			c.NoError(err, "canary file gone after rejected add %q", name)
			c.Eq("keep me", string(b), "canary file contents changed: %q", b)
		})
	}
}

// TestProfileAddValidatesBeforeWriting pins Fix 2: a profile that fails
// validation (here, an http:// url, which `profile.Validate` rejects because
// only https is a real control listener) must not be written to
// profiles.toml at all -- a later `rafiki profile list` must succeed and show
// only what existed before the failed add.
func TestProfileAddValidatesBeforeWriting(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	if _, err := runProfileCmd(t, "add", "work", "--socket", "/tmp/work.sock"); err != nil {
		t.Fatalf("add work: %v", err)
	}

	_, err := runProfileCmd(t, "add", "bad", "--url", "http://insecure", "--token", "t")
	c.Error(err, "profile add --url http://insecure = nil error, want a validation failure")

	// The manifest must still parse and must not contain "bad" -- proving
	// nothing was written before validation ran.
	set, err := profile.Load()
	c.NoError(err, "profiles.toml does not parse after a rejected add")
	_, ok := set.Get("bad")
	c.False(ok, "the invalid profile was written to the manifest despite validation failing")

	out, err := runProfileCmd(t, "list")
	c.NoError(err, "profile list after a rejected add")
	c.StrContains(out, "work", "profile list output missing the pre-existing profile:\n")
	c.NotStrContains(out, "bad", "profile list output shows the rejected profile:\n")
}

// TestProfileShowJSONRecord pins the machine-readable record the Python SDK's
// Client.from_profile shells out to: -o json (and -j) emit ONE record with the
// resolved token value, the Connect socket beside the profile's framed socket,
// and exactly one of socket/url — the shape pkg/profile is the only resolver
// of, and a shape that must not drift without breaking from_profile.
func TestProfileShowJSONRecord(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)

	if _, err := runProfileCmd(t, "add", "it", "--socket", "/tmp/show.sock"); err != nil {
		t.Fatalf("profile add: %v", err)
	}
	tokDir := filepath.Join(paths.ConfigDir(), "profiles", "it")
	c.Require().NoError(os.MkdirAll(tokDir, 0o700), "mkdir token dir")
	c.Require().NoError(os.WriteFile(filepath.Join(tokDir, "token"), []byte("tok-show-1\n"), 0o600), "write token")

	out, err := runProfileShowViaRoot(t, "-o", "json")
	c.Require().NoError(err, "profile show -o json")
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("show -o json is not one JSON record: %v\n%s", err, out)
	}
	for _, key := range []string{"name", "socket", "url", "connect_socket", "token", "kind", "model", "preset", "labels"} {
		_, ok := rec[key]
		c.True(ok, "show -o json record missing %q: %s", key, out)
	}
	if rec["name"] != "it" {
		t.Errorf("name = %v, want it", rec["name"])
	}
	if rec["socket"] != "/tmp/show.sock" {
		t.Errorf("socket = %v, want /tmp/show.sock", rec["socket"])
	}
	if rec["url"] != "" {
		t.Errorf("url = %v, want empty on a socket profile", rec["url"])
	}
	if rec["connect_socket"] != "/tmp/show.sock" {
		t.Errorf("connect_socket = %v, want /tmp/show.sock (it duplicates socket)", rec["connect_socket"])
	}
	if rec["token"] != "tok-show-1" {
		t.Errorf("token = %v, want the RESOLVED value tok-show-1", rec["token"])
	}
	if labels, ok := rec["labels"].(map[string]any); !ok || len(labels) != 0 {
		t.Errorf("labels = %v, want an empty object (never null)", rec["labels"])
	}

	// -j is the shorthand for the same record, compact on one line under -J.
	jOut, err := runProfileShowViaRoot(t, "it", "-j")
	c.Require().NoError(err, "profile show it -j")
	var rec2 map[string]any
	if err := json.Unmarshal([]byte(jOut), &rec2); err != nil {
		t.Fatalf("show -j is not one JSON record: %v\n%s", err, jOut)
	}
	c.False(rec2["connect_socket"] != rec["connect_socket"] || rec2["token"] != rec["token"], "-j record drifted from -o json: %v vs %v", rec2, rec)
}

// runProfileShowViaRoot executes `rafiki profile show <args...>` through the
// ROOT command, because --output/-j/-J are root PERSISTENT flags: they do not
// exist on the profile subcommand driven directly, which is how the other
// runProfileCmd tests run it.
func runProfileShowViaRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"profile", "show"}, args...))
	err := root.Execute()
	return buf.String(), err
}
