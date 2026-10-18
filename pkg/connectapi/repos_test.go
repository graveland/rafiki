// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/repos"

	"github.com/multigres/testkit/assert"
)

// fakeRepos records every call the handlers make, so a test can assert
// the handler passed the request through unchanged and mapped the result.
type fakeRepos struct {
	addRow  RepoRow
	addErr  error
	rows    []RepoRow
	listErr error

	refreshed string // the name the last RefreshRepo got
	scripts   []RepoScript
	packages  []RepoPackage
	venvReady bool
	venvError string
	refreshEr error

	removed string
	delErr  error
}

func (f *fakeRepos) AddRepo(_ context.Context, name, url, ref string) (RepoRow, error) {
	if f.addErr != nil {
		return RepoRow{}, f.addErr
	}
	f.addRow = RepoRow{Name: name, URL: url, Ref: ref}
	return f.addRow, nil
}

func (f *fakeRepos) ListRepos(context.Context) ([]RepoRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeRepos) RefreshRepo(_ context.Context, name string) ([]RepoScript, []RepoPackage, bool, string, error) {
	f.refreshed = name
	if f.refreshEr != nil {
		return nil, nil, false, "", f.refreshEr
	}
	return f.scripts, f.packages, f.venvReady, f.venvError, nil
}

func (f *fakeRepos) RemoveRepo(_ context.Context, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.removed = name
	return nil
}

// TestRepoManagerUnavailableBeforeWiring mirrors TestPymodulesUnwired:
// with no manager attached, all four repo verbs answer CodeUnavailable
// rather than nil-panicking or pretending success.
func TestRepoManagerUnavailableBeforeWiring(t *testing.T) {
	s := &Server{}
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"AddRepo", func() error {
			_, err := s.AddRepo(context.Background(), connect.NewRequest(&rafikiv1.AddRepoRequest{Name: "x", Url: "u"}))
			return err
		}},
		{"ListRepos", func() error {
			_, err := s.ListRepos(context.Background(), connect.NewRequest(&rafikiv1.ListReposRequest{}))
			return err
		}},
		{"RefreshRepo", func() error {
			_, err := s.RefreshRepo(context.Background(), connect.NewRequest(&rafikiv1.RefreshRepoRequest{Name: "x"}))
			return err
		}},
		{"RemoveRepo", func() error {
			_, err := s.RemoveRepo(context.Background(), connect.NewRequest(&rafikiv1.RemoveRepoRequest{Name: "x"}))
			return err
		}},
	} {
		err := call.fn()
		if err == nil {
			t.Errorf("%s with no manager: accepted", call.name)
			continue
		}
		assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "%s with no manager: got code %v, want Unavailable", call.name, connect.CodeOf(err))
	}
}

// TestAddRepoRejectsEmptyName: an empty name can never become
// the `repo` argument, and the reserved "local" would make repo="local"
// ambiguous between the blob store and a repo — both refused before
// the manager is touched.
func TestAddRepoRejectsEmptyName(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakeRepos{}
	s.SetRepoManager(f)

	for _, name := range []string{"", "local"} {
		_, err := s.AddRepo(context.Background(), connect.NewRequest(&rafikiv1.AddRepoRequest{Name: name, Url: "https://example.net/x.git"}))
		c.Require().Error(err, "add with name %q: accepted", name)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "add with name %q: got code %v, want InvalidArgument", name, connect.CodeOf(err))
	}
	c.Eq((RepoRow{}), f.addRow, "manager was called despite the rejections")
}

// TestAddRepoRejectsEmptyURL: a repo without a url has
// nothing for the executor to clone.
func TestAddRepoRejectsEmptyURL(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakeRepos{}
	s.SetRepoManager(f)

	_, err := s.AddRepo(context.Background(), connect.NewRequest(&rafikiv1.AddRepoRequest{Name: "ops_tools", Url: ""}))
	c.Require().Error(err, "add without url: accepted")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "add without url: got code")
	c.Eq((RepoRow{}), f.addRow, "manager was called despite the rejection")
}

