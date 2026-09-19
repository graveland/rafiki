// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
)

// fakeGitSources records every call the handlers make, so a test can assert
// the handler passed the request through unchanged and mapped the result.
type fakeGitSources struct {
	addRow  GitSourceRow
	addErr  error
	rows    []GitSourceRow
	listErr error

	refreshed string // the name the last RefreshGitSource got
	scripts   []GitSourceScript
	packages  []GitSourcePackage
	venvReady bool
	venvError string
	refreshEr error

	removed string
	delErr  error
}

func (f *fakeGitSources) AddGitSource(_ context.Context, name, url, ref string) (GitSourceRow, error) {
	if f.addErr != nil {
		return GitSourceRow{}, f.addErr
	}
	f.addRow = GitSourceRow{Name: name, URL: url, Ref: ref}
	return f.addRow, nil
}

func (f *fakeGitSources) ListGitSources(context.Context) ([]GitSourceRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeGitSources) RefreshGitSource(_ context.Context, name string) ([]GitSourceScript, []GitSourcePackage, bool, string, error) {
	f.refreshed = name
	if f.refreshEr != nil {
		return nil, nil, false, "", f.refreshEr
	}
	return f.scripts, f.packages, f.venvReady, f.venvError, nil
}

func (f *fakeGitSources) RemoveGitSource(_ context.Context, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.removed = name
	return nil
}

// TestGitPymoduleManagerUnavailableBeforeWiring mirrors TestPymodulesUnwired:
// with no manager attached, all four git-source verbs answer CodeUnavailable
// rather than nil-panicking or pretending success.
func TestGitPymoduleManagerUnavailableBeforeWiring(t *testing.T) {
	s := &Server{}
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"AddPymoduleGitSource", func() error {
			_, err := s.AddPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.AddPymoduleGitSourceRequest{Name: "x", Url: "u"}))
			return err
		}},
		{"ListPymoduleGitSources", func() error {
			_, err := s.ListPymoduleGitSources(context.Background(), connect.NewRequest(&rafikiv1.ListPymoduleGitSourcesRequest{}))
			return err
		}},
		{"RefreshPymoduleGitSource", func() error {
			_, err := s.RefreshPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RefreshPymoduleGitSourceRequest{Name: "x"}))
			return err
		}},
		{"RemovePymoduleGitSource", func() error {
			_, err := s.RemovePymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RemovePymoduleGitSourceRequest{Name: "x"}))
			return err
		}},
	} {
		err := call.fn()
		if err == nil {
			t.Errorf("%s with no manager: accepted", call.name)
			continue
		}
		if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Errorf("%s with no manager: got code %v, want Unavailable", call.name, connect.CodeOf(err))
		}
	}
}

// TestAddPymoduleGitSourceRejectsEmptyName: an empty name can never become
// the `repo` argument, and the reserved "local" would make repo="local"
// ambiguous between the blob store and a git source — both refused before
// the manager is touched.
func TestAddPymoduleGitSourceRejectsEmptyName(t *testing.T) {
	s := &Server{}
	f := &fakeGitSources{}
	s.SetGitSourceManager(f)

	for _, name := range []string{"", "local"} {
		_, err := s.AddPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.AddPymoduleGitSourceRequest{Name: name, Url: "https://example.net/x.git"}))
		if err == nil {
			t.Fatalf("add with name %q: accepted", name)
		}
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("add with name %q: got code %v, want InvalidArgument", name, connect.CodeOf(err))
		}
	}
	if f.addRow != (GitSourceRow{}) {
		t.Errorf("manager was called despite the rejections: %+v", f.addRow)
	}
}

// TestAddPymoduleGitSourceRejectsEmptyURL: a git source without a url has
// nothing for the executor to clone.
func TestAddPymoduleGitSourceRejectsEmptyURL(t *testing.T) {
	s := &Server{}
	f := &fakeGitSources{}
	s.SetGitSourceManager(f)

	_, err := s.AddPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.AddPymoduleGitSourceRequest{Name: "ops_tools", Url: ""}))
	if err == nil {
		t.Fatal("add without url: accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("add without url: got code %v, want InvalidArgument", connect.CodeOf(err))
	}
	if f.addRow != (GitSourceRow{}) {
		t.Errorf("manager was called despite the rejection: %+v", f.addRow)
	}
}

// TestSetGitSourceManagerNilIsRefused: SetGitSourceManager(nil) is refused,
// not stored — a stored pointer to a nil interface would defeat the
// Unavailable path above and nil-panic the first handler call instead.
func TestSetGitSourceManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetGitSourceManager(nil)
	_, err := s.ListPymoduleGitSources(context.Background(), connect.NewRequest(&rafikiv1.ListPymoduleGitSourcesRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("after SetGitSourceManager(nil): got code %v, want Unavailable", connect.CodeOf(err))
	}
}

