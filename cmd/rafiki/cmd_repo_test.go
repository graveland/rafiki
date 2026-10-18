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
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// repoStubControl serves the four repo verbs and records what arrived,
// the same shape reviewStubControl serves the review verbs through.
type repoStubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	addResp     *rafikiv1.AddRepoResponse
	refreshResp *rafikiv1.RefreshRepoResponse
	listResp    *rafikiv1.ListReposResponse
	setResp     *rafikiv1.SetRepoResponse

	// refreshErr fails the named source only, so a multi-source run's
	// partial-failure path is testable.
	refreshErr map[string]error

	mu           sync.Mutex
	addCalls     []*rafikiv1.AddRepoRequest
	refreshCalls []*rafikiv1.RefreshRepoRequest
	removeCalls  []*rafikiv1.RemoveRepoRequest
	setCalls     []*rafikiv1.SetRepoRequest
	listCalls    int
}

func (s *repoStubControl) AddRepo(
	_ context.Context, req *connect.Request[rafikiv1.AddRepoRequest],
) (*connect.Response[rafikiv1.AddRepoResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addCalls = append(s.addCalls, req.Msg)
	return connect.NewResponse(s.addResp), nil
}

func (s *repoStubControl) ListRepos(
	_ context.Context, _ *connect.Request[rafikiv1.ListReposRequest],
) (*connect.Response[rafikiv1.ListReposResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return connect.NewResponse(s.listResp), nil
}

func (s *repoStubControl) RefreshRepo(
	_ context.Context, req *connect.Request[rafikiv1.RefreshRepoRequest],
) (*connect.Response[rafikiv1.RefreshRepoResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls = append(s.refreshCalls, req.Msg)
	if err := s.refreshErr[req.Msg.GetName()]; err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Echo the request's name exactly as the daemon does, so a multi-source
	// run's rows attribute themselves.
	out := &rafikiv1.RefreshRepoResponse{Name: req.Msg.GetName()}
	if s.refreshResp != nil {
		out.Scripts = s.refreshResp.GetScripts()
		out.Packages = s.refreshResp.GetPackages()
		out.VenvReady = s.refreshResp.GetVenvReady()
		out.VenvError = s.refreshResp.GetVenvError()
		out.Commit = s.refreshResp.GetCommit()
		out.Unchanged = s.refreshResp.GetUnchanged()
		out.Skills = s.refreshResp.GetSkills()
		out.SkillErrors = s.refreshResp.GetSkillErrors()
	}
	return connect.NewResponse(out), nil
}

func (s *repoStubControl) SetRepo(
	_ context.Context, req *connect.Request[rafikiv1.SetRepoRequest],
) (*connect.Response[rafikiv1.SetRepoResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCalls = append(s.setCalls, req.Msg)
	if s.setResp != nil {
		return connect.NewResponse(s.setResp), nil
	}
	return connect.NewResponse(&rafikiv1.SetRepoResponse{Row: &rafikiv1.RepoRow{Name: req.Msg.GetName()}}), nil
}

func (s *repoStubControl) sets() []*rafikiv1.SetRepoRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.SetRepoRequest(nil), s.setCalls...)
}

func (s *repoStubControl) RemoveRepo(
	_ context.Context, req *connect.Request[rafikiv1.RemoveRepoRequest],
) (*connect.Response[rafikiv1.RemoveRepoResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeCalls = append(s.removeCalls, req.Msg)
	return connect.NewResponse(&rafikiv1.RemoveRepoResponse{}), nil
}

func (s *repoStubControl) adds() []*rafikiv1.AddRepoRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.AddRepoRequest(nil), s.addCalls...)
}

func (s *repoStubControl) refreshes() []*rafikiv1.RefreshRepoRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.RefreshRepoRequest(nil), s.refreshCalls...)
}

func (s *repoStubControl) removes() []*rafikiv1.RemoveRepoRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.RemoveRepoRequest(nil), s.removeCalls...)
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
	cmd := newRepoCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	return cmd.Execute()
}

