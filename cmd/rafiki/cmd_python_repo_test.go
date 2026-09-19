// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
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

// newRepoHarness wires an isolated profile whose connect.sock sibling serves
// the stub, and returns the stub — the Connect-only variant of
// newReviewHarness (the python repo verbs never touch the framed plane).
func newRepoHarness(t *testing.T, stub *repoStubControl) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// Short dir name: unix socket paths are capped at ~104 bytes on darwin.
	dir, err := os.MkdirTemp("", "repo")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	controlSock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, filepath.Join(dir, "connect.sock"), routePath, handler)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: controlSock},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
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
		if err := runRepoCmd(t, "add", "ops_tools", "https://example.net/ops.git"); err != nil {
			t.Fatalf("python repo add: %v", err)
		}
	})
	if !strings.Contains(out, "ops_tools") || !strings.Contains(out, "https://example.net/ops.git") {
		t.Errorf("add output does not name the source:\n%s", out)
	}
	if !strings.Contains(out, "2 script(s)") || !strings.Contains(out, "1 package(s)") {
		t.Errorf("add output does not report the discovered inventory:\n%s", out)
	}

	// The refresh that produced the summary named the registered source.
	adds := stub.adds()
	if len(adds) != 1 || adds[0].GetName() != "ops_tools" || adds[0].GetUrl() != "https://example.net/ops.git" || adds[0].GetRef() != "main" {
		t.Fatalf("AddPymoduleGitSource request = %+v, want ops_tools/url with the --ref main default", adds)
	}
	refreshes := stub.refreshes()
	if len(refreshes) != 1 || refreshes[0].GetName() != "ops_tools" {
		t.Fatalf("RefreshPymoduleGitSource requests = %v, want exactly one for ops_tools", refreshes)
	}
}

// TestPythonRepoRefreshRendersVenvFailure pins the other half of the summary:
// a venv that failed to build is printed, and does not fail the command —
// the refresh succeeded, only the build didn't.
func TestPythonRepoRefreshRendersVenvFailure(t *testing.T) {
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
		if err := runRepoCmd(t, "refresh", "ops_tools"); err != nil {
			t.Fatalf("python repo refresh: %v", err)
		}
	})
	if !strings.Contains(out, "refreshed ops_tools") {
		t.Errorf("refresh output does not name the source:\n%s", out)
	}
	if !strings.Contains(out, "1 script(s)") {
		t.Errorf("refresh output does not report the discovered count:\n%s", out)
	}
	if !strings.Contains(out, "venv build failed: uv sync failed: no solution found") {
		t.Errorf("refresh output does not carry the venv error:\n%s", out)
	}
	if got := len(stub.refreshes()); got != 1 || stub.refreshes()[0].GetName() != "ops_tools" {
		t.Errorf("refresh requests = %v, want one for ops_tools", stub.refreshes())
	}
}

// TestPythonRepoListRendersTable pins the list table: NAME, URL, REF — and
// that a missing cell renders "-" rather than a blank.
func TestPythonRepoListRendersTable(t *testing.T) {
	stub := &repoStubControl{
		listResp: &rafikiv1.ListPymoduleGitSourcesResponse{Rows: []*rafikiv1.GitSourceRow{
			{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
			{Name: "shared_lib", Url: "https://example.net/lib.git"},
		}},
	}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		if err := runRepoCmd(t, "list"); err != nil {
			t.Fatalf("python repo list: %v", err)
		}
	})
	for _, want := range []string{"NAME", "URL", "REF", "ops_tools", "https://example.net/ops.git", "main", "shared_lib"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// A row registered with no ref renders "-", not an empty cell.
	if !strings.Contains(out, "-") {
		t.Errorf("list output has no dash fallback for the refless row:\n%s", out)
	}
	if stub.listCalls != 1 {
		t.Errorf("ListPymoduleGitSources called %d time(s), want 1", stub.listCalls)
	}
}

// TestPythonRepoRemoveNamesTheSource: remove sends the name and confirms.
func TestPythonRepoRemoveNamesTheSource(t *testing.T) {
	stub := &repoStubControl{}
	newRepoHarness(t, stub)

	out := captureStdout(t, func() {
		if err := runRepoCmd(t, "remove", "ops_tools"); err != nil {
			t.Fatalf("python repo remove: %v", err)
		}
	})
	if !strings.Contains(out, "removed ops_tools") {
		t.Errorf("remove output = %q, want it to name the removed source", out)
	}
	if got := stub.removes(); len(got) != 1 || got[0].GetName() != "ops_tools" {
		t.Errorf("RemovePymoduleGitSource requests = %v, want one for ops_tools", got)
	}
}

// The group's own tree: exactly the four verbs, and the aliases the CLI
// output contract implies (remove carries rm; the group has none).
func TestPythonRepoCommandTree(t *testing.T) {
	cmd := newPythonRepoCmd()
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	for _, want := range []string{"add", "list", "refresh", "remove"} {
		if !got[want] {
			t.Errorf("python repo subcommands missing %q (have %v)", want, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("python repo subcommands = %v, want exactly add/list/refresh/remove", got)
	}

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
		if err := runRepoCmd(t, args...); err == nil {
			t.Errorf("python repo %v: executed without error, want an arg refusal", args)
		}
	}

	// The name pre-check the add verb performs locally: "local" is reserved
	// for the blob store and a non-identifier never becomes a `repo` value —
	// both refused before any round trip.
	stub := &repoStubControl{}
	newRepoHarness(t, stub)
	for _, name := range []string{"local", "9bad"} {
		if err := runRepoCmd(t, "add", name, "https://example.net/x.git"); err == nil {
			t.Errorf("python repo add %q: accepted, want the name refusal", name)
		}
	}
	if len(stub.addCalls) != 0 {
		t.Errorf("the daemon was called despite the local name refusals: %v", stub.addCalls)
	}
}

// The rendered table cells, tested without a daemon: the header names the
// three columns and a URL/refless row falls back to "-".
func TestPythonRepoListCells(t *testing.T) {
	var buf strings.Builder
	if err := renderGitSourceList(&buf, []*rafikiv1.GitSourceRow{
		{Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main"},
		{Name: "shared_lib", Url: "https://example.net/lib.git"},
	}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "URL", "REF", "ops_tools", "main", "shared_lib"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderGitSourceList output missing %q:\n%s", want, out)
		}
	}
}

// TestPythonRepoAddRejectsLocalNameBeforeDial is folded into
// TestPythonRepoCommandTree's arg loop; this keeps the pinned name so the
// reviewer's expectation ("add refuses reserved names locally") has a named
// anchor.
func TestPythonRepoAddRejectsLocalNameBeforeDial(t *testing.T) {
	t.Run("reserved and malformed names", func(t *testing.T) {
		stub := &repoStubControl{}
		newRepoHarness(t, stub)
		for _, name := range []string{"local", "9bad"} {
			if err := runRepoCmd(t, "add", name, "https://example.net/x.git"); err == nil {
				t.Errorf("python repo add %q: accepted", name)
			}
		}
		if len(stub.addCalls) != 0 {
			t.Errorf("daemon called despite local refusals: %v", stub.addCalls)
		}
	})
}
