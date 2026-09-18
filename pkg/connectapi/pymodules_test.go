// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

type fakePymodules struct {
	rows    []PymoduleRow
	listErr error
	getErr  error
	putErr  error
	delErr  error

	puts []recordedPut
	dels []string
}

// recordedPut is what the fake remembers from one PutPymodule call: the three
// arguments the handler must pass through unchanged.
type recordedPut struct{ name, code, description string }

func (f *fakePymodules) ListPymodules(_ context.Context) ([]PymoduleRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakePymodules) GetPymodule(_ context.Context, name string) (PymoduleRow, error) {
	if f.getErr != nil {
		return PymoduleRow{}, f.getErr
	}
	return PymoduleRow{Version: 7, Name: name, Description: "d", CreatedAt: "2026-01-01T00:00:00Z", Code: "x = 1"}, nil
}

func (f *fakePymodules) PutPymodule(_ context.Context, name, code, description string) (PymoduleRow, error) {
	if f.putErr != nil {
		return PymoduleRow{}, f.putErr
	}
	f.puts = append(f.puts, recordedPut{name: name, code: code, description: description})
	return PymoduleRow{Version: 8, Name: name, Description: description, CreatedAt: "2026-01-01T00:00:00Z", Code: code}, nil
}

func (f *fakePymodules) DeletePymodule(_ context.Context, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.dels = append(f.dels, name)
	return nil
}

func TestListPymodulesOmitsCode(t *testing.T) {
	s := &Server{}
	f := &fakePymodules{rows: []PymoduleRow{
		{Version: 2, Name: "alpha", Description: "first", CreatedAt: "2026-01-01T00:00:00Z", Code: "a = 1"},
		{Version: 1, Name: "beta", CreatedAt: "2026-01-02T00:00:00Z", Code: "b = 2"},
	}}
	s.SetPymoduleManager(f)

	resp, err := s.ListPymodules(context.Background(), connect.NewRequest(&rafikiv1.ListPymodulesRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Msg.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(resp.Msg.Rows))
	}
	for i, want := range []struct {
		version int64
		name    string
	}{{2, "alpha"}, {1, "beta"}} {
		got := resp.Msg.Rows[i]
		if got.Version != want.version || got.Name != want.name {
			t.Errorf("row %d: got version=%d name=%q, want version=%d name=%q", i, got.Version, got.Name, want.version, want.name)
		}
		if got.Code != "" {
			t.Errorf("row %d (%q): list leaked code %q", i, got.Name, got.Code)
		}
	}
}

func TestGetPymoduleValidation(t *testing.T) {
	s := &Server{}
	f := &fakePymodules{}
	s.SetPymoduleManager(f)

	for _, name := range []string{"", "9bad"} {
		_, err := s.GetPymodule(context.Background(), connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: name}))
		if err == nil {
			t.Fatalf("get with name %q: accepted", name)
		}
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("get with name %q: got code %v, want InvalidArgument", name, connect.CodeOf(err))
		}
	}

	resp, err := s.GetPymodule(context.Background(), connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: "good_name"}))
	if err != nil {
		t.Fatalf("get with valid name: %v", err)
	}
	if resp.Msg.Row.Name != "good_name" || resp.Msg.Row.Code != "x = 1" {
		t.Errorf("get: got name=%q code=%q, want good_name/\"x = 1\"", resp.Msg.Row.Name, resp.Msg.Row.Code)
	}
}

func TestPutPymoduleRequiresCodeAndValidName(t *testing.T) {
	s := &Server{}
	f := &fakePymodules{}
	s.SetPymoduleManager(f)

	_, err := s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{Name: "x"}))
	if err == nil {
		t.Fatal("put without code: accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("put without code: got code %v, want InvalidArgument", connect.CodeOf(err))
	}

	_, err = s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{Name: "9bad", Code: "y = 1"}))
	if err == nil {
		t.Fatal("put with name 9bad: accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("put with name 9bad: got code %v, want InvalidArgument", connect.CodeOf(err))
	}
	if len(f.puts) != 0 {
		t.Errorf("store was written despite the rejections: %+v", f.puts)
	}

	resp, err := s.PutPymodule(context.Background(), connect.NewRequest(&rafikiv1.PutPymoduleRequest{
		Name: "good_name", Code: "y = 1", Description: "desc",
	}))
	if err != nil {
		t.Fatalf("put with valid name and code: %v", err)
	}
	if len(f.puts) != 1 {
		t.Fatalf("got %d puts, want 1", len(f.puts))
	}
	got := f.puts[0]
	if got.name != "good_name" || got.code != "y = 1" || got.description != "desc" {
		t.Errorf("manager got name=%q code=%q description=%q, want good_name/\"y = 1\"/desc", got.name, got.code, got.description)
	}
	if resp.Msg.Row.Code != "y = 1" || resp.Msg.Row.Name != "good_name" {
		t.Errorf("put response: got name=%q code=%q, want good_name/\"y = 1\"", resp.Msg.Row.Name, resp.Msg.Row.Code)
	}
}

func TestDeletePymodule(t *testing.T) {
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
		if err == nil {
			t.Fatalf("delete with name %q: accepted", tc.name)
		}
		if connect.CodeOf(err) != tc.want {
			t.Errorf("delete with name %q: got code %v, want %v", tc.name, connect.CodeOf(err), tc.want)
		}
		if len(f.dels) != 0 {
			t.Errorf("store was written despite the rejection: %v", f.dels)
		}
	}

	s.SetPymoduleManager(&fakePymodules{delErr: pymodules.ErrNotFound})
	_, err := s.DeletePymodule(context.Background(), connect.NewRequest(&rafikiv1.DeletePymoduleRequest{Name: "gone"}))
	if err == nil {
		t.Fatal("delete of unknown name: accepted")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("delete of unknown name: got code %v, want NotFound", connect.CodeOf(err))
	}
	if !errors.Is(err, pymodules.ErrNotFound) {
		t.Errorf("delete of unknown name: error does not wrap ErrNotFound: %v", err)
	}
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
		if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Errorf("%s with no manager: got code %v, want Unavailable", call.name, connect.CodeOf(err))
		}
	}
}
