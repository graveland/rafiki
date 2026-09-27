// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// repoStubControl serves the four git-source verbs and records what arrived,
// the same shape reviewStubControl serves the review verbs through.
type repoStubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	addResp     *rafikiv1.AddPymoduleGitSourceResponse
	refreshResp *rafikiv1.RefreshPymoduleGitSourceResponse
	listResp    *rafikiv1.ListPymoduleGitSourcesResponse

	mu           sync.Mutex
	addCalls     []*rafikiv1.AddPymoduleGitSourceRequest
	refreshCalls []*rafikiv1.RefreshPymoduleGitSourceRequest
	removeCalls  []*rafikiv1.RemovePymoduleGitSourceRequest
	listCalls    int
}

func (s *repoStubControl) AddPymoduleGitSource(
	_ context.Context, req *connect.Request[rafikiv1.AddPymoduleGitSourceRequest],
) (*connect.Response[rafikiv1.AddPymoduleGitSourceResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addCalls = append(s.addCalls, req.Msg)
	return connect.NewResponse(s.addResp), nil
}

func (s *repoStubControl) ListPymoduleGitSources(
	_ context.Context, _ *connect.Request[rafikiv1.ListPymoduleGitSourcesRequest],
) (*connect.Response[rafikiv1.ListPymoduleGitSourcesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return connect.NewResponse(s.listResp), nil
}

func (s *repoStubControl) RefreshPymoduleGitSource(
	_ context.Context, req *connect.Request[rafikiv1.RefreshPymoduleGitSourceRequest],
) (*connect.Response[rafikiv1.RefreshPymoduleGitSourceResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls = append(s.refreshCalls, req.Msg)
	return connect.NewResponse(s.refreshResp), nil
}

func (s *repoStubControl) RemovePymoduleGitSource(
	_ context.Context, req *connect.Request[rafikiv1.RemovePymoduleGitSourceRequest],
) (*connect.Response[rafikiv1.RemovePymoduleGitSourceResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeCalls = append(s.removeCalls, req.Msg)
	return connect.NewResponse(&rafikiv1.RemovePymoduleGitSourceResponse{}), nil
}

func (s *repoStubControl) adds() []*rafikiv1.AddPymoduleGitSourceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.AddPymoduleGitSourceRequest(nil), s.addCalls...)
}

func (s *repoStubControl) refreshes() []*rafikiv1.RefreshPymoduleGitSourceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.RefreshPymoduleGitSourceRequest(nil), s.refreshCalls...)
}

func (s *repoStubControl) removes() []*rafikiv1.RemovePymoduleGitSourceRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.RemovePymoduleGitSourceRequest(nil), s.removeCalls...)
}

// newRepoHarness wires an isolated profile whose own socket serves the stub,
// and returns the stub.
func newRepoHarness(t *testing.T, stub *repoStubControl) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// Short dir name: unix socket paths are capped at ~104 bytes on darwin.
	dir, err := os.MkdirTemp("", "repo")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
}

func runRepoCmd(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newPythonRepoCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	return cmd.Execute()
}

// TestPythonRepoAddRendersDiscoveredCount pins the add contract end to end:
// the registration goes out first (with the CLI's --ref default applied) and
// the printed output names the source AND the discovered inventory the
// daemon's synchronous first refresh reported.
func TestPythonRepoAddRendersDiscoveredCount(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		addResp: &rafikiv1.AddPymoduleGitSourceResponse{Row: &rafikiv1.GitSourceRow{
			Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
		}},
		refreshResp: &rafikiv1.RefreshPymoduleGitSourceResponse{
			Scripts: []*rafikiv1.GitSourceScript{
				{Name: "rotate_keys", Description: "rotate the keys"},
				{Name: "deploy", Description: "deploy the thing"},
			},
			Packages:  []*rafikiv1.GitSourcePackage{{Name: "opslib", Description: "shared helpers"}},
			VenvReady: true,
		},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "add", "ops_tools", "https://example.net/ops.git"), "python repo add")
	})
	c.False(!strings.Contains(out, "ops_tools") || !strings.Contains(out, "https://example.net/ops.git"), "add output does not name the source:\n%s", out)
	c.False(!strings.Contains(out, "2 script(s)") || !strings.Contains(out, "1 package(s)"), "add output does not report the discovered inventory:\n%s", out)

	// The refresh that produced the summary named the registered source.
	adds := stub.adds()
	c.Require().False(len(adds) != 1 || adds[0].GetName() != "ops_tools" || adds[0].GetUrl() != "https://example.net/ops.git" || adds[0].GetRef() != "main", "AddPymoduleGitSource request = %+v, want ops_tools/url with the --ref main default", adds)
	refreshes := stub.refreshes()
	c.Require().False(len(refreshes) != 1 || refreshes[0].GetName() != "ops_tools", "RefreshPymoduleGitSource requests = %v, want exactly one for ops_tools", refreshes)
}

