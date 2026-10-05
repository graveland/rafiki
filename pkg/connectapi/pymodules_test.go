// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/pymodules"

	"github.com/multigres/testkit/assert"
)

type fakePymodules struct {
	rows    []PymoduleRow
	listErr error
	getErr  error
	putErr  error
	delErr  error

	// listRepo records the repo filter the last ListPymodules call carried,
	// so a test can pin the handler's pass-through of req.Msg.GetRepo().
	listRepo string

	puts []recordedPut
	dels []string
}

// recordedPut is what the fake remembers from one PutPymodule call: the three
// arguments the handler must pass through unchanged.
type recordedPut struct{ name, code, description string }

func (f *fakePymodules) ListPymodules(_ context.Context, repo string) ([]PymoduleRow, error) {
	f.listRepo = repo
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakePymodules) GetPymodule(_ context.Context, name string) (PymoduleRow, error) {
	if f.getErr != nil {
		return PymoduleRow{}, f.getErr
	}
	return PymoduleRow{Version: 7, Name: name, Description: "d", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Code: "x = 1"}, nil
}

func (f *fakePymodules) PutPymodule(_ context.Context, name, code, description string) (PymoduleRow, error) {
	if f.putErr != nil {
		return PymoduleRow{}, f.putErr
	}
	f.puts = append(f.puts, recordedPut{name: name, code: code, description: description})
	return PymoduleRow{Version: 8, Name: name, Description: description, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Code: code}, nil
}

func (f *fakePymodules) DeletePymodule(_ context.Context, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.dels = append(f.dels, name)
	return nil
}

func TestListPymodulesOmitsCode(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakePymodules{rows: []PymoduleRow{
		{Version: 2, Name: "alpha", Description: "first", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Code: "a = 1"},
		{Version: 1, Name: "beta", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Code: "b = 2"},
	}}
	s.SetPymoduleManager(f)

	resp, err := s.ListPymodules(context.Background(), connect.NewRequest(&rafikiv1.ListPymodulesRequest{}))
	c.Require().NoError(err, "list")
	c.Require().Len(resp.Msg.Rows, 2, "got %d rows, want 2", len(resp.Msg.Rows))
	for i, want := range []struct {
		version int64
		name    string
	}{{2, "alpha"}, {1, "beta"}} {
		got := resp.Msg.Rows[i]
		if got.Version != want.version || got.Name != want.name {
			t.Errorf("row %d: got version=%d name=%q, want version=%d name=%q", i, got.Version, got.Name, want.version, want.name)
		}
		c.Eq("", got.Code, "row %d (%q): list leaked code", i, got.Name)
	}
}

// TestListPymodulesPassesRepoFilterThrough pins the handler's role: it
// forwards req.Msg.GetRepo() to the manager UNCHANGED — scoping is entirely
// the manager's job (the daemon-side adapter decides what "local" and a git
// source's name mean) — and every manager-supplied field, Repo included,
// reaches the wire unmangled while code stays blanked.
func TestListPymodulesPassesRepoFilterThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakePymodules{rows: []PymoduleRow{
		{Version: 1, Name: "gitonly", Repo: "ops_tools", Code: "leaked if not blanked"},
	}}
	s.SetPymoduleManager(f)

	for _, want := range []string{"", "local", "ops_tools"} {
		if _, err := s.ListPymodules(context.Background(), connect.NewRequest(&rafikiv1.ListPymodulesRequest{Repo: want})); err != nil {
			t.Fatalf("list with repo %q: %v", want, err)
		}
		c.Eq(want, f.listRepo, "manager got repo")
	}

	resp, err := s.ListPymodules(context.Background(), connect.NewRequest(&rafikiv1.ListPymodulesRequest{Repo: "ops_tools"}))
	c.Require().NoError(err, "list")
	got := resp.Msg.GetRows()
	c.False(len(got) != 1 || got[0].GetRepo() != "ops_tools" || got[0].GetCode() != "", "rows = %+v, want the git-sourced row with Repo ops_tools and no code", got)
}

func TestGetPymoduleValidation(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakePymodules{}
	s.SetPymoduleManager(f)

	for _, name := range []string{"", "9bad"} {
		_, err := s.GetPymodule(context.Background(), connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: name}))
		c.Require().Error(err, "get with name %q: accepted", name)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "get with name %q: got code %v, want InvalidArgument", name, connect.CodeOf(err))
	}

	resp, err := s.GetPymodule(context.Background(), connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: "good_name"}))
	c.Require().NoError(err, "get with valid name")
	if resp.Msg.Row.Name != "good_name" || resp.Msg.Row.Code != "x = 1" {
		t.Errorf("get: got name=%q code=%q, want good_name/\"x = 1\"", resp.Msg.Row.Name, resp.Msg.Row.Code)
	}
}

