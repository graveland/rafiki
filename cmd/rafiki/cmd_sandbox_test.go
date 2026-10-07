// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// ─── the mount-flag parser ────────────────────────────────────────────────────

func TestSandboxCmdParseMount(t *testing.T) {
	t.Run("valid forms", func(t *testing.T) {
		cases := []struct {
			in   string
			kind string
			tgt  string
			host string
			vol  string
		}{
			{"ro:/work=host:/srv/repos/app", "ro", "/work", "/srv/repos/app", ""},
			{"rw:/scratch", "rw", "/scratch", "", ""},
			{"ephemeral:/tmp", "ephemeral", "/tmp", "", ""},
			{"rw:/scratch=volume:scratch", "rw", "/scratch", "", "scratch"},
		}
		for _, tc := range cases {
			m, err := parseSandboxMount(tc.in)
			if err != nil {
				t.Fatalf("parseSandboxMount(%q) = %v, want no error", tc.in, err)
			}
			if m.GetKind() != tc.kind || m.GetTarget() != tc.tgt || m.GetHostPath() != tc.host || m.GetVolume() != tc.vol {
				t.Fatalf("parseSandboxMount(%q) = %+v, want kind=%q target=%q host=%q vol=%q",
					tc.in, m, tc.kind, tc.tgt, tc.host, tc.vol)
			}
		}
	})

	t.Run("errors", func(t *testing.T) {
		cases := []struct {
			in      string
			wantSub string
		}{
			{"/work", "missing kind"},
			{":/work", "unknown mount kind"},
			{"rwx:/work", "unknown mount kind"},
			{"ro:", "missing target"},
			{"ro:/work=host:/a=volume:b", "at most one"},
			{"ro:/work=ftp:/a", "unknown source"},
			{"ro:/work=host", "host:<path> or volume:<name>"},
			{"ro:/work=host:", "host:<path> or volume:<name>"},
		}
		for _, tc := range cases {
			_, err := parseSandboxMount(tc.in)
			if err == nil {
				t.Fatalf("parseSandboxMount(%q) succeeded, want an error containing %q", tc.in, tc.wantSub)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.wantSub)) {
				t.Fatalf("parseSandboxMount(%q) error = %q, want it to contain %q", tc.in, err, tc.wantSub)
			}
			// The error must name the offending flag value.
			if !bytes.Contains([]byte(err.Error()), []byte(tc.in)) {
				t.Fatalf("parseSandboxMount(%q) error = %q, want it to name the flag value", tc.in, err)
			}
		}
	})
}

// ─── size parsing ─────────────────────────────────────────────────────────────

func TestSandboxCmdParseMemory(t *testing.T) {
	c := assert.NewAborting(t)
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1048576", 1048576},
		{"1b", 1},
		{"1k", 1 << 10},
		{"1kb", 1 << 10},
		{"1KiB", 1 << 10},
		{"512m", 512 << 20},
		{"1g", 1 << 30},
		{"2GB", 2 << 30},
		{"1t", 1 << 40},
	}
	for _, tc := range ok {
		got, err := parseSandboxMemoryBytes(tc.in)
		c.NoError(err, "parseSandboxMemoryBytes(%q)", tc.in)
		c.Eq(tc.want, got, "parseSandboxMemoryBytes(%q)", tc.in)
	}

	for _, bad := range []string{"", "-1", "abc", "1e3", "12x"} {
		_, err := parseSandboxMemoryBytes(bad)
		c.Require().Error(err, "parseSandboxMemoryBytes(%q) succeeded, want an error", bad)
		c.StrContains(err.Error(), bad, "the error must name the flag value")
	}
}

// ─── the Connect round trips ─────────────────────────────────────────────────

// sandboxStubControl serves the sandbox slice over Connect for the CLI tests:
// it returns the rows each test seeds and records every request, so a test can
// pin the request shape as well as the rendering.
type sandboxStubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	rows      []*rafikiv1.SandboxInfo
	created   *rafikiv1.SandboxInfo
	sawCreate *rafikiv1.CreateSandboxRequest
	sawList   *rafikiv1.ListSandboxesRequest
	sawRemove *rafikiv1.RemoveSandboxRequest
}

func (s *sandboxStubControl) CreateSandbox(
	_ context.Context,
	req *connect.Request[rafikiv1.CreateSandboxRequest],
) (*connect.Response[rafikiv1.CreateSandboxResponse], error) {
	s.sawCreate = req.Msg
	sb := s.created
	if sb == nil {
		sb = &rafikiv1.SandboxInfo{Id: "sb-1", Name: req.Msg.GetSpec().GetName()}
	}
	return connect.NewResponse(&rafikiv1.CreateSandboxResponse{Sandbox: sb}), nil
}

