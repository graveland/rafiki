// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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
	s := &Server{}
	f := &fakeSkills{}
	s.SetSkillManager(f)

	_, err := s.UpsertSkill(context.Background(), connect.NewRequest(&rafikiv1.UpsertSkillRequest{
		Name: "x", Body: "y", Source: "rafiki-core",
	}))
	if err == nil {
		t.Fatal("upsert accepted the reserved source")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", connect.CodeOf(err))
	}
	if len(f.upserts) != 0 {
		t.Errorf("store was written despite the rejection: %+v", f.upserts)
	}
}

func TestUpsertSkillDefaultsNamespaceAndSource(t *testing.T) {
	s := &Server{}
	f := &fakeSkills{}
	s.SetSkillManager(f)

	_, err := s.UpsertSkill(context.Background(), connect.NewRequest(&rafikiv1.UpsertSkillRequest{
		Name: "x", Body: "y",
	}))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if len(f.upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(f.upserts))
	}
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
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("got code %v (%v), want AlreadyExists", connect.CodeOf(err), err)
	}
}

// The two halves of a qualified name are a model-facing identifier and a
// tool argument: a colon breaks the "ns:name" parse on the way back, and a
// space, slash, dot or control character reaches the paths that render or
// store them. The verb is the one gate every client goes through, so the
// check lives here rather than in the CLI.
func TestUpsertSkillRejectsNonSlugNamespacesAndNames(t *testing.T) {
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
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("%s=%q: got code %v (%v), want InvalidArgument", what, val, connect.CodeOf(err), err)
			}
			if len(f.upserts) != 0 {
				t.Errorf("%s=%q: store was written despite the rejection", what, val)
			}
		}
	}

	// The control: a real slug still reaches the manager, so the guard cannot
	// have tightened into a blanket refusal.
	s := &Server{}
	f := &fakeSkills{}
	s.SetSkillManager(f)
	if _, err := s.UpsertSkill(context.Background(), connect.NewRequest(&rafikiv1.UpsertSkillRequest{
		Namespace: "my-plugin", Name: "design-postgres-tables", Body: "y",
	})); err != nil {
		t.Fatalf("valid slug rejected: %v", err)
	}
	if len(f.upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(f.upserts))
	}
}

// A missing skill is an ANSWER (NotFound), not an internal failure. Getting
// this wrong makes a typo look like a broken daemon.
func TestGetSkillMapsNotFound(t *testing.T) {
	s := &Server{}
	s.SetSkillManager(&fakeSkills{getErr: ErrSkillNotFound})

	_, err := s.GetSkill(context.Background(), connect.NewRequest(&rafikiv1.GetSkillRequest{
		Namespace: "rafiki", Name: "nope",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("got code %v (%v), want NotFound", connect.CodeOf(err), err)
	}
	_ = errors.Is(err, nil)
}

// An inventory carries no documents: ListSkills must strip Body even when the
// manager returns populated rows. Dropping the strip would ship the whole
// corpus into every list response, silently.
func TestListSkillsOmitsBodies(t *testing.T) {
	s := &Server{}
	s.SetSkillManager(&fakeSkills{rows: []SkillRow{
		{Namespace: "rafiki", Name: "big", Body: "the entire skill document"},
	}})

	resp, err := s.ListSkills(context.Background(), connect.NewRequest(&rafikiv1.ListSkillsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	rows := resp.Msg.GetRows()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := rows[0].GetBody(); got != "" {
		t.Errorf("list response carried a body of %d bytes, want empty", len(got))
	}
	// The strip must cost only the body: the row itself survives.
	if rows[0].GetName() != "big" || rows[0].GetNamespace() != "rafiki" {
		t.Errorf("row fields lost along with the body: %+v", rows[0])
	}
}

// include_disabled must reach the manager exactly as the caller sent it. The
// daemon-side adapter inverts it into the store's enabledOnly, so an inversion
// here as well would double-flip and --all would silently stop working.
func TestListSkillsPassesIncludeDisabledThrough(t *testing.T) {
	for _, tc := range []struct{ include, want bool }{
		{false, false},
		{true, true},
	} {
		f := &fakeSkills{}
		s := &Server{}
		s.SetSkillManager(f)
		if _, err := s.ListSkills(context.Background(),
			connect.NewRequest(&rafikiv1.ListSkillsRequest{IncludeDisabled: tc.include})); err != nil {
			t.Fatal(err)
		}
		if f.sawIncludeDisabled != tc.want {
			t.Errorf("IncludeDisabled=%v: manager got %v, want %v", tc.include, f.sawIncludeDisabled, tc.want)
		}
	}
}

// Same failure shape as TestListExecutorsUnwiredIsUnavailable: a request that
// arrives before main.go wires the backend fails closed, not with a nil deref.
func TestListSkillsUnwiredIsUnavailable(t *testing.T) {
	s := &Server{}
	_, err := s.ListSkills(context.Background(), connect.NewRequest(&rafikiv1.ListSkillsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("want CodeUnavailable, got %v", err)
	}
}
