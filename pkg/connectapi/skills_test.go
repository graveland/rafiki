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
	rows   []SkillRow
	getErr error
	// getRow, when set, is what GetSkill answers: the seam for the fields only
	// a single-row read carries (the body and the file inventory).
	getRow *SkillRow
	// setErr/deleteErr are the write refusals a manager returns; the handler
	// must preserve their Connect code rather than reporting internal.
	setErr    error
	deleteErr error
	// sawIncludeDisabled captures the flag ListSkills handed the manager, so a
	// test can pin the pass-through half of the include_disabled contract. The
	// daemon's adapter (cmd/rafikid's connectSkills) is where the flag is
	// applied: it lists every row and drops disabled ones itself, because
	// shadowing needs a disabled override in hand. A verb that filtered here
	// too would hide the row such an override shadows.
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
	if f.getRow != nil {
		return *f.getRow, nil
	}
	return SkillRow{Namespace: ns, Name: name, Body: "b"}, nil
}
func (f *fakeSkills) DeleteSkill(context.Context, string, string) error           { return f.deleteErr }
func (f *fakeSkills) SetSkillEnabled(context.Context, string, string, bool) error { return f.setErr }

// A missing skill is an ANSWER (NotFound), not an internal failure. Getting
// this wrong makes a typo look like a broken daemon.
func TestGetSkillMapsNotFound(t *testing.T) {
	s := &Server{}
	s.SetSkillManager(&fakeSkills{getErr: ErrSkillNotFound})

	_, err := s.GetSkill(context.Background(), connect.NewRequest(&rafikiv1.GetSkillRequest{
		Namespace: "rafiki", Name: "nope",
	}))
	assert.NewCollecting(t).Eq(connect.CodeNotFound, connect.CodeOf(err), "got code %v (%v), want NotFound", connect.CodeOf(err), err)
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

// A refusal the daemon coded deliberately must keep its code: a shared row a
// non-admin may not toggle is permission_denied, and a row whose source owns
// its lifecycle is failed_precondition. Re-wrapping either as internal makes a
// permission boundary indistinguishable from a broken daemon.
func TestSkillRefusalKeepsItsConnectCode(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		code connect.Code
	}{
		{"shared row", connect.CodePermissionDenied},
		{"source-owned row", connect.CodeFailedPrecondition},
	} {
		refused := connect.NewError(tc.code, errors.New("refused"))
		s := &Server{}
		s.SetSkillManager(&fakeSkills{setErr: refused, deleteErr: refused})

		_, err := s.SetSkillEnabled(context.Background(), connect.NewRequest(&rafikiv1.SetSkillEnabledRequest{
			Namespace: "rafiki", Name: "s", Enabled: false,
		}))
		c.Eq(tc.code, connect.CodeOf(err), "%s: SetSkillEnabled got %v (%v), want", tc.name, connect.CodeOf(err), err)

		_, err = s.DeleteSkill(context.Background(), connect.NewRequest(&rafikiv1.DeleteSkillRequest{
			Namespace: "rafiki", Name: "s",
		}))
		c.Eq(tc.code, connect.CodeOf(err), "%s: DeleteSkill got %v (%v), want", tc.name, connect.CodeOf(err), err)
	}
}

// GetSkill is the one read that carries the row's identity and its file
// inventory: a name is unique only within a layer, so id, owner, layer and
// shadowed are what tell three same-named rows apart.
func TestGetSkillCarriesLayerAndFiles(t *testing.T) {
	c := assert.NewCollecting(t)
	row := SkillRow{
		ID: "own-id", Namespace: "rafiki", Name: "s", Description: "d",
		Source: "manual", OwnerUserID: "u1", Layer: "user", Enabled: true, Body: "b",
		Files: []SkillFile{
			{Path: "reference/a.md", Mode: 0o644, Size: 12},
			{Path: "scripts/run.sh", Mode: 0o755, Size: 34},
		},
	}
	s := &Server{}
	s.SetSkillManager(&fakeSkills{getRow: &row})

	resp, err := s.GetSkill(context.Background(), connect.NewRequest(&rafikiv1.GetSkillRequest{
		Namespace: "rafiki", Name: "s",
	}))
	c.Require().NoError(err)
	got := resp.Msg.GetRow()
	c.Eq("own-id", got.GetId(), "id")
	c.Eq("u1", got.GetOwnerUserId(), "owner_user_id")
	c.Eq("user", got.GetLayer(), "layer")
	c.Eq("b", got.GetBody(), "body")
	c.Require().Len(got.GetFiles(), 2, "files")
	c.Eq("reference/a.md", got.GetFiles()[0].GetPath(), "first file path")
	c.Eq(uint32(0o644), got.GetFiles()[0].GetMode(), "first file mode")
	c.Eq(int64(12), got.GetFiles()[0].GetSize(), "first file size")
	c.Eq(uint32(0o755), got.GetFiles()[1].GetMode(), "second file mode")
}

// An inventory carries the layer and shadow flag too — that is the whole point
// of listing a shadowed row — and still carries no body.
func TestListSkillsCarriesLayerFields(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Server{}
	s.SetSkillManager(&fakeSkills{rows: []SkillRow{{
		ID: "core-id", Namespace: "rafiki", Name: "x", Source: "rafiki-core",
		Layer: "core", Enabled: true, Shadowed: true, Body: "a document",
	}}})

	resp, err := s.ListSkills(context.Background(), connect.NewRequest(&rafikiv1.ListSkillsRequest{}))
	c.Require().NoError(err)
	rows := resp.Msg.GetRows()
	c.Require().Len(rows, 1, "rows")
	c.Eq("core-id", rows[0].GetId(), "id")
	c.Eq("core", rows[0].GetLayer(), "layer")
	c.True(rows[0].GetShadowed(), "shadowed")
	c.Eq("", rows[0].GetBody(), "an inventory carries no body")
}