// TestRepoAddRendersDiscoveredCount pins the add contract end to end:
// the registration goes out first (with the CLI's --ref default applied) and
// the printed output is the one summary line the contract names —
// `refreshed <name> at <commit[:12]>: N script(s), N package(s), N skill(s)` —
// with the commit and skill count from the registration's own refresh and the
// script/package counts from the follow-up refresh.
func TestRepoAddRendersDiscoveredCount(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		addResp: &rafikiv1.AddRepoResponse{
			Row: &rafikiv1.RepoRow{
				Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
			},
			Commit: "0123456789abcdef0123456789abcdef01234567",
			Skills: 3,
		},
		refreshResp: &rafikiv1.RefreshRepoResponse{
			Scripts: []*rafikiv1.RepoScript{
				{Name: "rotate_keys", Description: "rotate the keys"},
				{Name: "deploy", Description: "deploy the thing"},
			},
			Packages:  []*rafikiv1.RepoPackage{{Name: "opslib", Description: "shared helpers"}},
			VenvReady: true,
		},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "add", "ops_tools", "https://example.net/ops.git"), "repo add")
	})
	c.StrContains(out, "refreshed ops_tools at 0123456789ab: 2 script(s), 1 package(s), 3 skill(s)", "add output:\n")

	// The refresh that produced the counts named the registered source.
	adds := stub.adds()
	c.Require().False(len(adds) != 1 || adds[0].GetName() != "ops_tools" || adds[0].GetUrl() != "https://example.net/ops.git" || adds[0].GetRef() != "main", "AddRepo request = %+v, want ops_tools/url with the --ref main default", adds)
	refreshes := stub.refreshes()
	c.Require().False(len(refreshes) != 1 || refreshes[0].GetName() != "ops_tools", "RefreshRepo requests = %v, want exactly one for ops_tools", refreshes)

	// --refresh carries the interval onto the registration; absent leaves it
	// off (on demand), which is the wire's zero.
	c.Require().NoError(runRepoCmd(t, "add", "scheduled", "https://example.net/s.git", "--refresh", "15m"))
	adds = stub.adds()
	c.Require().Len(adds, 2, "AddRepo calls")
	c.Eq(15*time.Minute, adds[1].GetRefreshInterval().AsDuration(), "AddRepo refresh interval")

	// A too-short interval is refused locally, before the round trip.
	c.Error(runRepoCmd(t, "add", "short", "https://example.net/s.git", "--refresh", "30s"), "repo add --refresh 30s: accepted")
	c.Len(stub.adds(), 2, "the daemon was called despite the refused interval")
}

// TestRepoRefreshRendersVenvFailure pins the other half of the summary:
// a venv that failed to build is printed, and does not fail the command —
// the refresh succeeded, only the build didn't.
func TestRepoRefreshRendersVenvFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		refreshResp: &rafikiv1.RefreshRepoResponse{
			Scripts:   []*rafikiv1.RepoScript{{Name: "rotate_keys"}},
			Packages:  []*rafikiv1.RepoPackage{},
			VenvReady: false,
			VenvError: "uv sync failed: no solution found",
		},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "refresh", "ops_tools"), "repo refresh")
	})
	c.StrContains(out, "refreshed ops_tools", "refresh output does not name the source:\n")
	c.StrContains(out, "1 script(s)", "refresh output does not report the discovered count:\n")
	c.StrContains(out, "venv build failed: uv sync failed: no solution found", "refresh output does not carry the venv error:\n")
	if got := len(stub.refreshes()); got != 1 || stub.refreshes()[0].GetName() != "ops_tools" {
		t.Errorf("refresh requests = %v, want one for ops_tools", stub.refreshes())
	}
}

// TestRepoListRendersTable pins the list table: NAME, URL, REF — and
// that a missing cell renders "-" rather than a blank.
func TestRepoListRendersTable(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListReposResponse{Rows: []*rafikiv1.RepoRow{
			{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
			{Name: "shared_lib", Url: "https://example.net/lib.git"},
		}},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "list"), "repo list")
	})
	for _, want := range []string{"NAME", "URL", "REF", "ops_tools", "https://example.net/ops.git", "main", "shared_lib"} {
		c.StrContains(out, want, "list output missing")
	}
	// A row registered with no ref renders "-", not an empty cell.
	c.StrContains(out, "-", "list output has no dash fallback for the refless row:\n")
	c.Eq(1, stub.listCalls, "ListRepos called")
}

