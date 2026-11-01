// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// fakeRafikidOnPATH puts an executable named rafikid in a fresh temp dir at the
// front of PATH, so findDaemonBinary resolves it (its sibling lookup next to
// the test binary finds nothing) and returns exactly this path.
func fakeRafikidOnPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, daemonBinaryName)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", daemonBinaryName, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fake
}

// dbExecRec records what the passthrough would have exec'd.
//
// syscall.Exec replaces the process, so a successful exec is unobservable by
// construction; this recorder — installed through the dbExec seam — is the only
// place a test can see the argv rafikid would receive.
type dbExecRec struct {
	path     string
	argv     []string
	executed bool
}

// stubDbExec replaces the exec seam for the duration of the test.
func stubDbExec(t *testing.T) *dbExecRec {
	t.Helper()
	rec := &dbExecRec{}
	dbExec = func(path string, argv []string, _ []string) error {
		rec.path = path
		rec.argv = append([]string(nil), argv...)
		rec.executed = true
		return nil
	}
	t.Cleanup(func() { dbExec = syscall.Exec })
	return rec
}

// TestDbPassesArgsThroughUnchanged pins the passthrough contract: rafikid is
// handed the verb and EVERYTHING after it byte for byte, including a bare "--"
// and including arguments that look like this client's own flags. It pins the
// other half too: a client flag written before the verb is consumed here and
// never forwarded.
func TestDbPassesArgsThroughUnchanged(t *testing.T) {
	isolateProfiles(t)
	fake := fakeRafikidOnPATH(t)
	rec := stubDbExec(t)

	c := assert.NewCollecting(t)
	c.Require().NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: "/tmp/rafiki-db-test.sock"},
	}}), "Save profiles")
	c.Require().NoError(profile.SavePointer("scratch"), "SavePointer")

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "everything from the verb onward, -- and client-looking flags included",
			args: []string{"db", "psql", "--", "-c", "select 1", "-P", "work"},
			want: []string{"psql", "--", "-c", "select 1", "-P", "work"},
		},
		{
			name: "a client flag before the verb is consumed, not forwarded",
			args: []string{"-P", "scratch", "db", "psql", "-c", "select 1"},
			want: []string{"psql", "-c", "select 1"},
		},
		{
			name: "a verb with its own positional argument",
			args: []string{"db", "backup", "/tmp/rafiki.dump"},
			want: []string{"backup", "/tmp/rafiki.dump"},
		},
		{
			name: "a verb's own flag needs no -- from us",
			args: []string{"db", "backup", "/tmp/rafiki.dump", "--force"},
			want: []string{"backup", "/tmp/rafiki.dump", "--force"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := assert.NewCollecting(t)
			rec.path, rec.argv, rec.executed = "", nil, false
			// Resolution is once per process; clearing it here is what makes
			// the "-P scratch" case actually parse the flag.
			resetProfileCache()

			root := newRootCmd()
			root.SetArgs(tc.args)
			st.Require().NoError(root.Execute(), "rafiki %v", tc.args)

			st.True(rec.executed, "rafiki %v must exec rafikid", tc.args)
			st.Eq(fake, rec.path, "the exec target must be the rafikid findDaemonBinary resolved")
			st.EqDeep(append([]string{fake, "db"}, tc.want...), rec.argv,
				"argv must be rafikid's, with the passthrough arguments untouched (rafiki %v)", tc.args)
		})
	}
}

// TestDbRefusesRemoteProfile pins the refusal that keeps a database on another
// machine out of reach. The profile carries NO token on purpose: the refusal is
// about where the daemon is, not about a credential, so it must not degrade
// into "your token is missing".
func TestDbRefusesRemoteProfile(t *testing.T) {
	isolateProfiles(t)
	rec := stubDbExec(t)

	c := assert.NewCollecting(t)
	c.Require().NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", URL: "https://rafiki.example.net:7345"},
	}}), "Save profiles")
	c.Require().NoError(profile.SavePointer("work"), "SavePointer")
	resetProfileCache()

	root := newRootCmd()
	root.SetArgs([]string{"db", "psql", "--", "-c", "select 1"})
	err := root.Execute()

	c.Require().Error(err, "a remote profile must be refused before anything is exec'd")
	c.StrContains(err.Error(), "https://rafiki.example.net:7345", "the refusal must name the endpoint:")
	c.StrContains(err.Error(), "the database lives on the daemon's host", "the refusal must say where the database is:")
	c.False(rec.executed, "nothing may be exec'd against a remote profile")
}

// TestDbEndpointLocality pins what "local" means: the unix socket, or a
// loopback host. Everything else is refused, including a name that only
// resolves to loopback (resolving DNS here would let a typo through).
func TestDbEndpointLocality(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"unix socket (no URL)", "", true},
		{"loopback v4", "https://127.0.0.1:7345", true},
		{"loopback v6", "https://[::1]:7345", true},
		{"localhost by name", "https://localhost:7345", true},
		{"the web proxy face on loopback", "http://localhost:8088", true},
		{"a remote host", "https://rafiki.example.net", false},
		{"a private-network address", "http://192.168.1.10:7345", false},
		{"an unparseable URL", "https://", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NewCollecting(t).Eq(tc.want,
				dbEndpointIsLocal(profile.Resolved{Profile: profile.Profile{URL: tc.url}}),
				"locality of %q", tc.url)
		})
	}
}

// TestDbWithNoVerbPrintsHelp keeps a bare `rafiki db` from exec'ing rafikid's
// group just to read its help: the client explains the passthrough itself.
func TestDbWithNoVerbPrintsHelp(t *testing.T) {
	isolateProfiles(t)
	rec := stubDbExec(t)
	resetProfileCache()

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"db"})

	c := assert.NewCollecting(t)
	c.Require().NoError(root.Execute(), "rafiki db")
	c.False(rec.executed, "a bare `rafiki db` must not exec")
	c.StrContains(out.String(), "rafiki db psql", "the help must show how to pass arguments through:")
}
