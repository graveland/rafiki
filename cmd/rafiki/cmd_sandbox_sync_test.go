// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	"github.com/multigres/testkit/assert"
)

// ─── the endpoint parser ──────────────────────────────────────────────────────

func TestSplitSyncEndpoint(t *testing.T) {
	c := assert.NewAborting(t)

	ok := []struct {
		in       string
		executor string
		path     string
	}{
		{"greyshift:/Users/x/repo", "greyshift", "/Users/x/repo"},
		{"sbx-1:/work/repo", "sbx-1", "/work/repo"},
		// Split at the FIRST ":" only: a colon inside the path is the path's.
		{"host:/a:b/c", "host", "/a:b/c"},
		{"host:/", "host", "/"},
	}
	for _, tc := range ok {
		executor, path, err := splitSyncEndpoint(tc.in)
		c.NoError(err, "splitSyncEndpoint(%q)", tc.in)
		c.Eq(tc.executor, executor, "splitSyncEndpoint(%q) executor", tc.in)
		c.Eq(tc.path, path, "splitSyncEndpoint(%q) path", tc.in)
	}

	// Every refusal must name the offending argument and name the wanted
	// shape.
	for _, bad := range []string{":/x", "host:rel", "noColon", "a/b:/x", "host:", ":/", ""} {
		_, _, err := splitSyncEndpoint(bad)
		c.Require().Error(err, "splitSyncEndpoint(%q) succeeded, want an error", bad)
		c.StrContains(err.Error(), bad, "the error must name the argument")
		c.StrContains(err.Error(), "want <executor>:<absolute path>", "the error must name the wanted shape")
	}
}

// ─── registration and help ───────────────────────────────────────────────────

// TestSandboxCmdRegistersSyncVerbs pins that both verbs are findable under
// `sandbox` and that the noun's help lists them, so a new verb cannot be
// implemented and left unreachable.
func TestSandboxCmdRegistersSyncVerbs(t *testing.T) {
	c := assert.NewAborting(t)

	sandbox := newSandboxCmd()
	for _, name := range []string{"sync", "sync-repo"} {
		found, _, err := sandbox.Find([]string{name})
		c.Require().NoError(err, "sandbox has no subcommand %q", name)
		c.Eq(name, found.Name(), "the resolved subcommand must be %q", name)
	}
	c.StrContains(sandbox.Long, "sync", "the noun help must list sync")
	c.StrContains(sandbox.Long, "sync-repo", "the noun help must list sync-repo")
}

// ─── the Connect round trips ─────────────────────────────────────────────────

// syncStubControl serves the sync verbs over Connect for the CLI tests: it
// records every request so a test can pin the request shape, and returns the
// response each test seeds.
type syncStubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	sawPath *rafikiv1.SyncPathRequest
	pathRes *rafikiv1.SyncPathResponse

	sawRepo *rafikiv1.SyncRepoRequest
	repoRes *rafikiv1.SyncRepoResponse
}

func (s *syncStubControl) SyncPath(
	_ context.Context,
	req *connect.Request[rafikiv1.SyncPathRequest],
) (*connect.Response[rafikiv1.SyncPathResponse], error) {
	s.sawPath = req.Msg
	res := s.pathRes
	if res == nil {
		res = &rafikiv1.SyncPathResponse{Files: 3, Bytes: 42}
	}
	return connect.NewResponse(res), nil
}

func (s *syncStubControl) SyncRepo(
	_ context.Context,
	req *connect.Request[rafikiv1.SyncRepoRequest],
) (*connect.Response[rafikiv1.SyncRepoResponse], error) {
	s.sawRepo = req.Msg
	res := s.repoRes
	if res == nil {
		res = &rafikiv1.SyncRepoResponse{}
	}
	return connect.NewResponse(res), nil
}

func TestSandboxSyncRequestMapping(t *testing.T) {
	t.Run("flags map to the proto request", func(t *testing.T) {
		c := assert.NewCollecting(t)
		srv := &syncStubControl{}
		sandboxTestDaemon(t, srv)

		out := runSandboxCLI(t, "sync", "greyshift:/Users/x/repo", "sbx-1:/work/repo",
			"--overwrite", "--max-bytes", "1024")

		c.Require().NotNil(srv.sawPath, "the CLI sent no SyncPath request")
		c.Eq("greyshift", srv.sawPath.GetSrc().GetExecutor(), "src.executor")
		c.Eq("/Users/x/repo", srv.sawPath.GetSrc().GetPath(), "src.path")
		c.Eq("sbx-1", srv.sawPath.GetDst().GetExecutor(), "dst.executor")
		c.Eq("/work/repo", srv.sawPath.GetDst().GetPath(), "dst.path")
		c.True(srv.sawPath.GetOverwrite(), "overwrite")
		c.Require().NotNil(srv.sawPath.MaxBytes, "a supplied --max-bytes must be present on the wire")
		c.Eq(int64(1024), srv.sawPath.GetMaxBytes(), "max_bytes")

		c.StrContains(out, "copied 3 files, 42 bytes", "table output:\n")
	})

	t.Run("max-bytes absent leaves the optional field unset", func(t *testing.T) {
		c := assert.NewCollecting(t)
		srv := &syncStubControl{}
		sandboxTestDaemon(t, srv)

		runSandboxCLI(t, "sync", "host:/a", "host:/b")

		c.Require().NotNil(srv.sawPath, "the CLI sent no SyncPath request")
		c.Nil(srv.sawPath.MaxBytes, "an omitted --max-bytes must leave the field unset, not zero")
		c.False(srv.sawPath.GetOverwrite(), "overwrite defaults off")
	})

	t.Run("json is the canonical protojson of the response", func(t *testing.T) {
		c := assert.NewCollecting(t)
		res := &rafikiv1.SyncPathResponse{Files: 7, Bytes: 999}
		srv := &syncStubControl{pathRes: res}
		sandboxTestDaemon(t, srv)

		out := runSandboxCLI(t, "sync", "host:/a", "host:/b", "-j")

		b, err := marshalProtoJSON(res)
		c.Require().NoError(err, "marshalProtoJSON")
		var want bytes.Buffer
		c.Require().NoError(writeIndentedJSON(&want, b), "writeIndentedJSON")
		c.Eq(want.String(), out, "sync -j must be the canonical protojson of the response")
	})

	t.Run("jsonl is one compact line", func(t *testing.T) {
		c := assert.NewCollecting(t)
		res := &rafikiv1.SyncPathResponse{Files: 7, Bytes: 999}
		srv := &syncStubControl{pathRes: res}
		sandboxTestDaemon(t, srv)

		out := runSandboxCLI(t, "sync", "host:/a", "host:/b", "-J")

		b, err := marshalProtoJSON(res)
		c.Require().NoError(err, "marshalProtoJSON")
		c.Eq(string(b)+"\n", out, "sync -J must be the compact protojson of the response")
	})
}