// TestRepoRemoveNamesTheSource: remove sends the name and confirms.
func TestRepoRemoveNamesTheSource(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "remove", "ops_tools"), "repo remove")
	})
	c.StrContains(out, "removed ops_tools", "remove output")
	if got := stub.removes(); len(got) != 1 || got[0].GetName() != "ops_tools" {
		t.Errorf("RemoveRepo requests = %v, want one for ops_tools", got)
	}
}

// The group's own tree: exactly the four verbs, and the aliases the CLI
// output contract implies (remove carries rm; the group has none).
// TestPythonRepoIsRetired: the old `python repo` tree fails with a pointer to
// the new one instead of cobra's unknown-command help, and it does so for any
// verb/flag shape the old tree accepted (DisableFlagParsing keeps the flags
// from being refused before RunE).
func TestPythonRepoIsRetired(t *testing.T) {
	c := assert.NewAborting(t)
	for _, argv := range [][]string{
		{"python", "repo"},
		{"python", "repo", "add", "x", "https://example.net/x.git"},
		{"python", "repo", "list", "-J"},
	} {
		root := &cobra.Command{Use: "rafiki"}
		root.AddCommand(newPythonCmd())
		root.SetArgs(argv)
		root.SetOut(&strings.Builder{})
		root.SetErr(&strings.Builder{})
		err := root.Execute()
		c.Require().Error(err, "%v was accepted", argv)
		c.Eq(`"rafiki python repo" is now "rafiki repo"`, err.Error(), "retired-command error for %v", argv)
	}
}

func TestRepoCommandTree(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newRepoCmd()
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	for _, want := range []string{"add", "list", "refresh", "set", "remove"} {
		c.False(!got[want], "repo subcommands missing %q (have %v)", want, got)
	}
	c.Len(got, 5, "repo subcommands")

	// Arg contracts: add takes name+url, remove takes exactly one, and list
	// takes none — validation fails before any endpoint is touched, so these
	// run without a harness. `refresh` is deliberately ABSENT: it takes any
	// number of names, and none means "every registered source", so neither a
	// bare `refresh` nor `refresh a b` is an argument error any more (pinned by
	// TestRepoRefreshAllSources / TestRepoRefreshNamedSources).
	for _, args := range [][]string{
		{"add"},                    // missing url
		{"add", "only_name"},       // missing url
		{"add", "a", "u", "extra"}, // too many
		{"remove"},                 // missing name
		{"remove", "a", "b"},       // too many
		{"set"},                    // missing name
		{"set", "a", "b"},          // too many
		{"list", "extra"},          // list takes none
	} {
		c.Error(runRepoCmd(t, args...), "repo %v: executed without error, want an arg refusal", args)
	}

	// The name pre-check the add verb performs locally: "local" is reserved
	// for the blob store and a non-identifier never becomes a `repo` value —
	// both refused before any round trip.
	stub := &repoStubControl{}
	newRepoHarness(t, stub)
	for _, name := range []string{"local", "bad.name"} {
		c.Error(runRepoCmd(t, "add", name, "https://example.net/x.git"), "repo add %q: accepted, want the name refusal", name)
	}
	c.Empty(stub.addCalls, "the daemon was called despite the local name refusals")
}