// TestSetRepoManagerNilIsRefused: SetRepoManager(nil) is refused,
// not stored — a stored pointer to a nil interface would defeat the
// Unavailable path above and nil-panic the first handler call instead.
func TestSetRepoManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetRepoManager(nil)
	_, err := s.ListRepos(context.Background(), connect.NewRequest(&rafikiv1.ListReposRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetRepoManager(nil): got code")
}

// TestRepoHandlersPassThroughToManager pins the handler contract on a
// wired manager: arguments pass through unchanged, the response maps the
// manager's rows, an unknown name maps to NotFound, a store failure maps to
// Internal, and validation failures never reach the manager.
func TestRepoHandlersPassThroughToManager(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}

	// Add passes (name, url, ref) through and returns the row; an empty ref
	// stays an empty ref (the CLI defaults it, the daemon stores what it got).
	f := &fakeRepos{}
	s.SetRepoManager(f)
	resp, err := s.AddRepo(context.Background(), connect.NewRequest(&rafikiv1.AddRepoRequest{
		Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
	}))
	c.Require().NoError(err, "add")
	if got := f.addRow; got.Name != "ops_tools" || got.URL != "https://example.net/ops.git" || got.Ref != "main" {
		t.Errorf("manager got %+v, want ops_tools/url/main", got)
	}
	if row := resp.Msg.GetRow(); row.GetName() != "ops_tools" || row.GetUrl() != "https://example.net/ops.git" || row.GetRef() != "main" {
		t.Errorf("add response row = %+v", row)
	}

	// List maps rows verbatim.
	s.SetRepoManager(&fakeRepos{rows: []RepoRow{
		{Name: "ops_tools", URL: "https://example.net/ops.git", Ref: "main"},
		{Name: "shared_lib", URL: "https://example.net/lib.git", Ref: "v2"},
	}})
	lresp, err := s.ListRepos(context.Background(), connect.NewRequest(&rafikiv1.ListReposRequest{}))
	c.Require().NoError(err, "list")
	c.Require().Len(lresp.Msg.GetRows(), 2, "list returned %d rows, want 2", len(lresp.Msg.GetRows()))
	if lresp.Msg.GetRows()[0].GetName() != "ops_tools" || lresp.Msg.GetRows()[0].GetRef() != "main" {
		t.Errorf("list row 0 = %+v", lresp.Msg.GetRows()[0])
	}

	// Refresh maps scripts/packages and the venv state.
	s.SetRepoManager(&fakeRepos{
		scripts:   []RepoScript{{Name: "rotate_keys", Description: "rotate keys"}},
		packages:  []RepoPackage{{Name: "opslib", Description: "shared ops helpers"}},
		venvReady: true,
	})
	rresp, err := s.RefreshRepo(context.Background(), connect.NewRequest(&rafikiv1.RefreshRepoRequest{Name: "ops_tools"}))
	c.Require().NoError(err, "refresh")
	c.Eq(1, len(rresp.Msg.GetScripts()), "refresh returned")
	c.Eq(1, len(rresp.Msg.GetPackages()), "refresh returned")
	c.True(rresp.Msg.GetVenvReady(), "refresh response lost venvReady")
	// The response names its own source: a caller refreshing several sources in
	// turn attributes each by this field alone.
	c.Eq("ops_tools", rresp.Msg.GetName(), "refresh response does not echo the source name")

	// A venv failure stays in the response: the refresh itself succeeded.
	s.SetRepoManager(&fakeRepos{venvReady: false, venvError: "uv sync failed"})
	rresp, err = s.RefreshRepo(context.Background(), connect.NewRequest(&rafikiv1.RefreshRepoRequest{Name: "ops_tools"}))
	c.Require().NoError(err, "refresh with failed venv")
	if rresp.Msg.GetVenvReady() || rresp.Msg.GetVenvError() != "uv sync failed" {
		t.Errorf("refresh response lost the venv failure: ready=%v error=%q", rresp.Msg.GetVenvReady(), rresp.Msg.GetVenvError())
	}

	// Remove records the name; a never-registered name maps ErrNotFound to
	// CodeNotFound, and a store failure maps to Internal.
	f = &fakeRepos{}
	s.SetRepoManager(f)
	if _, err := s.RemoveRepo(context.Background(), connect.NewRequest(&rafikiv1.RemoveRepoRequest{Name: "ops_tools"})); err != nil {
		t.Fatalf("remove: %v", err)
	}
	c.Eq("ops_tools", f.removed, "manager removed")

	s.SetRepoManager(&fakeRepos{delErr: repos.ErrNotFound})
	_, err = s.RemoveRepo(context.Background(), connect.NewRequest(&rafikiv1.RemoveRepoRequest{Name: "gone"}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "remove of unknown name: got code")

	boom := errors.New("connection refused")
	s.SetRepoManager(&fakeRepos{delErr: boom})
	_, err = s.RemoveRepo(context.Background(), connect.NewRequest(&rafikiv1.RemoveRepoRequest{Name: "x"}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "remove store failure: got code")

	// A malformed name is refused before the manager, on every verb that
	// takes one — the store would reject it too, but the handler owes the
	// caller the InvalidArgument-shaped answer.
	f = &fakeRepos{}
	s.SetRepoManager(f)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"refresh", func() error {
			_, err := s.RefreshRepo(context.Background(), connect.NewRequest(&rafikiv1.RefreshRepoRequest{Name: "bad.name"}))
			return err
		}},
		{"remove", func() error {
			_, err := s.RemoveRepo(context.Background(), connect.NewRequest(&rafikiv1.RemoveRepoRequest{Name: "bad.name"}))
			return err
		}},
	} {
		if err := tc.call(); err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s with malformed name: got %v, want InvalidArgument", tc.name, err)
		}
	}
	if f.refreshed != "" || f.removed != "" {
		t.Errorf("manager reached despite the malformed names: refreshed=%q removed=%q", f.refreshed, f.removed)
	}
}

// TestRepoAddValidationShim is a shim so the verify pattern (-run
// TestRepo, an UNANCHORED substring match) also runs the three pinned
// tests above, whose names do not contain the pattern — the same trick
// cmd/rafikid/pymodulesync_test.go uses for its pinned payload-builder test.
func TestRepoAddValidationShim(t *testing.T) {
	t.Run("empty name", TestAddRepoRejectsEmptyName)
	t.Run("empty url", TestAddRepoRejectsEmptyURL)
	t.Run("nil manager refused", TestSetRepoManagerNilIsRefused)
}
