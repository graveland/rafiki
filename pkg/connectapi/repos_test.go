// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/repos"

	"github.com/multigres/testkit/assert"
)

// fakeRepos records every call the handlers make, so a test can assert
// the handler passed the request through unchanged and mapped the result.
type fakeRepos struct {
	addRow      RepoRow
	addErr      error
	addInterval time.Duration // the interval the last AddRepo got
	rows        []RepoRow
	listErr     error

	refreshed string // the name the last RefreshRepo got
	scripts   []RepoScript
	packages  []RepoPackage
	venvReady bool
	venvError string
	refreshEr error
	commit    string
	unchanged bool
	skills    int
	skillErrs []string

	setReq RepoSetRequest // the last SetRepo request
	setErr error

	removed string
	delErr  error
}

func (f *fakeRepos) AddRepo(_ context.Context, name, url, ref string, refreshInterval time.Duration) (RepoRow, RepoRefresh, error) {
	if f.addErr != nil {
		return RepoRow{}, RepoRefresh{}, f.addErr
	}
	f.addInterval = refreshInterval
	f.addRow = RepoRow{Name: name, URL: url, Ref: ref, Commit: f.commit, RefreshInterval: refreshInterval, Skills: f.skills}
	return f.addRow, RepoRefresh{Commit: f.commit, Unchanged: f.unchanged, Skills: f.skills, SkillErrors: f.skillErrs}, nil
}

func (f *fakeRepos) ListRepos(context.Context) ([]RepoRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeRepos) RefreshRepo(_ context.Context, name string) (RepoRefresh, error) {
	f.refreshed = name
	if f.refreshEr != nil {
		return RepoRefresh{}, f.refreshEr
	}
	return RepoRefresh{
		Commit: f.commit, Unchanged: f.unchanged,
		Scripts: f.scripts, Packages: f.packages,
		VenvReady: f.venvReady, VenvError: f.venvError,
		Skills: f.skills, SkillErrors: f.skillErrs,
	}, nil
}

func (f *fakeRepos) SetRepo(_ context.Context, req RepoSetRequest) (RepoRow, error) {
	f.setReq = req
	if f.setErr != nil {
		return RepoRow{}, f.setErr
	}
	row := RepoRow{Name: req.Name}
	if req.Ref != nil {
		row.Ref = *req.Ref
	}
	if req.RefreshInterval != nil {
		row.RefreshInterval = *req.RefreshInterval
	}
	return row, nil
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
		commit:    "0123456789abcdef0123456789abcdef01234567",
		unchanged: true,
		skills:    3,
		skillErrs: []string{"skills/demo: unreadable"},
	})
	rresp, err := s.RefreshRepo(context.Background(), connect.NewRequest(&rafikiv1.RefreshRepoRequest{Name: "ops_tools"}))
	c.Require().NoError(err, "refresh")
	c.Eq(1, len(rresp.Msg.GetScripts()), "refresh returned")
	c.Eq(1, len(rresp.Msg.GetPackages()), "refresh returned")
	c.True(rresp.Msg.GetVenvReady(), "refresh response lost venvReady")
	// The response names its own source: a caller refreshing several sources in
	// turn attributes each by this field alone.
	c.Eq("ops_tools", rresp.Msg.GetName(), "refresh response does not echo the source name")
	// ...and carries the resolved commit, the unchanged flag, the skill count
	// and the skills half's errors.
	c.Eq("0123456789abcdef0123456789abcdef01234567", rresp.Msg.GetCommit(), "refresh response lost the resolved commit")
	c.True(rresp.Msg.GetUnchanged(), "refresh response lost the unchanged flag")
	c.Eq(int32(3), rresp.Msg.GetSkills(), "refresh response lost the skill count")
	c.EqDeep([]string{"skills/demo: unreadable"}, rresp.Msg.GetSkillErrors(), "refresh response lost the skill errors")

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

