// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/presets"
)

type fakePresets struct {
	rows    []presets.Record
	getRows []presets.Record
	listErr error
	getErr  error
	putErr  error
	delErr  error

	// specs records the Spec the last PutPreset calls carried, so a test can
	// pin the tri-state's survival across the wire.
	specs []presets.Spec
	dels  []string
}

func (f *fakePresets) ListPresets(_ context.Context, prefix string) ([]presets.Record, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakePresets) GetPreset(_ context.Context, name string, history bool) ([]presets.Record, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getRows, nil
}

func (f *fakePresets) PutPreset(_ context.Context, spec presets.Spec) (presets.Record, error) {
	if f.putErr != nil {
		return presets.Record{}, f.putErr
	}
	f.specs = append(f.specs, spec)
	return spec.Record(), nil
}

func (f *fakePresets) DeletePreset(_ context.Context, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.dels = append(f.dels, name)
	return nil
}

// SetPresetManager(nil) is refused, not stored: a stored pointer to a nil
// interface would defeat the Unavailable path and nil-panic the first
// handler call instead.
func TestSetPresetManagerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetPresetManager(nil)
	_, err := s.ListPresets(context.Background(), connect.NewRequest(&rafikiv1.ListPresetsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("after SetPresetManager(nil): got code %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestPresetHandlersUnwiredAreUnavailable(t *testing.T) {
	s := &Server{}
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"ListPresets", func() error {
			_, err := s.ListPresets(context.Background(), connect.NewRequest(&rafikiv1.ListPresetsRequest{}))
			return err
		}},
		{"GetPreset", func() error {
			_, err := s.GetPreset(context.Background(), connect.NewRequest(&rafikiv1.GetPresetRequest{Name: "x"}))
			return err
		}},
		{"PutPreset", func() error {
			_, err := s.PutPreset(context.Background(), connect.NewRequest(&rafikiv1.PutPresetRequest{
				Preset: &rafikiv1.PresetRow{Name: "x"},
			}))
			return err
		}},
		{"DeletePreset", func() error {
			_, err := s.DeletePreset(context.Background(), connect.NewRequest(&rafikiv1.DeletePresetRequest{Name: "x"}))
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

// TestPresetHandlersMapErrors pins presetError's three branches: a manager's
// ErrNotFound is CodeNotFound, an ErrInvalidPreset wrapping is
// CodeInvalidArgument, and anything else — a store failure — is
// CodeInternal.
func TestPresetHandlersMapErrors(t *testing.T) {
	boom := errors.New("connection refused")

	s := &Server{}
	s.SetPresetManager(&fakePresets{getErr: presets.ErrNotFound})
	_, err := s.GetPreset(context.Background(), connect.NewRequest(&rafikiv1.GetPresetRequest{Name: "gone"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("get of unknown preset: got code %v, want NotFound", connect.CodeOf(err))
	}
	if !errors.Is(err, presets.ErrNotFound) {
		t.Errorf("get of unknown preset: error does not wrap ErrNotFound: %v", err)
	}

	s = &Server{}
	s.SetPresetManager(&fakePresets{delErr: presets.ErrNotFound})
	_, err = s.DeletePreset(context.Background(), connect.NewRequest(&rafikiv1.DeletePresetRequest{Name: "gone"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("delete of unknown preset: got code %v, want NotFound", connect.CodeOf(err))
	}

	s = &Server{}
	s.SetPresetManager(&fakePresets{putErr: fmt.Errorf("%w: bad kind", ErrInvalidPreset)})
	_, err = s.PutPreset(context.Background(), connect.NewRequest(&rafikiv1.PutPresetRequest{
		Preset: &rafikiv1.PresetRow{Name: "x"},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("put with invalid preset: got code %v, want InvalidArgument", connect.CodeOf(err))
	}

	s = &Server{}
	s.SetPresetManager(&fakePresets{listErr: boom})
	_, err = s.ListPresets(context.Background(), connect.NewRequest(&rafikiv1.ListPresetsRequest{}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("list failure: got code %v, want Internal", connect.CodeOf(err))
	}

	s = &Server{}
	s.SetPresetManager(&fakePresets{getErr: boom})
	_, err = s.GetPreset(context.Background(), connect.NewRequest(&rafikiv1.GetPresetRequest{Name: "x"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("get failure: got code %v, want Internal", connect.CodeOf(err))
	}

	s = &Server{}
	s.SetPresetManager(&fakePresets{putErr: boom})
	_, err = s.PutPreset(context.Background(), connect.NewRequest(&rafikiv1.PutPresetRequest{
		Preset: &rafikiv1.PresetRow{Name: "x"},
	}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("put failure: got code %v, want Internal", connect.CodeOf(err))
	}
}

// TestPutPresetPreservesEmptyToolsOverTheWire pins the reason StringList
// exists: a PRESENT-AND-EMPTY tools list must arrive at the manager as a
// non-nil pointer to an empty slice ("none"), not nil ("the kind's default
// = everything"), and an absent tools must arrive nil.
func TestPutPresetPreservesEmptyToolsOverTheWire(t *testing.T) {
	s := &Server{}
	f := &fakePresets{}
	s.SetPresetManager(f)

	_, err := s.PutPreset(context.Background(), connect.NewRequest(&rafikiv1.PutPresetRequest{
		Preset: &rafikiv1.PresetRow{Name: "none", Tools: &rafikiv1.StringList{Items: []string{}}},
	}))
	if err != nil {
		t.Fatalf("put with empty tools: %v", err)
	}
	if len(f.specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(f.specs))
	}
	none := f.specs[0].Tools
	if none == nil || len(*none) != 0 {
		t.Errorf("empty StringList arrived as %#v, want non-nil empty (\"none\")", none)
	}

	_, err = s.PutPreset(context.Background(), connect.NewRequest(&rafikiv1.PutPresetRequest{
		Preset: &rafikiv1.PresetRow{Name: "default"},
	}))
	if err != nil {
		t.Fatalf("put without tools: %v", err)
	}
	if got := f.specs[1].Tools; got != nil {
		t.Errorf("absent StringList arrived as %#v, want nil (the kind's default)", got)
	}

	if f.specs[0].Name != "none" || f.specs[1].Name != "default" {
		t.Errorf("specs = %q, %q; want none, default", f.specs[0].Name, f.specs[1].Name)
	}
}

// TestPutPresetRequiresPresetAndName pins the handler-side argument checks:
// a missing preset or a preset with no name is rejected with
// CodeInvalidArgument before the manager is reached.
func TestPutPresetRequiresPresetAndName(t *testing.T) {
	s := &Server{}
	f := &fakePresets{}
	s.SetPresetManager(f)

	for _, tc := range []struct {
		name   string
		preset *rafikiv1.PresetRow
	}{
		{"no preset", nil},
		{"no name", &rafikiv1.PresetRow{}},
	} {
		_, err := s.PutPreset(context.Background(), connect.NewRequest(&rafikiv1.PutPresetRequest{Preset: tc.preset}))
		if err == nil {
			t.Fatalf("put with %s: accepted", tc.name)
		}
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("put with %s: got code %v, want InvalidArgument", tc.name, connect.CodeOf(err))
		}
	}
	if len(f.specs) != 0 {
		t.Errorf("store was written despite the rejections: %+v", f.specs)
	}

	// An empty name is also required on Get and Delete.
	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"GetPreset", func() error {
			_, err := s.GetPreset(context.Background(), connect.NewRequest(&rafikiv1.GetPresetRequest{}))
			return err
		}},
		{"DeletePreset", func() error {
			_, err := s.DeletePreset(context.Background(), connect.NewRequest(&rafikiv1.DeletePresetRequest{}))
			return err
		}},
	} {
		err := tc.fn()
		if err == nil {
			t.Errorf("%s with empty name: accepted", tc.name)
			continue
		}
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s with empty name: got code %v, want InvalidArgument", tc.name, connect.CodeOf(err))
		}
	}

	resp, err := s.PutPreset(context.Background(), connect.NewRequest(&rafikiv1.PutPresetRequest{
		Preset: &rafikiv1.PresetRow{Name: "good_name", Kind: "fundi"},
	}))
	if err != nil {
		t.Fatalf("put with preset and name: %v", err)
	}
	if resp.Msg.Preset.GetName() != "good_name" {
		t.Errorf("put response name = %q, want good_name", resp.Msg.Preset.GetName())
	}
}