// TestGitPymoduleHandlersPassThroughToManager pins the handler contract on a
// wired manager: arguments pass through unchanged, the response maps the
// manager's rows, an unknown name maps to NotFound, a store failure maps to
// Internal, and validation failures never reach the manager.
func TestGitPymoduleHandlersPassThroughToManager(t *testing.T) {
	s := &Server{}

	// Add passes (name, url, ref) through and returns the row; an empty ref
	// stays an empty ref (the CLI defaults it, the daemon stores what it got).
	f := &fakeGitSources{}
	s.SetGitSourceManager(f)
	resp, err := s.AddPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.AddPymoduleGitSourceRequest{
		Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
	}))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := f.addRow; got.Name != "ops_tools" || got.URL != "https://example.net/ops.git" || got.Ref != "main" {
		t.Errorf("manager got %+v, want ops_tools/url/main", got)
	}
	if row := resp.Msg.GetRow(); row.GetName() != "ops_tools" || row.GetUrl() != "https://example.net/ops.git" || row.GetRef() != "main" {
		t.Errorf("add response row = %+v", row)
	}

	// List maps rows verbatim.
	s.SetGitSourceManager(&fakeGitSources{rows: []GitSourceRow{
		{Name: "ops_tools", URL: "https://example.net/ops.git", Ref: "main"},
		{Name: "shared_lib", URL: "https://example.net/lib.git", Ref: "v2"},
	}})
	lresp, err := s.ListPymoduleGitSources(context.Background(), connect.NewRequest(&rafikiv1.ListPymoduleGitSourcesRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(lresp.Msg.GetRows()) != 2 {
		t.Fatalf("list returned %d rows, want 2", len(lresp.Msg.GetRows()))
	}
	if lresp.Msg.GetRows()[0].GetName() != "ops_tools" || lresp.Msg.GetRows()[0].GetRef() != "main" {
		t.Errorf("list row 0 = %+v", lresp.Msg.GetRows()[0])
	}

	// Refresh maps scripts/packages and the venv state.
	s.SetGitSourceManager(&fakeGitSources{
		scripts:   []GitSourceScript{{Name: "rotate_keys", Description: "rotate keys"}},
		packages:  []GitSourcePackage{{Name: "opslib", Description: "shared ops helpers"}},
		venvReady: true,
	})
	rresp, err := s.RefreshPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RefreshPymoduleGitSourceRequest{Name: "ops_tools"}))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := len(rresp.Msg.GetScripts()); got != 1 {
		t.Errorf("refresh returned %d script(s), want 1", got)
	}
	if got := len(rresp.Msg.GetPackages()); got != 1 {
		t.Errorf("refresh returned %d package(s), want 1", got)
	}
	if !rresp.Msg.GetVenvReady() {
		t.Error("refresh response lost venvReady")
	}

	// A venv failure stays in the response: the refresh itself succeeded.
	s.SetGitSourceManager(&fakeGitSources{venvReady: false, venvError: "uv sync failed"})
	rresp, err = s.RefreshPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RefreshPymoduleGitSourceRequest{Name: "ops_tools"}))
	if err != nil {
		t.Fatalf("refresh with failed venv: %v", err)
	}
	if rresp.Msg.GetVenvReady() || rresp.Msg.GetVenvError() != "uv sync failed" {
		t.Errorf("refresh response lost the venv failure: ready=%v error=%q", rresp.Msg.GetVenvReady(), rresp.Msg.GetVenvError())
	}

	// Remove records the name; a never-registered name maps ErrNotFound to
	// CodeNotFound, and a store failure maps to Internal.
	f = &fakeGitSources{}
	s.SetGitSourceManager(f)
	if _, err := s.RemovePymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RemovePymoduleGitSourceRequest{Name: "ops_tools"})); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if f.removed != "ops_tools" {
		t.Errorf("manager removed %q, want ops_tools", f.removed)
	}

	s.SetGitSourceManager(&fakeGitSources{delErr: gitpymodules.ErrNotFound})
	_, err = s.RemovePymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RemovePymoduleGitSourceRequest{Name: "gone"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("remove of unknown name: got code %v, want NotFound", connect.CodeOf(err))
	}

	boom := errors.New("connection refused")
	s.SetGitSourceManager(&fakeGitSources{delErr: boom})
	_, err = s.RemovePymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RemovePymoduleGitSourceRequest{Name: "x"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("remove store failure: got code %v, want Internal", connect.CodeOf(err))
	}

	// A malformed name is refused before the manager, on every verb that
	// takes one — the store would reject it too, but the handler owes the
	// caller the InvalidArgument-shaped answer.
	f = &fakeGitSources{}
	s.SetGitSourceManager(f)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"refresh", func() error {
			_, err := s.RefreshPymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RefreshPymoduleGitSourceRequest{Name: "9bad"}))
			return err
		}},
		{"remove", func() error {
			_, err := s.RemovePymoduleGitSource(context.Background(), connect.NewRequest(&rafikiv1.RemovePymoduleGitSourceRequest{Name: "9bad"}))
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

// TestGitPymoduleAddValidationShim is a shim so the verify pattern (-run
// TestGitPymodule, an UNANCHORED substring match) also runs the three pinned
// tests above, whose names do not contain the pattern — the same trick
// cmd/rafikid/pymodulesync_test.go uses for its pinned payload-builder test.
func TestGitPymoduleAddValidationShim(t *testing.T) {
	t.Run("empty name", TestAddPymoduleGitSourceRejectsEmptyName)
	t.Run("empty url", TestAddPymoduleGitSourceRejectsEmptyURL)
	t.Run("nil manager refused", TestSetGitSourceManagerNilIsRefused)
}