func (s *sandboxStubControl) ListSandboxes(
	_ context.Context,
	req *connect.Request[rafikiv1.ListSandboxesRequest],
) (*connect.Response[rafikiv1.ListSandboxesResponse], error) {
	s.sawList = req.Msg
	return connect.NewResponse(&rafikiv1.ListSandboxesResponse{Sandboxes: s.rows}), nil
}

func (s *sandboxStubControl) RemoveSandbox(
	_ context.Context,
	req *connect.Request[rafikiv1.RemoveSandboxRequest],
) (*connect.Response[rafikiv1.RemoveSandboxResponse], error) {
	s.sawRemove = req.Msg
	return connect.NewResponse(&rafikiv1.RemoveSandboxResponse{}), nil
}

// sandboxTestDaemon stands a stub Control handler up on a scratch profile's own
// socket, mirroring executorTestDaemon, so a CLI verb dials THAT daemon.
func sandboxTestDaemon(t *testing.T, ctl rafikiv1connect.ControlHandler) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// A short directory, not t.TempDir(): unix socket paths are capped at
	// ~104 bytes, and t.TempDir() nests under the full test name.
	dir, err := os.MkdirTemp("", "raf-sb")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(ctl)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
}

// runSandboxCLI executes one `rafiki sandbox …` argv through the real root
// command and returns what the verb wrote to stdout.
func runSandboxCLI(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"sandbox"}, args...))
	out := captureStdout(t, func() {
		assert.NewAborting(t).NoError(root.Execute(), "rafiki sandbox %v", args)
	})
	return out
}

func TestSandboxCmdCreateBuildsRequest(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := &sandboxStubControl{created: &rafikiv1.SandboxInfo{Id: "sb-9", Name: "s1", State: "creating"}}
	sandboxTestDaemon(t, srv)

	out := runSandboxCLI(t, "create",
		"--name", "s1",
		"--image", "alpine:3",
		"--launcher", "laptop",
		"--workdir", "/w",
		"--network", "none",
		"--read-only-rootfs",
		"--user", "1000",
		"--memory", "1g",
		"--cpus", "2",
		"--pids", "100",
		"--ttl", "30m",
		"--env", "A=1",
		"--env", "B=2",
		"--label", "team=core",
		"--mount", "ro:/work=host:/srv/repos/app",
		"--mount", "rw:/scratch",
		"--mount", "ephemeral:/tmp",
	)

	c.Require().NotNil(srv.sawCreate, "the CLI sent no CreateSandbox request")
	spec := srv.sawCreate.GetSpec()
	c.Require().NotNil(spec, "request carried no spec")
	c.Eq("s1", spec.GetName(), "name")
	c.Eq("alpine:3", spec.GetImage(), "image")
	c.Eq("laptop", spec.GetLauncher(), "launcher")
	c.Eq("/w", spec.GetWorkdir(), "workdir")
	c.Eq("none", spec.GetNetwork(), "network")
	c.True(spec.GetReadOnlyRootfs(), "read_only_rootfs")
	c.Eq("1000", spec.GetUser(), "user")
	c.Eq(int64(1)<<30, spec.GetMemoryBytes(), "memory_bytes")
	c.Eq(2.0, spec.GetCpus(), "cpus")
	c.Eq(int64(100), spec.GetPidsLimit(), "pids_limit")
	c.Eq(30*time.Minute, spec.GetTtl().AsDuration(), "ttl")
	c.Eq("1", spec.GetEnv()["A"], "env[A]")
	c.Eq("2", spec.GetEnv()["B"], "env[B]")
	c.Eq("core", spec.GetLabels()["team"], "labels[team]")

	mounts := spec.GetMounts()
	c.Require().Len(mounts, 3, "mounts")
	c.Eq("ro", mounts[0].GetKind(), "mounts[0].kind")
	c.Eq("/work", mounts[0].GetTarget(), "mounts[0].target")
	c.Eq("/srv/repos/app", mounts[0].GetHostPath(), "mounts[0].host_path")
	c.Eq("rw", mounts[1].GetKind(), "mounts[1].kind")
	c.Eq("", mounts[1].GetHostPath(), "mounts[1].host_path")
	c.Eq("", mounts[1].GetVolume(), "mounts[1].volume")
	c.Eq("ephemeral", mounts[2].GetKind(), "mounts[2].kind")

	// The echo is the response's canonical protojson.
	c.StrContains(out, "\"sandbox\"", "create echo must be the response protojson:\n")
	c.StrContains(out, "\"name\": \"s1\"", "create echo missing the sandbox name:\n")
}

func TestSandboxCmdCreateTTLOmitted(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := &sandboxStubControl{}
	sandboxTestDaemon(t, srv)

	runSandboxCLI(t, "create", "--name", "s2")

	c.Require().NotNil(srv.sawCreate, "the CLI sent no CreateSandbox request")
	c.Nil(srv.sawCreate.GetSpec().GetTtl(), "an omitted --ttl must leave the field unset, not zero")
}

