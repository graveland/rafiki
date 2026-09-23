// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/presets"
)

// fakePresetStore is an in-memory PresetStore. It records what the tools
// hand it so a test can assert the tri-state (nil vs non-nil empty slice)
// survived the trip, and returns canned records and errors.
type fakePresetStore struct {
	listRecs   []presets.Record
	listPrefix string
	listErr    error

	getRec  presets.Record
	getName string
	getErr  error

	histRecs []presets.Record
	histName string
	histErr  error

	puts   []presets.Spec
	putRec presets.Record
	putErr error
	nextID int64

	deleted   []string
	deleteErr error
}

func (s *fakePresetStore) List(_ context.Context, prefix string) ([]presets.Record, error) {
	s.listPrefix = prefix
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.listRecs, nil
}

func (s *fakePresetStore) Get(_ context.Context, name string) (presets.Record, error) {
	s.getName = name
	if s.getErr != nil {
		return presets.Record{}, s.getErr
	}
	return s.getRec, nil
}

func (s *fakePresetStore) History(_ context.Context, name string) ([]presets.Record, error) {
	s.histName = name
	if s.histErr != nil {
		return nil, s.histErr
	}
	return s.histRecs, nil
}

func (s *fakePresetStore) Put(_ context.Context, spec presets.Spec) (presets.Record, error) {
	if s.putErr != nil {
		return presets.Record{}, s.putErr
	}
	s.puts = append(s.puts, spec)
	s.nextID++
	rec := s.putRec
	if rec.ID == 0 {
		rec.ID = s.nextID
	}
	if rec.Name == "" {
		rec.Name = spec.Name
	}
	return rec, nil
}

func (s *fakePresetStore) Delete(_ context.Context, name string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, name)
	return nil
}

// With no store configured all four blueprints decline: they never register
// as tools that can only answer "not configured". Same rule PyModuleStore's
// tools follow.
func TestPresetToolsDeclineWithoutStore(t *testing.T) {
	for _, bp := range []Materializer{
		PresetListBlueprint{}, PresetGetBlueprint{}, PresetPutBlueprint{}, PresetDeleteBlueprint{},
	} {
		tool, err := bp.Materialize(ToolOpts{})
		if tool != nil || err != nil {
			t.Errorf("%T.Materialize with nil Presets = (%v, %v), want (nil, nil)", bp, tool, err)
		}
	}
}