func TestPutPymoduleRequiresCodeAndValidName(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakePymodules{}
	s.SetPymoduleManager(f)

	_, err := s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{Name: "x"}))
	c.Require().Error(err, "put without code: accepted")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "put without code: got code")

	_, err = s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{Name: "9bad", Code: "y = 1"}))
	c.Require().Error(err, "put with name 9bad: accepted")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "put with name 9bad: got code")
	c.Empty(f.puts, "store was written despite the rejections")

	resp, err := s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{
		Name: "good_name", Code: "y = 1", Description: "desc",
	}))
	c.Require().NoError(err, "put with valid name and code")
	c.Require().Len(f.puts, 1, "got %d puts, want 1", len(f.puts))
	got := f.puts[0]
	c.False(got.name != "good_name" || got.code != "y = 1" || got.description != "desc", "manager got name=%q code=%q description=%q, want good_name/\"y = 1\"/desc", got.name, got.code, got.description)
	if resp.Msg.Row.Code != "y = 1" || resp.Msg.Row.Name != "good_name" {
		t.Errorf("put response: got name=%q code=%q, want good_name/\"y = 1\"", resp.Msg.Row.Name, resp.Msg.Row.Code)
	}
}

func TestDeletePymodule(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}

	for _, tc := range []struct {
		name string
		want connect.Code
	}{
		{"", connect.CodeInvalidArgument},
		{"9bad", connect.CodeInvalidArgument},
	} {
		f := &fakePymodules{}
		s.SetPymoduleManager(f)
		_, err := s.DeletePymodule(context.Background(), connect.NewRequest(&rafikiv1.DeletePymoduleRequest{Name: tc.name}))
		c.Require().Error(err, "delete with name %q: accepted", tc.name)
		c.Eq(tc.want, connect.CodeOf(err), "delete with name %q: got code %v, want", tc.name, connect.CodeOf(err))
		c.Empty(f.dels, "store was written despite the rejection")
	}

	s.SetPymoduleManager(&fakePymodules{delErr: pymodules.ErrNotFound})
	_, err := s.DeletePymodule(context.Background(), connect.NewRequest(&rafikiv1.DeletePymoduleRequest{Name: "gone"}))
	c.Require().Error(err, "delete of unknown name: accepted")
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "delete of unknown name: got code")
	c.ErrorIs(err, pymodules.ErrNotFound, "delete of unknown name: error does not wrap ErrNotFound")
}

func TestPymodulesUnwired(t *testing.T) {
	s := &Server{}
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"ListPymodules", func() error {
			_, err := s.ListPymodules(context.Background(), connect.NewRequest(&rafikiv1.ListPymodulesRequest{}))
			return err
		}},
		{"GetPymodule", func() error {
			_, err := s.GetPymodule(context.Background(), connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: "x"}))
			return err
		}},
		{"PutPymodule", func() error {
			_, err := s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{Name: "x", Code: "y"}))
			return err
		}},
		{"DeletePymodule", func() error {
			_, err := s.DeletePymodule(context.Background(), connect.NewRequest(&rafikiv1.DeletePymoduleRequest{Name: "x"}))
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

// SetPymoduleManager(nil) is refused, not stored: a stored pointer to a nil
// interface would defeat the Unavailable path above and nil-panic the first
// handler call instead.
func TestSetPymoduleManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetPymoduleManager(nil)
	_, err := s.ListPymodules(context.Background(), connect.NewRequest(&rafikiv1.ListPymodulesRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetPymoduleManager(nil): got code")
}

// pymoduleError maps ErrNotFound to CodeNotFound and everything else to
// CodeInternal. NotFound is exercised by the tests above; this pins the
// Internal branch — a store failure must not surface as a client-side
// InvalidArgument-shaped answer.
func TestPymoduleErrorMapsInternal(t *testing.T) {
	boom := errors.New("connection refused")
	s := &Server{}
	s.SetPymoduleManager(&fakePymodules{listErr: boom})
	if _, err := s.ListPymodules(context.Background(), connect.NewRequest(&rafikiv1.ListPymodulesRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("list failure: got code %v, want Internal", connect.CodeOf(err))
	}
	s.SetPymoduleManager(&fakePymodules{getErr: boom})
	if _, err := s.GetPymodule(context.Background(), connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: "x"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("get failure: got code %v, want Internal", connect.CodeOf(err))
	}
	s.SetPymoduleManager(&fakePymodules{putErr: boom})
	if _, err := s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{Name: "x", Code: "y"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("put failure: got code %v, want Internal", connect.CodeOf(err))
	}
	s.SetPymoduleManager(&fakePymodules{delErr: boom})
	_, err := s.DeletePymodule(context.Background(), connect.NewRequest(&rafikiv1.DeletePymoduleRequest{Name: "x"}))
	assert.NewCollecting(t).Eq(connect.CodeInternal, connect.CodeOf(err), "delete failure: got code")
}