func TestSandboxCmdCreateRequiresName(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	root := newRootCmd()
	root.SetArgs([]string{"sandbox", "create", "--image", "alpine"})
	err := root.Execute()
	c.Require().Error(err, "create without --name succeeded, want a refusal")
	c.StrContains(err.Error(), "--name", "the refusal must name the missing flag")
}

func TestSandboxCmdListOnConnect(t *testing.T) {
	expiresAt := time.UnixMilli(1700000000000)
	srv := &sandboxStubControl{rows: []*rafikiv1.SandboxInfo{
		{Id: "sb-1", Name: "alpha", State: "ready", Network: "egress", Image: "alpine:3",
			Launcher: "laptop", Connected: true, ExpiresAt: timestamppb.New(expiresAt)},
		{Id: "sb-2", Name: "beta", State: "lost", Network: "none", Image: "alpine:3",
			Launcher: "rack"},
	}}
	sandboxTestDaemon(t, srv)

	t.Run("table renders the eight columns", func(t *testing.T) {
		c := assert.NewCollecting(t)
		out := runSandboxCLI(t, "ls")
		for _, want := range []string{"alpha", "sb-1", "ready", "egress", "alpine:3", "laptop", "yes", "beta", "no"} {
			c.StrContains(out, want, "output missing")
		}
		for _, header := range []string{"NAME", "ID", "STATE", "NETWORK", "IMAGE", "LAUNCHER", "EXPIRES", "CONNECTED"} {
			c.StrContains(out, header, "missing header")
		}
		c.NotStrContains(out, "\x1b", "plain stdout must carry no ANSI escapes:\n")
		c.Require().NotNil(srv.sawList, "the CLI sent no ListSandboxes request")
	})

	t.Run("json is the canonical protojson of the response rows", func(t *testing.T) {
		c := assert.NewCollecting(t)
		out := runSandboxCLI(t, "ls", "-j")

		// Build the expected bytes the same way emitProtoRows does: the
		// {"rows":[...]} envelope, each row the canonical protojson of its
		// SandboxInfo, re-indented with writeJSON's two-space indent.
		parts := make([][]byte, len(srv.rows))
		for i, row := range srv.rows {
			b, err := marshalProtoJSON(row)
			c.Require().NoError(err, "marshalProtoJSON(row %d)", i)
			parts[i] = b
		}
		env := []byte(`{"rows":[`)
		env = append(env, bytes.Join(parts, []byte(","))...)
		env = append(env, ']', '}')
		var want bytes.Buffer
		c.Require().NoError(writeIndentedJSON(&want, env), "writeIndentedJSON")
		c.Eq(want.String(), out, "ls -j must be the canonical protojson of the response")
	})

	t.Run("jsonl is one compact row per line, no envelope", func(t *testing.T) {
		c := assert.NewCollecting(t)
		out := runSandboxCLI(t, "ls", "-J")
		lines := nonEmptyLines(out)
		c.Require().Len(lines, 2, "got %d lines, want one row per line:\n%s", len(lines), out)
		c.NotStrContains(out, "\"rows\"", "jsonl must not wrap rows in an envelope:\n")
		c.StrContains(lines[0], "\"id\":\"sb-1\"", "row order/content")
		c.StrContains(lines[1], "\"id\":\"sb-2\"", "row order/content")
	})

	t.Run("empty list", func(t *testing.T) {
		c := assert.NewCollecting(t)
		empty := &sandboxStubControl{}
		sandboxTestDaemon(t, empty)
		out := runSandboxCLI(t, "ls")
		c.StrContains(out, "No sandboxes.", "empty list output")
	})
}

func TestSandboxCmdRemove(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := &sandboxStubControl{}
	sandboxTestDaemon(t, srv)

	out := runSandboxCLI(t, "rm", "alpha")

	c.Require().NotNil(srv.sawRemove, "the CLI sent no RemoveSandbox request")
	c.Eq("alpha", srv.sawRemove.GetRef(), "ref")
	c.StrContains(out, "Sandbox alpha removed.", "remove output")
}

func TestSandboxCmdCompletions(t *testing.T) {
	c := assert.NewAborting(t)
	c.EqDiff([]string{"ro:", "rw:", "ephemeral:"}, completeSandboxMountKinds(""), "all kinds")
	c.EqDiff([]string{"ro:", "rw:"}, completeSandboxMountKinds("r"), "prefix filter")
	c.Empty(completeSandboxMountKinds("x"), "no match")
	c.EqDiff([]string{"egress", "none"}, sandboxNetworkValues, "network values")
}