func TestSandboxSyncRepoRequestMapping(t *testing.T) {
	t.Run("flags map to the proto request", func(t *testing.T) {
		c := assert.NewCollecting(t)
		srv := &syncStubControl{}
		sandboxTestDaemon(t, srv)

		runSandboxCLI(t, "sync-repo", "greyshift:/Users/x/repo", "sbx-1:/work/repo", "main", "--force")

		c.Require().NotNil(srv.sawRepo, "the CLI sent no SyncRepo request")
		c.Eq("greyshift", srv.sawRepo.GetSrc().GetExecutor(), "src.executor")
		c.Eq("/Users/x/repo", srv.sawRepo.GetSrc().GetPath(), "src.path")
		c.Eq("sbx-1", srv.sawRepo.GetDst().GetExecutor(), "dst.executor")
		c.Eq("/work/repo", srv.sawRepo.GetDst().GetPath(), "dst.path")
		c.Eq("main", srv.sawRepo.GetBranch(), "branch")
		c.True(srv.sawRepo.GetForce(), "force")
	})

	t.Run("force defaults off", func(t *testing.T) {
		c := assert.NewCollecting(t)
		srv := &syncStubControl{}
		sandboxTestDaemon(t, srv)

		runSandboxCLI(t, "sync-repo", "host:/a", "host:/b", "main")

		c.Require().NotNil(srv.sawRepo, "the CLI sent no SyncRepo request")
		c.False(srv.sawRepo.GetForce(), "force defaults off")
	})

	t.Run("table renders created, up-to-date and transferred", func(t *testing.T) {
		cases := []struct {
			name string
			res  *rafikiv1.SyncRepoResponse
			want string
		}{
			{"created repo", &rafikiv1.SyncRepoResponse{CreatedRepo: true}, "created repo\n"},
			{"up to date", &rafikiv1.SyncRepoResponse{UpToDate: true}, "up to date\n"},
			{
				"created branch abbreviates the new oid and prints (new)",
				&rafikiv1.SyncRepoResponse{OldOid: "", NewOid: "abcdef1234567890"},
				"(new)..abcdef123456\n",
			},
			{
				"transfer abbreviates both oids",
				&rafikiv1.SyncRepoResponse{OldOid: "1111111111112222", NewOid: "3333333333334444"},
				"111111111111..333333333333\n",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := assert.NewCollecting(t)
				srv := &syncStubControl{repoRes: tc.res}
				sandboxTestDaemon(t, srv)
				out := runSandboxCLI(t, "sync-repo", "host:/a", "host:/b", "main")
				c.Eq(tc.want, out, "sync-repo table")
			})
		}
	})

	t.Run("json is the canonical protojson of the response", func(t *testing.T) {
		c := assert.NewCollecting(t)
		res := &rafikiv1.SyncRepoResponse{OldOid: "1111", NewOid: "2222", CreatedRepo: false}
		srv := &syncStubControl{repoRes: res}
		sandboxTestDaemon(t, srv)

		out := runSandboxCLI(t, "sync-repo", "host:/a", "host:/b", "main", "-j")

		b, err := marshalProtoJSON(res)
		c.Require().NoError(err, "marshalProtoJSON")
		var want bytes.Buffer
		c.Require().NoError(writeIndentedJSON(&want, b), "writeIndentedJSON")
		c.Eq(want.String(), out, "sync-repo -j must be the canonical protojson of the response")
	})
}

func TestSandboxSyncRejectsNonPositiveMaxBytes(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// The refusal is client-side, before any round trip: the endpoint is never
	// dialled, so a value that is not positive never reaches the daemon.
	for _, bad := range []string{"0", "-1"} {
		root := newRootCmd()
		root.SetArgs([]string{"sandbox", "sync", "host:/a", "host:/b", "--max-bytes", bad})
		err := root.Execute()
		c.Require().Error(err, "--max-bytes %s succeeded, want a refusal", bad)
		c.StrContains(err.Error(), "--max-bytes must be > 0", "--max-bytes %s refusal", bad)
	}
}

func TestSandboxSyncRejectsBadEndpoint(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	root := newRootCmd()
	root.SetArgs([]string{"sandbox", "sync", "host:rel", "host:/b"})
	err := root.Execute()
	c.Require().Error(err, "a non-absolute path succeeded, want a refusal")
	c.StrContains(err.Error(), "host:rel", "the refusal must name the offending argument")
}