// TestRepoSummaryJSONLIsOneCompactLine pins the -J contract for the
// add/refresh summary emitter: ONE compact record, no envelope, no
// indentation — the shape `emitPymoduleCode` emits in the same mode, never
// the indented -j form.
func TestRepoSummaryJSONLIsOneCompactLine(t *testing.T) {
	c := assert.NewCollecting(t)
	summary := func() *rafikiv1.RefreshRepoResponse {
		return &rafikiv1.RefreshRepoResponse{
			Scripts: []*rafikiv1.RepoScript{
				{Name: "rotate_keys"},
				{Name: "deploy"},
			},
			Packages:  []*rafikiv1.RepoPackage{{Name: "opslib"}},
			VenvReady: true,
		}
	}

	// The emitter itself, both ways around: -j indents, -J stays one line.
	var jBuf, jlBuf bytes.Buffer
	c.Require().NoError(emitRepoSummary(&jBuf, summary(), outputJSON))
	c.Require().NoError(emitRepoSummary(&jlBuf, summary(), outputJSONL))
	if lines := strings.Split(strings.TrimRight(jlBuf.String(), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("emitRepoSummary -J printed %d line(s), want one compact record:\n%s", len(lines), jlBuf.String())
	}
	c.StrContains(jBuf.String(), "\n  ", "emitRepoSummary -j lost its indentation (the shape that distinguishes the two modes):\n")

	// End to end: `repo add <name> <url> -J`. The -J flag lives on the
	// root's persistent flag set, so the command runs under a root carrying
	// the same persistent-flag registrations the real binary has (the same
	// trick newTestRoot uses for the profile flag).
	stub := &repoStubControl{
		addResp: &rafikiv1.AddRepoResponse{
			Row: &rafikiv1.RepoRow{
				Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
			},
			Commit: "0123456789abcdef0123456789abcdef01234567",
			Skills: 2,
		},
		refreshResp: summary(),
	}
	newRepoHarness(t, stub)

	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.PersistentFlags().StringP("output", "o", "auto", "")
	root.PersistentFlags().BoolP("json", "j", false, "")
	root.PersistentFlags().BoolP("jsonl", "J", false, "")
	root.AddCommand(newRepoCmd()) // the repo group is a root command now
	root.SetArgs([]string{"repo", "add", "ops_tools", "https://example.net/ops.git", "-J"})

	out := captureStdout(t, func() {
		c.Require().NoError(root.Execute(), "repo add -J")
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	c.Require().Len(lines, 1, "add -J printed %d line(s), want exactly one compact record:\n%s", len(lines), out)
	// add -J emits the REGISTRATION's answer (AddRepoResponse): the row, the
	// first refresh's commit and skill count, and its skill errors.
	var resp rafikiv1.AddRepoResponse
	err := json.Unmarshal([]byte(lines[0]), &resp)
	c.Require().NoError(err, "add -J line is not a bare record: %v\n%s", err, lines[0])
	if resp.GetRow().GetName() != "ops_tools" || resp.GetCommit() != "0123456789abcdef0123456789abcdef01234567" || resp.GetSkills() != 2 {
		t.Errorf("add -J record = %+v, want the registration's answer", &resp)
	}
}

// The rendered table cells, tested without a daemon: the header names the
// seven columns, a refless row falls back to "-", the commit is shortened to
// twelve characters, and an on-demand source renders "-" in REFRESH.
func TestRepoListCells(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf strings.Builder
	c.Require().NoError(renderRepoList(&buf, []*rafikiv1.RepoRow{
		{
			Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
			Commit:          "0123456789abcdef0123456789abcdef01234567",
			RefreshInterval: durationpb.New(15 * time.Minute),
			Skills:          3,
			Scripts:         2,
		},
		{Name: "shared_lib", Url: "https://example.net/lib.git"},
	}, false))
	out := buf.String()
	for _, want := range []string{
		"NAME", "URL", "REF", "COMMIT", "REFRESH", "SKILLS", "SCRIPTS",
		"ops_tools", "https://example.net/ops.git", "main", "shared_lib",
		"0123456789ab", // the commit, shortened
		"15m0s",        // the schedule
		"3",            // skills
		"2",            // scripts
	} {
		c.StrContains(out, want, "renderRepoList output missing")
	}
	c.NotStrContains(out, "0123456789abcdef", "the commit was not shortened to twelve characters")
	// The never-refreshed row is on demand with no commit: both render "-".
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var shared string
	for _, line := range lines {
		if strings.Contains(line, "shared_lib") {
			shared = line
		}
	}
	c.Require().NotEq("", shared, "no shared_lib row in:\n%s", out)
	c.StrContains(shared, "-", "the never-refreshed row has no dash fallback:\n%s", out)
}

// TestRepoAddRejectsLocalNameBeforeDial is folded into
// TestRepoCommandTree's arg loop; this keeps the pinned name so the
// reviewer's expectation ("add refuses reserved names locally") has a named
// anchor.
func TestRepoAddRejectsLocalNameBeforeDial(t *testing.T) {
	t.Run("reserved and malformed names", func(t *testing.T) {
		c := assert.NewCollecting(t)
		stub := &repoStubControl{}
		newRepoHarness(t, stub)
		for _, name := range []string{"local", "bad.name"} {
			c.Error(runRepoCmd(t, "add", name, "https://example.net/x.git"), "repo add %q: accepted", name)
		}
		c.Empty(stub.addCalls, "daemon called despite local refusals")
	})
}

// TestRepoRefreshAllSources pins the no-argument contract: `refresh`
// with no names lists the caller's sources ONCE and refreshes every one of
// them, in list order, naming each in the output.
func TestRepoRefreshAllSources(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListReposResponse{Rows: []*rafikiv1.RepoRow{
			{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
			{Name: "shared_lib", Url: "https://example.net/lib.git", Ref: "main"},
		}},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "refresh"), "repo refresh (no names)")
	})
	c.Eq(1, stub.listCalls, "ListRepos calls for a whole-catalogue refresh")
	refreshes := stub.refreshes()
	c.Require().Len(refreshes, 2, "RefreshRepo calls")
	c.Eq("ops_tools", refreshes[0].GetName(), "first refreshed source")
	c.Eq("shared_lib", refreshes[1].GetName(), "second refreshed source")
	c.StrContains(out, "refreshed ops_tools", "refresh-all output missing the first source:\n")
	c.StrContains(out, "refreshed shared_lib", "refresh-all output missing the second source:\n")
}

