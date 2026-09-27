// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

type fakeSkills struct {
	rows      []SkillRow
	upserts   []SkillRow
	getErr    error
	upsertErr error
	// sawIncludeDisabled captures the flag ListSkills handed the manager, so a
	// test can pin the pass-through half of the include_disabled contract. The
	// inversion into the store's enabledOnly belongs to the daemon's adapter
	// (cmd/rafikid's connectSkills), one layer up — a verb that inverted here
	// too would double-flip it there.
	sawIncludeDisabled bool
}

func (f *fakeSkills) ListSkills(_ context.Context, includeDisabled bool) ([]SkillRow, error) {
	f.sawIncludeDisabled = includeDisabled
	return f.rows, nil
}
func (f *fakeSkills) GetSkill(_ context.Context, ns, name string) (SkillRow, error) {
	if f.getErr != nil {
		return SkillRow{}, f.getErr
	}
	return SkillRow{Namespace: ns, Name: name, Body: "b"}, nil
}
func (f *fakeSkills) UpsertSkill(_ context.Context, r SkillRow) (SkillRow, error) {
	if f.upsertErr != nil {
		return SkillRow{}, f.upsertErr
	}
	f.upserts = append(f.upserts, r)
	return r, nil
}
func (f *fakeSkills) DeleteSkill(context.Context, string, string) error           { return nil }
func (f *fakeSkills) SetSkillEnabled(context.Context, string, string, bool) error { return nil }

// The daemon's own startup sync owns 'rafiki-core'. A client that could claim
// it could plant a row the next sync would then prune or fight over, so the
// verb refuses it outright.
func TestUpsertSkillRejectsTheReservedCoreSource(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	f := &fakeSkills{}
	s.SetSkillManager(f)

	_, err := s.UpsertSkill(context.Background(), connect.NewRequest(&rafikiv1.UpsertSkillRequest{
		Name: "x", Body: "y", Source: "rafiki-core",
	}))
	c.Require().Error(err, "upsert accepted the reserved source")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
	c.Empty(f.upserts, "store was written despite the rejection")
}

func TestUpsertSkillDefaultsNamespaceAndSource(t *testing.T) {
	c := assert.NewAborting(t)
	s := &Server{}
	f := &fakeSkills{}
	s.SetSkillManager(f)

	_, err := s.UpsertSkill(context.Background(), connect.NewRequest(&rafikiv1.UpsertSkillRequest{
		Name: "x", Body: "y",
	}))
	c.NoError(err, "upsert")
	c.Len(f.upserts, 1, "got %d upserts, want 1", len(f.upserts))
	if got := f.upserts[0]; got.Namespace != "rafiki" || got.Source != "manual" {
		t.Errorf("got namespace=%q source=%q, want rafiki/manual", got.Namespace, got.Source)
	}
}

// A name held by an enabled row of another source is an ANSWER about the
// corpus, like a missing skill: it must surface as AlreadyExists so a client
// can tell the operator "disable or delete the incumbent first" instead of
// reporting a broken daemon.
func TestUpsertSkillMapsSourceConflictToAlreadyExists(t *testing.T) {
	s := &Server{}
	s.SetSkillManager(&fakeSkills{upsertErr: ErrSkillSourceConflict})

	_, err := s.UpsertSkill(context.Background(), connect.NewRequest(&rafikiv1.UpsertSkillRequest{
		Name: "x", Body: "y",
	}))
	assert.NewCollecting(t).Eq(connect.CodeAlreadyExists, connect.CodeOf(err), "got code %v (%v), want AlreadyExists", connect.CodeOf(err), err)
}

