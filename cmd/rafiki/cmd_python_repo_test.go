// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	// refreshErr fails the named source only, so a multi-source run's
	// partial-failure path is testable.
	refreshErr map[string]error

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
	if err := s.refreshErr[req.Msg.GetName()]; err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Echo the request's name exactly as the daemon does, so a multi-source
	// run's rows attribute themselves.
	out := &rafikiv1.RefreshPymoduleGitSourceResponse{Name: req.Msg.GetName()}
	if s.refreshResp != nil {
		out.Scripts = s.refreshResp.GetScripts()
		out.Packages = s.refreshResp.GetPackages()
		out.VenvReady = s.refreshResp.GetVenvReady()
		out.VenvError = s.refreshResp.GetVenvError()
	}
	return connect.NewResponse(out), nil
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

	// Arg contracts: add takes name+url, remove takes exactly one, and list
	// takes none — validation fails before any endpoint is touched, so these
	// run without a harness. `refresh` is deliberately ABSENT: it takes any
	// number of names, and none means "every registered source", so neither a
	// bare `refresh` nor `refresh a b` is an argument error any more (pinned by
	// TestPythonRepoRefreshAllSources / TestPythonRepoRefreshNamedSources).
	for _, args := range [][]string{
		{"add"},                    // missing url
		{"add", "only_name"},       // missing url
		{"add", "a", "u", "extra"}, // too many
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
	for _, name := range []string{"local", "bad.name"} {
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
		for _, name := range []string{"local", "bad.name"} {
			c.Error(runRepoCmd(t, "add", name, "https://example.net/x.git"), "python repo add %q: accepted", name)
		}
		c.Empty(stub.addCalls, "daemon called despite local refusals")
	})
}

// TestPythonRepoRefreshAllSources pins the no-argument contract: `refresh`
// with no names lists the caller's sources ONCE and refreshes every one of
// them, in list order, naming each in the output.
func TestPythonRepoRefreshAllSources(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListPymoduleGitSourcesResponse{Rows: []*rafikiv1.GitSourceRow{
			{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
			{Name: "shared_lib", Url: "https://example.net/lib.git", Ref: "main"},
		}},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "refresh"), "python repo refresh (no names)")
	})
	c.Eq(1, stub.listCalls, "ListPymoduleGitSources calls for a whole-catalogue refresh")
	refreshes := stub.refreshes()
	c.Require().Len(refreshes, 2, "RefreshPymoduleGitSource calls")
	c.Eq("ops_tools", refreshes[0].GetName(), "first refreshed source")
	c.Eq("shared_lib", refreshes[1].GetName(), "second refreshed source")
	c.StrContains(out, "refreshed ops_tools", "refresh-all output missing the first source:\n")
	c.StrContains(out, "refreshed shared_lib", "refresh-all output missing the second source:\n")
}

// TestPythonRepoRefreshNamedSources pins the other half: naming sources
// refreshes exactly those and never consults the catalogue.
func TestPythonRepoRefreshNamedSources(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{}
	newRepoHarness(t, stub)

	c.Require().NoError(runRepoCmd(t, "refresh", "shared_lib", "ops_tools"), "python repo refresh a b")
	c.Eq(0, stub.listCalls, "ListPymoduleGitSources must not be called when names are given")
	refreshes := stub.refreshes()
	c.Require().Len(refreshes, 2, "RefreshPymoduleGitSource calls")
	c.Eq("shared_lib", refreshes[0].GetName(), "names are refreshed in the order given")
	c.Eq("ops_tools", refreshes[1].GetName(), "names are refreshed in the order given")
}

// TestPythonRepoRefreshNoSources pins the empty catalogue: nothing to do is
// not an error, and no refresh is attempted.
func TestPythonRepoRefreshNoSources(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{listResp: &rafikiv1.ListPymoduleGitSourcesResponse{}}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "refresh"), "python repo refresh with nothing registered")
	})
	c.StrContains(out, "no git sources to refresh", "empty-refresh output:\n")
	c.Empty(stub.refreshes(), "refreshed a source despite an empty catalogue")
}