// TestPythonRepoRefreshRendersVenvFailure pins the other half of the summary:
// a venv that failed to build is printed, and does not fail the command —
// the refresh succeeded, only the build didn't.
func TestPythonRepoRefreshRendersVenvFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		refreshResp: &rafikiv1.RefreshPymoduleGitSourceResponse{
			Scripts:   []*rafikiv1.GitSourceScript{{Name: "rotate_keys"}},
			Packages:  []*rafikiv1.GitSourcePackage{},
			VenvReady: false,
			VenvError: "uv sync failed: no solution found",
		},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "refresh", "ops_tools"), "python repo refresh")
	})
	c.StrContains(out, "refreshed ops_tools", "refresh output does not name the source:\n")
	c.StrContains(out, "1 script(s)", "refresh output does not report the discovered count:\n")
	c.StrContains(out, "venv build failed: uv sync failed: no solution found", "refresh output does not carry the venv error:\n")
	if got := len(stub.refreshes()); got != 1 || stub.refreshes()[0].GetName() != "ops_tools" {
		t.Errorf("refresh requests = %v, want one for ops_tools", stub.refreshes())
	}
}

// TestPythonRepoListRendersTable pins the list table: NAME, URL, REF — and
// that a missing cell renders "-" rather than a blank.
func TestPythonRepoListRendersTable(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListPymoduleGitSourcesResponse{Rows: []*rafikiv1.GitSourceRow{
			{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
			{Name: "shared_lib", Url: "https://example.net/lib.git"},
		}},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "list"), "python repo list")
	})
	for _, want := range []string{"NAME", "URL", "REF", "ops_tools", "https://example.net/ops.git", "main", "shared_lib"} {
		c.StrContains(out, want, "list output missing")
	}
	// A row registered with no ref renders "-", not an empty cell.
	c.StrContains(out, "-", "list output has no dash fallback for the refless row:\n")
	c.Eq(1, stub.listCalls, "ListPymoduleGitSources called")
}

// TestPythonRepoRemoveNamesTheSource: remove sends the name and confirms.
func TestPythonRepoRemoveNamesTheSource(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "remove", "ops_tools"), "python repo remove")
	})
	c.StrContains(out, "removed ops_tools", "remove output")
	if got := stub.removes(); len(got) != 1 || got[0].GetName() != "ops_tools" {
		t.Errorf("RemovePymoduleGitSource requests = %v, want one for ops_tools", got)
	}
}

// The group's own tree: exactly the four verbs, and the aliases the CLI
// output contract implies (remove carries rm; the group has none).
func TestPythonRepoCommandTree(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newPythonRepoCmd()
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	for _, want := range []string{"add", "list", "refresh", "remove"} {
		c.False(!got[want], "python repo subcommands missing %q (have %v)", want, got)
	}
	c.Len(got, 4, "python repo subcommands")

	// Arg contracts: add takes name+url, the name-verbs take exactly one, and
	// list takes none — validation fails before any endpoint is touched, so
	// these run without a harness.
	for _, args := range [][]string{
		{"add"},                    // missing url
		{"add", "only_name"},       // missing url
		{"add", "a", "u", "extra"}, // too many
		{"refresh"},                // missing name
		{"refresh", "a", "b"},      // too many
		{"remove"},                 // missing name
		{"remove", "a", "b"},       // too many
		{"list", "extra"},          // list takes none
	} {
		c.Error(runRepoCmd(t, args...), "python repo %v: executed without error, want an arg refusal", args)
	}

	// The name pre-check the add verb performs locally: "local" is reserved
	// for the blob store and a non-identifier never becomes a `repo` value —
	// both refused before any round trip.
	stub := &repoStubControl{}
	newRepoHarness(t, stub)
	for _, name := range []string{"local", "9bad"} {
		c.Error(runRepoCmd(t, "add", name, "https://example.net/x.git"), "python repo add %q: accepted, want the name refusal", name)
	}
	c.Empty(stub.addCalls, "the daemon was called despite the local name refusals")
}