// The tri-state must survive the tool boundary: JSON [] is "none" (a non-nil
// pointer to an empty slice), an absent field is "no request" (nil). The
// zero-value trap -- collapsing the two -- silently turns a narrow preset
// into an everything preset.
func TestPresetPutKeepsEmptyToolsDistinct(t *testing.T) {
	store := &fakePresetStore{}
	tool, err := PresetPutBlueprint{}.Materialize(ToolOpts{Presets: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{"name":"a","tools":[]}`)); err != nil {
		t.Fatalf("Execute(tools:[]): %v", err)
	}
	if len(store.puts) != 1 {
		t.Fatalf("store Put calls = %d, want 1", len(store.puts))
	}
	if got := store.puts[0].Tools; got == nil || len(*got) != 0 {
		t.Errorf(`"tools":[] arrived as %#v, want a non-nil pointer to an empty slice`, got)
	}

	store2 := &fakePresetStore{}
	tool2, err := PresetPutBlueprint{}.Materialize(ToolOpts{Presets: store2})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := tool2.Execute(context.Background(), ToolInput(`{"name":"a"}`)); err != nil {
		t.Fatalf("Execute(no tools): %v", err)
	}
	if got := store2.puts[0].Tools; got != nil {
		t.Errorf("absent tools arrived as %#v, want nil", got)
	}
}

// A not-found from the store must keep wrapping presets.ErrNotFound so a
// caller can errors.Is against the domain sentinel, while reading as the
// tool's own sentence.
func TestPresetGetNotFoundWrapsErrNotFound(t *testing.T) {
	tool, err := PresetGetBlueprint{}.Materialize(ToolOpts{Presets: &fakePresetStore{getErr: presets.ErrNotFound}})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	_, err = tool.Execute(context.Background(), ToolInput(`{"name":"nope"}`))
	if err == nil || !errors.Is(err, presets.ErrNotFound) {
		t.Fatalf("Execute(nope) = %v, want an error wrapping presets.ErrNotFound", err)
	}
	if want := `preset_get: no preset "nope"`; err.Error() != want {
		t.Errorf("Execute error = %q, want exactly %q", err.Error(), want)
	}

	histTool, err := PresetGetBlueprint{}.Materialize(ToolOpts{Presets: &fakePresetStore{histErr: presets.ErrNotFound}})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := histTool.Execute(context.Background(), ToolInput(`{"name":"nope","history":true}`)); err == nil || !errors.Is(err, presets.ErrNotFound) {
		t.Errorf("Execute(history of nope) = %v, want an error wrapping presets.ErrNotFound", err)
	}
}

// history=true renders every version as JSON, newest first as the store
// returns them, with deleted_at only on deleted rows and written_by_child
// only when set -- and the spec's tri-state visible through the JSON.
func TestPresetGetHistoryJSON(t *testing.T) {
	live := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	gone := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakePresetStore{histRecs: []presets.Record{
		{ID: 3, Name: "default:impl", Kind: presets.KindFundi, Model: "m/one", WrittenByChild: "c_child",
			Tools: []string{}, CreatedAt: live},
		{ID: 1, Name: "default:impl", Kind: presets.KindFundi, Model: "m/old", CreatedAt: gone, DeletedAt: &gone},
	}}
	tool, err := PresetGetBlueprint{}.Materialize(ToolOpts{Presets: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"default:impl","history":true}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var versions []map[string]any
	if err := json.Unmarshal([]byte(res.Text), &versions); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, res.Text)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2:\n%s", len(versions), res.Text)
	}
	if versions[0]["version"] != float64(3) || versions[1]["version"] != float64(1) {
		t.Errorf("versions out of order: %v, %v", versions[0]["version"], versions[1]["version"])
	}
	if got := versions[0]["created_at"]; got != live.UTC().Format(time.RFC3339) {
		t.Errorf("created_at = %v, want %v (RFC3339)", got, live.UTC().Format(time.RFC3339))
	}
	if got := versions[0]["written_by_child"]; got != "c_child" {
		t.Errorf("written_by_child = %v, want c_child", got)
	}
	if _, ok := versions[0]["deleted_at"]; ok {
		t.Error("a live row must not carry deleted_at")
	}
	if _, ok := versions[1]["written_by_child"]; ok {
		t.Error("written_by_child must be omitted when empty")
	}
	if _, ok := versions[1]["deleted_at"]; !ok {
		t.Error("a deleted history row must carry deleted_at")
	}
	spec0, ok := versions[0]["spec"].(map[string]any)
	if !ok {
		t.Fatalf("version 0 carries no spec object: %v", versions[0])
	}
	if tools, ok := spec0["tools"].([]any); !ok || len(tools) != 0 {
		t.Errorf("a non-nil empty Tools must render as [], got %v", spec0["tools"])
	}
	spec1, ok := versions[1]["spec"].(map[string]any)
	if !ok {
		t.Fatalf("version 1 carries no spec object: %v", versions[1])
	}
	if _, ok := spec1["tools"]; ok {
		t.Errorf("a nil Tools must be omitted, got %v", spec1["tools"])
	}
}

// The default (no history) form returns exactly the latest live version, and
// its pretty JSON carries the version stamp and full spec.
func TestPresetGetLatestJSON(t *testing.T) {
	fixed := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	cost := 2.5
	store := &fakePresetStore{getRec: presets.Record{
		ID: 7, Name: "default:impl", Kind: presets.KindFundi, CreatedAt: fixed,
		Labels: map[string]string{"team": "core"}, MaxCost: &cost,
	}}
	tool, err := PresetGetBlueprint{}.Materialize(ToolOpts{Presets: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"default:impl"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Text, "{\n  \"version\": 7") {
		t.Errorf("output is not MarshalIndent-two-spaces JSON naming version 7; got:\n%s", res.Text)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(res.Text), &obj); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, res.Text)
	}
	if _, ok := obj["written_by_child"]; ok {
		t.Error("written_by_child must be omitted when empty")
	}
	if _, ok := obj["deleted_at"]; ok {
		t.Error("a live preset_get row must not carry deleted_at")
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		t.Fatalf("carries no spec object: %v", obj)
	}
	if spec["name"] != "default:impl" {
		t.Errorf("spec.name = %v", spec["name"])
	}
	if labels, ok := spec["labels"].(map[string]any); !ok || labels["team"] != "core" {
		t.Errorf("spec.labels = %v, want team=core", spec["labels"])
	}
	if spec["max_cost"] != cost {
		t.Errorf("spec.max_cost = %v, want %v", spec["max_cost"], cost)
	}
}

