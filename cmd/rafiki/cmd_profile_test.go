// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
)

// isolateProfiles points this test at its own config/state tree. Every test
// touching profiles needs it: TestMain's shared dir would let tests see each
// other's manifests.
func isolateProfiles(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
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
	isolateProfiles(t)

	if _, err := runProfileCmd(t, "add", "work", "--socket", "/tmp/work.sock"); err != nil {
		t.Fatalf("profile add: %v", err)
	}
	if _, err := runProfileCmd(t, "add", "personal", "--url", "https://rafiki.example.net", "--token", "sk-x"); err != nil {
		t.Fatalf("profile add personal: %v", err)
	}

	out, err := runProfileCmd(t, "list")
	if err != nil {
		t.Fatalf("profile list: %v", err)
	}
	for _, want := range []string{"work", "personal", "/tmp/work.sock", "https://rafiki.example.net"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	if _, err := runProfileCmd(t, "use", "work"); err != nil {
		t.Fatalf("profile use: %v", err)
	}
	if got := profile.LoadPointer(); got != "work" {
		t.Fatalf("pointer = %q, want work", got)
	}

	out, err = runProfileCmd(t, "current")
	if err != nil {
		t.Fatalf("profile current: %v", err)
	}
	if strings.TrimSpace(out) != "work" {
		t.Fatalf("current = %q, want work", strings.TrimSpace(out))
	}
}

func TestProfileListOnABareMachineIsSilentAndSucceeds(t *testing.T) {
	isolateProfiles(t)

	out, err := runProfileCmd(t, "list")
	if err != nil {
		t.Fatalf("profile list on a bare machine = %v, want nil — the profile verbs must not need a profile, or the feature cannot bootstrap itself", err)
	}
	if strings.Contains(out, "default") {
		t.Fatalf("profile list bootstrapped a profile:\n%s", out)
	}
}

func TestProfileAddRefusesARemoteWithNoToken(t *testing.T) {
	isolateProfiles(t)

	_, err := runProfileCmd(t, "add", "personal", "--url", "https://rafiki.example.net")
	if err == nil {
		t.Fatal("profile add --url with no --token = nil error; a tokenless remote can only ever 401")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("error %q does not mention the token", err)
	}
}

func TestProfileAddRefusesBothEndpoints(t *testing.T) {
	isolateProfiles(t)

	_, err := runProfileCmd(t, "add", "x", "--url", "https://h", "--socket", "/s", "--token", "t")
	if err == nil {
		t.Fatal("profile add with both --url and --socket = nil error")
	}
}

func TestProfileRemoveRefusesTheCurrentOneWithoutForce(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Get("work"); ok {
		t.Fatal("work survived remove --force")
	}
}