// TestPythonRepoRefreshCollectsPerSourceFailures pins the partial-failure
// contract: one source failing names itself on stderr, the others still
// refresh, and the command exits non-zero.
func TestPythonRepoRefreshCollectsPerSourceFailures(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListPymoduleGitSourcesResponse{Rows: []*rafikiv1.GitSourceRow{
			{Name: "ops_tools"}, {Name: "broken"}, {Name: "shared_lib"},
		}},
		refreshErr: map[string]error{"broken": errors.New("clone failed")},
	}
	newRepoHarness(t, stub)

	stderr := captureStderr(t)
	var err error
	out := captureStdout(t, func() {
		err = runRepoCmd(t, "refresh")
	})
	c.Require().Error(err, "a failed source must make the command exit non-zero")
	c.StrContains(stderr(), "error: refresh broken", "stderr does not name the failed source:\n")
	c.StrContains(out, "refreshed ops_tools", "a sibling source was not refreshed:\n")
	c.StrContains(out, "refreshed shared_lib", "a sibling source was not refreshed:\n")
	// All three were attempted, in order — a failure does not stop the run.
	refreshes := stub.refreshes()
	c.Require().Len(refreshes, 3, "every source is attempted")
	c.Eq("broken", refreshes[1].GetName(), "sources are attempted in list order")
}

// TestPythonRepoRefreshJSONRowsAreNamed pins the -J contract for a
// multi-source run: one compact, self-describing record per source — the
// response carries its own name, so no client-side output struct is needed.
func TestPythonRepoRefreshJSONRowsAreNamed(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListPymoduleGitSourcesResponse{Rows: []*rafikiv1.GitSourceRow{
			{Name: "ops_tools"}, {Name: "shared_lib"},
		}},
		refreshResp: &rafikiv1.RefreshPymoduleGitSourceResponse{
			Scripts:   []*rafikiv1.GitSourceScript{{Name: "rotate_keys"}},
			VenvReady: true,
		},
	}
	newRepoHarness(t, stub)

	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.PersistentFlags().StringP("output", "o", "auto", "")
	root.PersistentFlags().BoolP("json", "j", false, "")
	root.PersistentFlags().BoolP("jsonl", "J", false, "")
	root.AddCommand(newPythonCmd())
	root.SetArgs([]string{"python", "repo", "refresh", "-J"})

	out := captureStdout(t, func() {
		c.Require().NoError(root.Execute(), "python repo refresh -J")
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	c.Require().Len(lines, 2, "refresh -J printed %d line(s), want one per source:\n%s", len(lines), out)
	var names []string
	for i, line := range lines {
		var resp rafikiv1.RefreshPymoduleGitSourceResponse
		c.Require().NoError(json.Unmarshal([]byte(line), &resp), "refresh -J line %d is not a bare record: %v", i, line)
		names = append(names, resp.GetName())
	}
	c.EqDeep([]string{"ops_tools", "shared_lib"}, names, "refresh -J rows are not self-describing")
}

// TestPythonRepoRefreshCompletesSourceNames pins that the verb actually wires
// a completion function and that it offers the registered source names.
func TestPythonRepoRefreshCompletesSourceNames(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListPymoduleGitSourcesResponse{Rows: []*rafikiv1.GitSourceRow{
			{Name: "ops_tools"}, {Name: "shared_lib"},
		}},
	}
	newRepoHarness(t, stub)

	var refresh *cobra.Command
	for _, sub := range newPythonRepoCmd().Commands() {
		if sub.Name() == "refresh" {
			refresh = sub
		}
	}
	c.Require().NotNil(refresh, "python repo refresh not found")
	c.Require().NotNil(refresh.ValidArgsFunction, "refresh has no ValidArgsFunction — `rafiki python repo refresh <TAB>` completes nothing")
	got, directive := refresh.ValidArgsFunction(refresh, nil, "")
	c.EqDeep([]string{"ops_tools", "shared_lib"}, got, "refresh completion candidates")
	c.Eq(cobra.ShellCompDirectiveNoFileComp, directive, "completion directive")

	// A prefix narrows the candidates, and a second position keeps offering
	// them (refresh takes any number of names).
	got, _ = refresh.ValidArgsFunction(refresh, nil, "shared")
	c.EqDeep([]string{"shared_lib"}, got, "prefix-filtered completion candidates")
	got, _ = refresh.ValidArgsFunction(refresh, []string{"ops_tools"}, "")
	c.EqDeep([]string{"ops_tools", "shared_lib"}, got, "completion after a first name")
}