// An empty store is "No presets.", not an error; a non-empty one renders one
// tab-separated row per preset in the store's order, with a placeholder for
// an unset model.
func TestPresetListEmptyAndRows(t *testing.T) {
	store := &fakePresetStore{}
	tool, err := PresetListBlueprint{}.Materialize(ToolOpts{Presets: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{}`))
	if err != nil {
		t.Fatalf("Execute on an empty store: %v", err)
	}
	if res.Text != "No presets." {
		t.Errorf("empty store output = %q, want %q", res.Text, "No presets.")
	}

	store.listRecs = []presets.Record{
		{ID: 1, Name: "default:implementer", Kind: presets.KindFundi, Description: "impl seat"},
		{ID: 2, Name: "default:reviewer", Kind: presets.KindFundi, Model: "anthropic/claude-opus-4", Description: "review seat"},
	}
	res, err = tool.Execute(context.Background(), ToolInput(`{"prefix":"default:"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "default:implementer\tfundi\t(default model)\timpl seat\n" +
		"default:reviewer\tfundi\tanthropic/claude-opus-4\treview seat"
	if res.Text != want {
		t.Errorf("list output =\n%s\nwant\n%s", res.Text, want)
	}
	if store.listPrefix != "default:" {
		t.Errorf("prefix = %q, want it passed through to the store", store.listPrefix)
	}
}

// Every preset_* description ends with the convention text: it is the only
// place an agent learns the seat policy on any face the tools are served
// from.
func TestPresetDescriptionsCarryConvention(t *testing.T) {
	for _, d := range []string{presetListDescription, presetGetDescription, presetPutDescription, presetDeleteDescription} {
		if !strings.Contains(d, presetConvention) {
			t.Errorf("description does not carry presetConvention:\n%s", d)
		}
	}
}

func TestPresetPutReportsSavedVersion(t *testing.T) {
	store := &fakePresetStore{}
	tool, err := PresetPutBlueprint{}.Materialize(ToolOpts{Presets: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"default:implementer"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := `saved "default:implementer" as version 1`; res.Text != want {
		t.Errorf("result = %q, want %q", res.Text, want)
	}
}

func TestPresetDeleteReportsDeletedName(t *testing.T) {
	store := &fakePresetStore{}
	tool, err := PresetDeleteBlueprint{}.Materialize(ToolOpts{Presets: store})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	res, err := tool.Execute(context.Background(), ToolInput(`{"name":"default:reviewer"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := `deleted "default:reviewer"`; res.Text != want {
		t.Errorf("result = %q, want %q", res.Text, want)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "default:reviewer" {
		t.Errorf("store Delete calls = %v, want [default:reviewer]", store.deleted)
	}
}

// Each tool wraps store errors exactly once, under its own "preset_<verb>: "
// prefix -- the model sees one readable line, not a wrapped onion.
func TestPresetToolsWrapStoreErrorsWithTheirVerbPrefix(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		verb  string
		tool  Tool
		input string
	}{
		{"preset_list", mustMaterialize(t, PresetListBlueprint{}, &fakePresetStore{listErr: boom}), `{}`},
		{"preset_get", mustMaterialize(t, PresetGetBlueprint{}, &fakePresetStore{getErr: boom}), `{"name":"x"}`},
		{"preset_put", mustMaterialize(t, PresetPutBlueprint{}, &fakePresetStore{putErr: boom}), `{"name":"x"}`},
		{"preset_delete", mustMaterialize(t, PresetDeleteBlueprint{}, &fakePresetStore{deleteErr: boom}), `{"name":"x"}`},
	} {
		_, err := tc.tool.Execute(context.Background(), ToolInput(tc.input))
		if err == nil {
			t.Errorf("%s: nil error, want the store error wrapped through", tc.verb)
			continue
		}
		if !strings.HasPrefix(err.Error(), tc.verb+": ") || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s: error = %q, want it prefixed %q and carrying the store error", tc.verb, err.Error(), tc.verb+": ")
		}
	}
}

func mustMaterialize(t *testing.T, bp Materializer, store PresetStore) Tool {
	t.Helper()
	tool, err := bp.Materialize(ToolOpts{Presets: store})
	if err != nil {
		t.Fatalf("%T.Materialize: %v", bp, err)
	}
	if tool == nil {
		t.Fatalf("%T.Materialize declined with a store configured", bp)
	}
	return tool
}