// TestPythonRepoSummaryJSONLIsOneCompactLine pins the -J contract for the
// add/refresh summary emitter: ONE compact record, no envelope, no
// indentation — the shape `emitPymoduleCode` emits in the same mode, never
// the indented -j form.
func TestPythonRepoSummaryJSONLIsOneCompactLine(t *testing.T) {
	c := assert.NewCollecting(t)
	summary := func() *rafikiv1.RefreshPymoduleGitSourceResponse {
		return &rafikiv1.RefreshPymoduleGitSourceResponse{
			Scripts: []*rafikiv1.GitSourceScript{
				{Name: "rotate_keys"},
				{Name: "deploy"},
			},
			Packages:  []*rafikiv1.GitSourcePackage{{Name: "opslib"}},
			VenvReady: true,
		}
	}

	// The emitter itself, both ways around: -j indents, -J stays one line.
	var jBuf, jlBuf bytes.Buffer
	c.Require().NoError(emitGitSourceSummary(&jBuf, "refreshed ops_tools", summary(), outputJSON))
	c.Require().NoError(emitGitSourceSummary(&jlBuf, "refreshed ops_tools", summary(), outputJSONL))
	if lines := strings.Split(strings.TrimRight(jlBuf.String(), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("emitGitSourceSummary -J printed %d line(s), want one compact record:\n%s", len(lines), jlBuf.String())
	}
	c.StrContains(jBuf.String(), "\n  ", "emitGitSourceSummary -j lost its indentation (the shape that distinguishes the two modes):\n")

	// End to end: `python repo add <name> <url> -J`. The -J flag lives on the
	// root's persistent flag set, so the command runs under a root carrying
	// the same persistent-flag registrations the real binary has (the same
	// trick newTestRoot uses for the profile flag).
	stub := &repoStubControl{
		addResp: &rafikiv1.AddPymoduleGitSourceResponse{Row: &rafikiv1.GitSourceRow{
			Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
		}},
		refreshResp: summary(),
	}
	newRepoHarness(t, stub)

	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.PersistentFlags().StringP("output", "o", "auto", "")
	root.PersistentFlags().BoolP("json", "j", false, "")
	root.PersistentFlags().BoolP("jsonl", "J", false, "")
	root.AddCommand(newPythonCmd()) // the real python group, which now carries repo
	root.SetArgs([]string{"python", "repo", "add", "ops_tools", "https://example.net/ops.git", "-J"})

	out := captureStdout(t, func() {
		c.Require().NoError(root.Execute(), "python repo add -J")
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	c.Require().Len(lines, 1, "add -J printed %d line(s), want exactly one compact record:\n%s", len(lines), out)
	var resp rafikiv1.RefreshPymoduleGitSourceResponse
	err := json.Unmarshal([]byte(lines[0]), &resp)
	c.Require().NoError(err, "add -J line is not a bare record: %v\n%s", err, lines[0])
	if len(resp.GetScripts()) != 2 || len(resp.GetPackages()) != 1 {
		t.Errorf("add -J record carries scripts=%d packages=%d, want the discovered inventory", len(resp.GetScripts()), len(resp.GetPackages()))
	}
}

// The rendered table cells, tested without a daemon: the header names the
// three columns and a URL/refless row falls back to "-".
func TestPythonRepoListCells(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf strings.Builder
	c.Require().NoError(renderGitSourceList(&buf, []*rafikiv1.GitSourceRow{
		{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
		{Name: "shared_lib", Url: "https://example.net/lib.git"},
	}, false))
	out := buf.String()
	for _, want := range []string{"NAME", "URL", "REF", "ops_tools", "main", "shared_lib"} {
		c.StrContains(out, want, "renderGitSourceList output missing")
	}
}

// TestPythonRepoAddRejectsLocalNameBeforeDial is folded into
// TestPythonRepoCommandTree's arg loop; this keeps the pinned name so the
// reviewer's expectation ("add refuses reserved names locally") has a named
// anchor.
func TestPythonRepoAddRejectsLocalNameBeforeDial(t *testing.T) {
	t.Run("reserved and malformed names", func(t *testing.T) {
		c := assert.NewCollecting(t)
		stub := &repoStubControl{}
		newRepoHarness(t, stub)
		for _, name := range []string{"local", "9bad"} {
			c.Error(runRepoCmd(t, "add", name, "https://example.net/x.git"), "python repo add %q: accepted", name)
		}
		c.Empty(stub.addCalls, "daemon called despite local refusals")
	})
}