// TestRepoRefreshNamedSources pins the other half: naming sources
// refreshes exactly those and never consults the catalogue.
func TestRepoRefreshNamedSources(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{}
	newRepoHarness(t, stub)

	c.Require().NoError(runRepoCmd(t, "refresh", "shared_lib", "ops_tools"), "repo refresh a b")
	c.Eq(0, stub.listCalls, "ListRepos must not be called when names are given")
	refreshes := stub.refreshes()
	c.Require().Len(refreshes, 2, "RefreshRepo calls")
	c.Eq("shared_lib", refreshes[0].GetName(), "names are refreshed in the order given")
	c.Eq("ops_tools", refreshes[1].GetName(), "names are refreshed in the order given")
}

// TestRepoRefreshNoSources pins the empty catalogue: nothing to do is
// not an error, and no refresh is attempted.
func TestRepoRefreshNoSources(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{listResp: &rafikiv1.ListReposResponse{}}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "refresh"), "repo refresh with nothing registered")
	})
	c.StrContains(out, "no repos to refresh", "empty-refresh output:\n")
	c.Empty(stub.refreshes(), "refreshed a source despite an empty catalogue")
}

// TestRepoRefreshCollectsPerSourceFailures pins the partial-failure
// contract: one source failing names itself on stderr, the others still
// refresh, and the command exits non-zero.
func TestRepoRefreshCollectsPerSourceFailures(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListReposResponse{Rows: []*rafikiv1.RepoRow{
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

// TestRepoRefreshJSONRowsAreNamed pins the -J contract for a
// multi-source run: one compact, self-describing record per source — the
// response carries its own name, so no client-side output struct is needed.
func TestRepoRefreshJSONRowsAreNamed(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListReposResponse{Rows: []*rafikiv1.RepoRow{
			{Name: "ops_tools"}, {Name: "shared_lib"},
		}},
		refreshResp: &rafikiv1.RefreshRepoResponse{
			Scripts:   []*rafikiv1.RepoScript{{Name: "rotate_keys"}},
			VenvReady: true,
		},
	}
	newRepoHarness(t, stub)

	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.PersistentFlags().StringP("output", "o", "auto", "")
	root.PersistentFlags().BoolP("json", "j", false, "")
	root.PersistentFlags().BoolP("jsonl", "J", false, "")
	root.AddCommand(newRepoCmd())
	root.SetArgs([]string{"repo", "refresh", "-J"})

	out := captureStdout(t, func() {
		c.Require().NoError(root.Execute(), "repo refresh -J")
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	c.Require().Len(lines, 2, "refresh -J printed %d line(s), want one per source:\n%s", len(lines), out)
	var names []string
	for i, line := range lines {
		var resp rafikiv1.RefreshRepoResponse
		c.Require().NoError(json.Unmarshal([]byte(line), &resp), "refresh -J line %d is not a bare record: %v", i, line)
		names = append(names, resp.GetName())
	}
	c.EqDeep([]string{"ops_tools", "shared_lib"}, names, "refresh -J rows are not self-describing")
}

// TestRepoRefreshCompletesSourceNames pins that the verb actually wires
// a completion function and that it offers the registered source names.
func TestRepoRefreshCompletesSourceNames(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		listResp: &rafikiv1.ListReposResponse{Rows: []*rafikiv1.RepoRow{
			{Name: "ops_tools"}, {Name: "shared_lib"},
		}},
	}
	newRepoHarness(t, stub)

	var refresh *cobra.Command
	for _, sub := range newRepoCmd().Commands() {
		if sub.Name() == "refresh" {
			refresh = sub
		}
	}
	c.Require().NotNil(refresh, "repo refresh not found")
	c.Require().NotNil(refresh.ValidArgsFunction, "refresh has no ValidArgsFunction — `rafiki repo refresh <TAB>` completes nothing")
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

// TestRepoSetSendsOnlyTheNamedHalf pins `repo set`'s presence contract: each
// flag is optional and independent, an unnamed half is ABSENT on the wire (not
// a zero), `--refresh off` is a present zero (on demand), and naming neither
// is a usage error rather than a no-op round trip.
func TestRepoSetSendsOnlyTheNamedHalf(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{setResp: &rafikiv1.SetRepoResponse{Row: &rafikiv1.RepoRow{
		Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "v2",
		RefreshInterval: durationpb.New(15 * time.Minute),
	}}}
	newRepoHarness(t, stub)

	// --ref alone: the interval stays absent.
	out := captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "set", "ops_tools", "--ref", "v2"), "repo set --ref")
	})
	sets := stub.sets()
	c.Require().Len(sets, 1, "SetRepo calls")
	c.Require().NotNil(sets[0].Ref, "the ref did not reach the daemon")
	c.Eq("v2", sets[0].GetRef(), "ref")
	c.Nil(sets[0].RefreshInterval, "an interval was sent for a --ref-only set")
	c.StrContains(out, "set ops_tools", "set output:\n")

	// --refresh off: a PRESENT zero, which is on demand.
	c.Require().NoError(runRepoCmd(t, "set", "ops_tools", "--refresh", "off"), "repo set --refresh off")
	sets = stub.sets()
	c.Require().Len(sets, 2, "SetRepo calls")
	c.Require().NotNil(sets[1].RefreshInterval, "a present zero interval was sent as absent")
	c.Eq(time.Duration(0), sets[1].GetRefreshInterval().AsDuration(), "the off interval")
	c.Nil(sets[1].Ref, "a ref was sent for a --refresh-only set")

	// --refresh 15m: a real interval.
	c.Require().NoError(runRepoCmd(t, "set", "ops_tools", "--refresh", "15m"), "repo set --refresh 15m")
	sets = stub.sets()
	c.Require().Len(sets, 3, "SetRepo calls")
	c.Eq(15*time.Minute, sets[2].GetRefreshInterval().AsDuration(), "the interval")

	// Neither flag: refused before any round trip.
	c.Error(runRepoCmd(t, "set", "ops_tools"), "repo set with no flags: accepted")
	c.Len(stub.sets(), 3, "the daemon was called despite an empty set")

	// A too-short interval and a malformed name are refused locally too.
	c.Error(runRepoCmd(t, "set", "ops_tools", "--refresh", "30s"), "repo set --refresh 30s: accepted")
	c.Error(runRepoCmd(t, "set", "bad.name", "--ref", "v2"), "repo set with a malformed name: accepted")
	c.Len(stub.sets(), 3, "the daemon was called despite a local refusal")
}