// TestSetRepoPresenceIsTriState pins SetRepo's three-way request: an absent
// ref and an absent interval each leave their half alone (nil pointers), a
// present zero interval means "on demand" (a non-nil pointer to zero), and a
// present ref is passed through. Collapsing absent and zero would make
// `repo set <name> --refresh off` indistinguishable from `repo set <name>`,
// which would silently turn a schedule off on a ref-only edit.
func TestSetRepoPresenceIsTriState(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}

	// Neither field: both stay nil, so neither half is touched.
	f := &fakeRepos{}
	s.SetRepoManager(f)
	resp, err := s.SetRepo(context.Background(), connect.NewRequest(&rafikiv1.SetRepoRequest{Name: "ops_tools"}))
	c.Require().NoError(err, "set with no fields")
	if f.setReq.Ref != nil || f.setReq.RefreshInterval != nil {
		t.Errorf("manager got ref=%v interval=%v, want both absent", f.setReq.Ref, f.setReq.RefreshInterval)
	}
	c.Eq("ops_tools", f.setReq.Name, "manager got name")
	c.Eq("ops_tools", resp.Msg.GetRow().GetName(), "set response row")

	// A present zero interval: on demand, NOT "leave it alone".
	f = &fakeRepos{}
	s.SetRepoManager(f)
	_, err = s.SetRepo(context.Background(), connect.NewRequest(&rafikiv1.SetRepoRequest{
		Name: "ops_tools", RefreshInterval: durationpb.New(0),
	}))
	c.Require().NoError(err, "set with a zero interval")
	c.Require().NotNil(f.setReq.RefreshInterval, "a present zero interval reached the manager as absent")
	c.Eq(time.Duration(0), *f.setReq.RefreshInterval, "zero interval")
	c.Nil(f.setReq.Ref, "ref was set by an interval-only request")

	// A real interval and a ref, both present.
	f = &fakeRepos{}
	s.SetRepoManager(f)
	_, err = s.SetRepo(context.Background(), connect.NewRequest(&rafikiv1.SetRepoRequest{
		Name: "ops_tools", Ref: ptrString("v2"), RefreshInterval: durationpb.New(15 * time.Minute),
	}))
	c.Require().NoError(err, "set with ref and interval")
	c.Require().NotNil(f.setReq.Ref, "the ref did not reach the manager")
	c.Eq("v2", *f.setReq.Ref, "ref")
	c.Require().NotNil(f.setReq.RefreshInterval, "the interval did not reach the manager")
	c.Eq(15*time.Minute, *f.setReq.RefreshInterval, "interval")

	// An interval below the one-minute floor is InvalidArgument with the
	// store's own text, and the manager is never reached.
	f = &fakeRepos{}
	s.SetRepoManager(f)
	_, err = s.SetRepo(context.Background(), connect.NewRequest(&rafikiv1.SetRepoRequest{
		Name: "ops_tools", RefreshInterval: durationpb.New(30 * time.Second),
	}))
	c.Require().Error(err, "set with a 30s interval")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "short-interval code")
	c.StrContains(err.Error(), "refresh interval must be at least 1m", "short-interval error")
	c.Nil(f.setReq.RefreshInterval, "the manager was reached despite the refused interval")

	// An unknown name maps ErrNotFound, a malformed name never reaches the
	// manager.
	s.SetRepoManager(&fakeRepos{setErr: repos.ErrNotFound})
	_, err = s.SetRepo(context.Background(), connect.NewRequest(&rafikiv1.SetRepoRequest{Name: "gone"}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "set of an unknown name: got code")

	f = &fakeRepos{}
	s.SetRepoManager(f)
	_, err = s.SetRepo(context.Background(), connect.NewRequest(&rafikiv1.SetRepoRequest{Name: "bad.name"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "set of a malformed name: got code")
	c.Eq("", f.setReq.Name, "the manager was reached despite the malformed name")
}

// ptrString is the pointer helper the presence tests above need.
func ptrString(s string) *string { return &s }

// TestAddRepoCarriesIntervalAndRefreshOutcome pins the two additions to
// AddRepo: the optional interval reaches the manager as a duration (absent =
// on demand), and the first refresh's outcome rides the response — including
// the skill errors, which do NOT fail the call.
func TestAddRepoCarriesIntervalAndRefreshOutcome(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakeRepos{commit: "abc123", skills: 2, skillErrs: []string{"skills/demo: unreadable"}}
	s.SetRepoManager(f)

	resp, err := s.AddRepo(context.Background(), connect.NewRequest(&rafikiv1.AddRepoRequest{
		Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
		RefreshInterval: durationpb.New(10 * time.Minute),
	}))
	c.Require().NoError(err, "add with an interval")
	c.Eq(10*time.Minute, f.addInterval, "the manager got the interval")
	c.Eq("abc123", resp.Msg.GetCommit(), "add response commit")
	c.Eq(int32(2), resp.Msg.GetSkills(), "add response skills")
	c.EqDeep([]string{"skills/demo: unreadable"}, resp.Msg.GetSkillErrors(), "add response skill errors")
	c.Eq(10*time.Minute, resp.Msg.GetRow().GetRefreshInterval().AsDuration(), "add response row interval")

	// Absent interval: on demand, not an error.
	f = &fakeRepos{}
	s.SetRepoManager(f)
	_, err = s.AddRepo(context.Background(), connect.NewRequest(&rafikiv1.AddRepoRequest{
		Name: "ops_tools", Url: "https://example.net/ops.git", Ref: "main",
	}))
	c.Require().NoError(err, "add without an interval")
	c.Eq(time.Duration(0), f.addInterval, "the manager got a non-zero interval for an absent field")

	// Below the floor: refused before the manager, and nothing registered.
	f = &fakeRepos{}
	s.SetRepoManager(f)
	_, err = s.AddRepo(context.Background(), connect.NewRequest(&rafikiv1.AddRepoRequest{
		Name: "ops_tools", Url: "https://example.net/ops.git", RefreshInterval: durationpb.New(time.Second),
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "add with a 1s interval: got code")
	c.Eq((RepoRow{}), f.addRow, "the manager was reached despite the refused interval")
}

// TestListReposCarriesResolvedState pins the row's new columns: the resolved
// commit and instant, the interval, and the two counts all reach the wire, and
// an unresolved row leaves the interval and timestamp UNSET rather than
// sending a zero duration and the epoch.
func TestListReposCarriesResolvedState(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	resolved := time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)
	s.SetRepoManager(&fakeRepos{rows: []RepoRow{{
		Name: "ops_tools", URL: "https://example.net/ops.git", Ref: "main",
		Commit:          "0123456789abcdef0123456789abcdef01234567",
		RefreshInterval: 15 * time.Minute,
		ResolvedAt:      resolved,
		Skills:          4,
		Scripts:         2,
	}, {
		Name: "fresh", URL: "https://example.net/fresh.git", Ref: "main",
	}}})

	resp, err := s.ListRepos(context.Background(), connect.NewRequest(&rafikiv1.ListReposRequest{}))
	c.Require().NoError(err, "list")
	c.Require().Len(resp.Msg.GetRows(), 2, "list rows")
	row := resp.Msg.GetRows()[0]
	c.Eq("0123456789abcdef0123456789abcdef01234567", row.GetCommit(), "row commit")
	c.Eq(15*time.Minute, row.GetRefreshInterval().AsDuration(), "row interval")
	c.True(row.GetResolvedAt().AsTime().Equal(resolved), "row resolved_at = %v, want %v", row.GetResolvedAt().AsTime(), resolved)
	c.Eq(int32(4), row.GetSkills(), "row skills")
	c.Eq(int32(2), row.GetScripts(), "row scripts")

	fresh := resp.Msg.GetRows()[1]
	c.Nil(fresh.GetRefreshInterval(), "an on-demand row sent a refresh_interval")
	c.Nil(fresh.GetResolvedAt(), "a never-resolved row sent a resolved_at")
	c.Eq("", fresh.GetCommit(), "a never-resolved row sent a commit")
}