// The two halves of a qualified name are a model-facing identifier and a
// tool argument: a colon breaks the "ns:name" parse on the way back, and a
// space, slash, dot or control character reaches the paths that render or
// store them. The verb is the one gate every client goes through, so the
// check lives here rather than in the CLI.
func TestUpsertSkillRejectsNonSlugNamespacesAndNames(t *testing.T) {
	c := assert.NewCollecting(t)
	// namespace "" is deliberately absent: it is the documented spelling of
	// the default namespace, not a bad value.
	bad := []string{"has space", "a:b", "a/b", "a\tb", ".", ".."}
	for _, val := range bad {
		for _, what := range []string{"name", "namespace"} {
			req := &rafikiv1.UpsertSkillRequest{Name: "valid-name", Body: "y"}
			if what == "name" {
				req.Name = val
			} else {
				req.Namespace = val
			}
			s := &Server{}
			f := &fakeSkills{}
			s.SetSkillManager(f)
			_, err := s.UpsertSkill(context.Background(), connect.NewRequest(req))
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "%s=%q: got code %v (%v), want InvalidArgument", what, val, connect.CodeOf(err), err)
			c.Empty(f.upserts, "%s=%q: store was written despite the rejection", what, val)
		}
	}

	// The control: a real slug still reaches the manager, so the guard cannot
	// have tightened into a blanket refusal.
	s := &Server{}
	f := &fakeSkills{}
	s.SetSkillManager(f)
	_, err := s.UpsertSkill(context.Background(), connect.NewRequest(&rafikiv1.UpsertSkillRequest{
		Namespace: "my-plugin", Name: "design-postgres-tables", Body: "y",
	}))
	c.Require().NoError(err, "valid slug rejected")
	c.Require().Len(f.upserts, 1, "got %d upserts, want 1", len(f.upserts))
}

// A missing skill is an ANSWER (NotFound), not an internal failure. Getting
// this wrong makes a typo look like a broken daemon.
func TestGetSkillMapsNotFound(t *testing.T) {
	s := &Server{}
	s.SetSkillManager(&fakeSkills{getErr: ErrSkillNotFound})

	_, err := s.GetSkill(context.Background(), connect.NewRequest(&rafikiv1.GetSkillRequest{
		Namespace: "rafiki", Name: "nope",
	}))
	assert.NewCollecting(t).Eq(connect.CodeNotFound, connect.CodeOf(err), "got code %v (%v), want NotFound", connect.CodeOf(err), err)
	_ = errors.Is(err, nil)
}

// An inventory carries no documents: ListSkills must strip Body even when the
// manager returns populated rows. Dropping the strip would ship the whole
// corpus into every list response, silently.
func TestListSkillsOmitsBodies(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	s.SetSkillManager(&fakeSkills{rows: []SkillRow{
		{Namespace: "rafiki", Name: "big", Body: "the entire skill document"},
	}})

	resp, err := s.ListSkills(context.Background(), connect.NewRequest(&rafikiv1.ListSkillsRequest{}))
	c.Require().NoError(err)
	rows := resp.Msg.GetRows()
	c.Require().Len(rows, 1, "got %d rows, want 1", len(rows))
	got := rows[0].GetBody()
	c.Eq("", got, "list response carried a body of %d bytes, want empty", len(got))
	// The strip must cost only the body: the row itself survives.
	if rows[0].GetName() != "big" || rows[0].GetNamespace() != "rafiki" {
		t.Errorf("row fields lost along with the body: %+v", rows[0])
	}
}

// include_disabled must reach the manager exactly as the caller sent it. The
// daemon-side adapter inverts it into the store's enabledOnly, so an inversion
// here as well would double-flip and --all would silently stop working.
func TestListSkillsPassesIncludeDisabledThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct{ include, want bool }{
		{false, false},
		{true, true},
	} {
		f := &fakeSkills{}
		s := &Server{}
		s.SetSkillManager(f)
		_, err := s.ListSkills(context.Background(),
			connect.NewRequest(&rafikiv1.ListSkillsRequest{IncludeDisabled: tc.include}))
		c.Require().NoError(err)
		c.Eq(tc.want, f.sawIncludeDisabled, "IncludeDisabled=%v: manager got %v, want", tc.include, f.sawIncludeDisabled)
	}
}

// Same failure shape as TestListExecutorsUnwiredIsUnavailable: a request that
// arrives before main.go wires the backend fails closed, not with a nil deref.
func TestListSkillsUnwiredIsUnavailable(t *testing.T) {
	s := &Server{}
	_, err := s.ListSkills(context.Background(), connect.NewRequest(&rafikiv1.ListSkillsRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "want CodeUnavailable, got %v", err)
}

// SetSkillManager(nil) is refused, not stored — same rule as
// SetPymoduleManager: a stored pointer to a nil interface would defeat the
// Unavailable path above and nil-panic the first handler call instead.
func TestSetSkillManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetSkillManager(nil)
	_, err := s.ListSkills(context.Background(), connect.NewRequest(&rafikiv1.ListSkillsRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "after SetSkillManager(nil): want CodeUnavailable, got %v", err)
}