// TestRepoRefreshPrintsCommitSkillsAndErrors pins the refresh verb's output
// contract: `refreshed <name> at <commit[:12]>[ (unchanged)]: N script(s), N
// package(s), N skill(s)` on stdout, one `skill error: <text>` per failure on
// stderr, and a non-zero exit when there is any skill error — the scripts
// refreshed, but the skills the repo ships did not.
func TestRepoRefreshPrintsCommitSkillsAndErrors(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		refreshResp: &rafikiv1.RefreshRepoResponse{
			Commit:      "0123456789abcdef0123456789abcdef01234567",
			Unchanged:   true,
			Scripts:     []*rafikiv1.RepoScript{{Name: "rotate_keys"}, {Name: "deploy"}},
			Packages:    []*rafikiv1.RepoPackage{{Name: "opslib"}},
			VenvReady:   true,
			Skills:      4,
			SkillErrors: []string{"skills/demo: symlink references/a.md is not followed", "skills/other: too large"},
		},
	}
	newRepoHarness(t, stub)

	stderr := captureStderr(t)
	var err error
	out := captureStdout(t, func() {
		err = runRepoCmd(t, "refresh", "ops_tools")
	})
	c.StrContains(out, "refreshed ops_tools at 0123456789ab (unchanged): 2 script(s), 1 package(s), 4 skill(s)", "refresh output:\n")
	c.Require().Error(err, "a skill error must make the command exit non-zero")
	c.StrContains(err.Error(), "2 skill error(s)", "refresh error")
	c.StrContains(stderr(), "skill error: skills/demo: symlink references/a.md is not followed", "refresh stderr:\n")
	c.StrContains(stderr(), "skill error: skills/other: too large", "refresh stderr:\n")

	// No skill errors, no "(unchanged)": the line still reports the commit,
	// and the command succeeds.
	stub.refreshResp = &rafikiv1.RefreshRepoResponse{
		Commit: "fedcbafedcbafedcbafedcbafedcbafedcbafedc",
		Skills: 1,
	}
	stderr = captureStderr(t)
	out = captureStdout(t, func() {
		c.Require().NoError(runRepoCmd(t, "refresh", "ops_tools"), "repo refresh")
	})
	c.StrContains(out, "refreshed ops_tools at fedcbafedcba: 0 script(s), 0 package(s), 1 skill(s)", "refresh output:\n")
	c.NotStrContains(out, "(unchanged)", "an unchanged refresh line was printed for a changed one:\n")
	c.Eq("", stderr(), "stderr for a clean refresh")
}

