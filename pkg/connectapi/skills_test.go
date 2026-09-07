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
	rows    []SkillRow
	upserts []SkillRow
	getErr  error
}

func (f *fakeSkills) ListSkills(context.Context, bool) ([]SkillRow, error) { return f.rows, nil }
func (f *fakeSkills) GetSkill(_ context.Context, ns, name string) (SkillRow, error) {
	if f.getErr != nil {
		return SkillRow{}, f.getErr
	}
	return SkillRow{Namespace: ns, Name: name, Body: "b"}, nil
}
func (f *fakeSkills) UpsertSkill(_ context.Context, r SkillRow) (SkillRow, error) {
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
