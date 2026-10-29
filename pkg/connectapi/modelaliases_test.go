// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelalias"

	"github.com/multigres/testkit/assert"
)

type fakeModelAliases struct {
	rows    []modelalias.Alias
	listErr error
	putErr  error
	delErr  error

	// puts and dels record what the handlers handed down, so a test can pin
	// that the request's own name/target reach the manager unchanged.
	puts [][2]string
	dels []string
}

func (f *fakeModelAliases) ListModelAliases(_ context.Context) ([]modelalias.Alias, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeModelAliases) PutModelAlias(_ context.Context, name, target string) (modelalias.Alias, error) {
	if f.putErr != nil {
		return modelalias.Alias{}, f.putErr
	}
	f.puts = append(f.puts, [2]string{name, target})
	return modelalias.Alias{
		Name:      name,
		Target:    target,
		CreatedAt: time.Unix(1, 0).UTC(),
		UpdatedAt: time.Unix(2, 0).UTC(),
	}, nil
}

func (f *fakeModelAliases) DeleteModelAlias(_ context.Context, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.dels = append(f.dels, name)
	return nil
}

// SetModelAliasManager(nil) is refused, not stored: a stored pointer to a nil
// interface would defeat the Unavailable path and nil-panic the first handler
// call instead.
func TestSetModelAliasManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetModelAliasManager(nil)
	assert.NewAborting(t).Nil(s.modelAliases.Load(), "SetModelAliasManager(nil) stored a pointer")
	_, err := s.ListModelAliases(context.Background(), connect.NewRequest(&rafikiv1.ListModelAliasesRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetModelAliasManager(nil): got code")
}

// Every handler answers Unavailable before anyone wires a backend, rather than
// nil-panicking.
func TestModelAliasHandlersUnwiredAreUnavailable(t *testing.T) {
	s := &Server{}
	c := assert.NewAborting(t)
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"ListModelAliases", func() error {
			_, err := s.ListModelAliases(context.Background(), connect.NewRequest(&rafikiv1.ListModelAliasesRequest{}))
			return err
		}},
		{"PutModelAlias", func() error {
			_, err := s.PutModelAlias(context.Background(), connect.NewRequest(&rafikiv1.PutModelAliasRequest{Name: "ds", Target: "openrouter/x"}))
			return err
		}},
		{"DeleteModelAlias", func() error {
			_, err := s.DeleteModelAlias(context.Background(), connect.NewRequest(&rafikiv1.DeleteModelAliasRequest{Name: "ds"}))
			return err
		}},
	} {
		err := call.fn()
		c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "%s: got code", call.name)
	}
}

// List converts every row, timestamps included, and never returns nil for an
// empty store.
func TestModelAliasHandlersListConvertsRows(t *testing.T) {
	c := assert.NewAborting(t)
	created := time.Unix(100, 0).UTC()
	updated := time.Unix(200, 0).UTC()
	s := &Server{}
	s.SetModelAliasManager(&fakeModelAliases{rows: []modelalias.Alias{
		{Name: "ds", Target: "openrouter/x", CreatedAt: created, UpdatedAt: updated},
		{Name: "glm", Target: "openrouter/y[sort=price]"},
	}})

	res, err := s.ListModelAliases(context.Background(), connect.NewRequest(&rafikiv1.ListModelAliasesRequest{}))
	c.NoError(err, "list")
	rows := res.Msg.GetRows()
	c.Len(rows, 2, "rows")
	c.Eq("ds", rows[0].GetName(), "row 0 name")
	c.Eq("openrouter/x", rows[0].GetTarget(), "row 0 target")
	c.Eq(created, rows[0].GetCreatedAt().AsTime(), "row 0 created_at")
	c.Eq(updated, rows[0].GetUpdatedAt().AsTime(), "row 0 updated_at")
	c.Eq("glm", rows[1].GetName(), "row 1 name")

	empty := &Server{}
	empty.SetModelAliasManager(&fakeModelAliases{})
	res, err = empty.ListModelAliases(context.Background(), connect.NewRequest(&rafikiv1.ListModelAliasesRequest{}))
	c.NoError(err, "empty list")
	c.NotNil(res.Msg.GetRows(), "an empty list must not be nil")
	c.Len(res.Msg.GetRows(), 0, "empty rows")
}