// TestRepoRefreshLineShapes pins the line itself, without a daemon: the commit
// is shortened to twelve characters (and left alone when it is shorter), and
// the unchanged marker is appended before the counts.
func TestRepoRefreshLineShapes(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("refreshed ops_tools at 0123456789ab: 2 script(s), 1 package(s), 0 skill(s)",
		repoRefreshLine("ops_tools", "0123456789abcdef0123456789abcdef01234567", false, 2, 1, 0), "line")
	c.Eq("refreshed ops_tools at 0123456789ab (unchanged): 0 script(s), 0 package(s), 3 skill(s)",
		repoRefreshLine("ops_tools", "0123456789abcdef0123456789abcdef01234567", true, 0, 0, 3), "unchanged line")
	c.Eq("refreshed ops_tools at abc123: 0 script(s), 0 package(s), 0 skill(s)",
		repoRefreshLine("ops_tools", "abc123", false, 0, 0, 0), "short commit line")
	// A refresh that resolved nothing (an executor that answered with no
	// commit) renders "-" rather than a blank.
	c.StrContains(repoRefreshLine("ops_tools", "", false, 0, 0, 0), "refreshed ops_tools at -:", "empty commit line")
}

// TestRepoAddReportsSkillErrors pins add's skills-half contract: a skill error
// is printed on stderr as `skill error: <text>` and the command exits
// non-zero, even though the registration itself succeeded — only a namespace
// collision fails an add, but a repo whose skills did not land must not look
// like a clean success.
func TestRepoAddReportsSkillErrors(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := &repoStubControl{
		addResp: &rafikiv1.AddRepoResponse{
			Row:         &rafikiv1.RepoRow{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
			Commit:      "0123456789abcdef0123456789abcdef01234567",
			SkillErrors: []string{"skills/demo: symlink references/a.md is not followed"},
		},
		refreshResp: &rafikiv1.RefreshRepoResponse{
			Scripts:   []*rafikiv1.RepoScript{{Name: "rotate_keys"}},
			VenvReady: true,
		},
	}
	newRepoHarness(t, stub)

	stderr := captureStderr(t)
	var err error
	out := captureStdout(t, func() {
		err = runRepoCmd(t, "add", "ops_tools", "https://example.net/ops.git")
	})
	c.StrContains(out, "refreshed ops_tools at 0123456789ab: 1 script(s), 0 package(s), 0 skill(s)", "add output:\n")
	c.Require().Error(err, "a skill error must make `repo add` exit non-zero")
	c.StrContains(err.Error(), "1 skill error(s)", "add error")
	c.StrContains(stderr(), "skill error: skills/demo: symlink references/a.md is not followed", "add stderr:\n")
}