// Put passes the request through and returns the stored row.
func TestModelAliasHandlersPutReturnsRow(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeModelAliases{}
	s := &Server{}
	s.SetModelAliasManager(f)

	res, err := s.PutModelAlias(context.Background(), connect.NewRequest(
		&rafikiv1.PutModelAliasRequest{Name: "ds", Target: "openrouter/~deepseek/deepseek-flash-latest"}))
	c.NoError(err, "put")
	c.EqDeep([][2]string{{"ds", "openrouter/~deepseek/deepseek-flash-latest"}}, f.puts, "manager calls")
	c.Eq("ds", res.Msg.GetRow().GetName(), "row name")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest", res.Msg.GetRow().GetTarget(), "row target")
	c.Eq(time.Unix(1, 0).UTC(), res.Msg.GetRow().GetCreatedAt().AsTime(), "row created_at")
	c.Eq(time.Unix(2, 0).UTC(), res.Msg.GetRow().GetUpdatedAt().AsTime(), "row updated_at")

	_, err = s.DeleteModelAlias(context.Background(), connect.NewRequest(&rafikiv1.DeleteModelAliasRequest{Name: "ds"}))
	c.NoError(err, "delete")
	c.EqDeep([]string{"ds"}, f.dels, "delete pass-through")
}

// The two required-field refusals never reach the manager.
func TestModelAliasHandlersRequireFields(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeModelAliases{}
	s := &Server{}
	s.SetModelAliasManager(f)

	_, err := s.PutModelAlias(context.Background(), connect.NewRequest(&rafikiv1.PutModelAliasRequest{Target: "openrouter/x"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty name on put")
	_, err = s.PutModelAlias(context.Background(), connect.NewRequest(&rafikiv1.PutModelAliasRequest{Name: "ds"}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty target on put")
	_, err = s.DeleteModelAlias(context.Background(), connect.NewRequest(&rafikiv1.DeleteModelAliasRequest{}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "empty name on delete")

	c.Len(f.puts, 0, "no put may reach the manager")
	c.Len(f.dels, 0, "no delete may reach the manager")
}

// The manager's error vocabulary maps onto Connect codes: an unknown alias is
// NotFound, a validation failure is InvalidArgument, an authoring refusal is
// PermissionDenied, and an uncoded failure stays Internal instead of leaking
// its text.
func TestModelAliasHandlersMapErrors(t *testing.T) {
	c := assert.NewAborting(t)
	for _, tc := range []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not found", modelalias.ErrNotFound, connect.CodeNotFound},
		{"wrapped not found", fmt.Errorf("modelaliasdb: get: %w", modelalias.ErrNotFound), connect.CodeNotFound},
		{"invalid", fmt.Errorf("%w: name %q must match", modelalias.ErrInvalid, "Ds"), connect.CodeInvalidArgument},
		{"authoring refused", fmt.Errorf("%w: child", ErrModelAliasAuthoring), connect.CodePermissionDenied},
		{"store failure", errors.New("dial tcp 10.0.0.5:5432: connection refused"), connect.CodeInternal},
	} {
		s := &Server{}
		s.SetModelAliasManager(&fakeModelAliases{putErr: tc.err})

		_, err := s.PutModelAlias(context.Background(), connect.NewRequest(
			&rafikiv1.PutModelAliasRequest{Name: "ds", Target: "openrouter/x"}))
		c.Eq(tc.want, connect.CodeOf(err), "%s: put code", tc.name)

		s2 := &Server{}
		s2.SetModelAliasManager(&fakeModelAliases{delErr: tc.err})
		_, err = s2.DeleteModelAlias(context.Background(), connect.NewRequest(
			&rafikiv1.DeleteModelAliasRequest{Name: "ds"}))
		c.Eq(tc.want, connect.CodeOf(err), "%s: delete code", tc.name)
	}

	s := &Server{}
	s.SetModelAliasManager(&fakeModelAliases{listErr: modelalias.ErrNotFound})
	_, err := s.ListModelAliases(context.Background(), connect.NewRequest(&rafikiv1.ListModelAliasesRequest{}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "list maps through the same mapper")
}
