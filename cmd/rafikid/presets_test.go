package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakePresetStore is an in-memory presets.Store: resolver tests run against a
// bare &Controller{presetStore: fake} with no database behind it.
type fakePresetStore struct {
	recs   map[string]presets.Record // ownerUserID \x00 name -> latest row
	nextID int64
}

func newFakePresetStore(owner string, recs ...presets.Record) *fakePresetStore {
	s := &fakePresetStore{recs: make(map[string]presets.Record, len(recs))}
	for _, r := range recs {
		s.Put(context.Background(), owner, r) //nolint:errcheck // never fails
	}
	return s
}

func (s *fakePresetStore) Put(_ context.Context, ownerUserID string, r presets.Record) (presets.Record, error) {
	s.nextID++
	r.ID = s.nextID
	r.OwnerUserID = ownerUserID
	s.recs[ownerUserID+"\x00"+r.Name] = r
	return r, nil
}

func (s *fakePresetStore) Get(_ context.Context, ownerUserID, name string) (presets.Record, error) {
	r, ok := s.recs[ownerUserID+"\x00"+name]
	if !ok {
		return presets.Record{}, presets.ErrNotFound
	}
	return r, nil
}

func (s *fakePresetStore) List(_ context.Context, ownerUserID, prefix string) ([]presets.Record, error) {
	var out []presets.Record
	for _, r := range s.recs {
		if r.OwnerUserID == ownerUserID && strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *fakePresetStore) History(_ context.Context, ownerUserID, name string) ([]presets.Record, error) {
	r, ok := s.recs[ownerUserID+"\x00"+name]
	if !ok {
		return nil, presets.ErrNotFound
	}
	return []presets.Record{r}, nil
}

func (s *fakePresetStore) Delete(_ context.Context, ownerUserID, name string) error {
	key := ownerUserID + "\x00" + name
	if _, ok := s.recs[key]; !ok {
		return presets.ErrNotFound
	}
	delete(s.recs, key)
	return nil
}

// presetFixture is a fundi preset with an explicit id, as a store read would
// return it.
func presetFixture(name string) presets.Record {
	return presets.Record{
		ID:     7,
		Name:   name,
		Kind:   presets.KindFundi,
		Labels: map[string]string{},
	}
}

// presetController returns the bare controller applyPreset must be testable
// with: nothing but a preset store.
func presetController(store presets.Store) *Controller {
	return &Controller{presetStore: store}
}

// iPtrOf returns a pointer to v (test helper; f64ptr lives in models_rows_test.go).
func iPtrOf(v int) *int { return &v }

// wantInvalidArgs asserts err is the ControllerError applyPreset always
// returns and gives back its message.
func wantInvalidArgs(t *testing.T, err error) string {
	t.Helper()
	c := assert.NewAborting(t)
	c.Error(err, "expected an error, got nil")
	cerr, ok := err.(*connectapi.ControllerError)
	c.True(ok, "expected *connectapi.ControllerError, got %T: %v", err, err)
	c.Eq(protocol.ErrInvalidArgs, cerr.Code, "expected code %q, got %q (%s)", protocol.ErrInvalidArgs, cerr.Code, cerr.Message)
	return cerr.Message
}

func TestApplyPresetNoPresetIsIdentity(t *testing.T) {
	ck := assert.NewAborting(t)
	c := presetController(nil)
	in := protocol.SpawnRequest{
		Kind:               protocol.KindFundi,
		Cwd:                "/tmp/w",
		Model:              "m",
		Provider:           "p",
		Thinking:           "high",
		Labels:             map[string]string{"team": "core"},
		Tools:              "read,bash",
		Skills:             []string{"s"},
		MCPServers:         []string{"m"},
		SystemPrompt:       "sp",
		AppendSystemPrompt: "asp",
		ExecutorSelector:   "env=work",
		MaxCost:            f64ptr(1.5),
		MaxDepth:           iPtrOf(2),
		MaxChildren:        iPtrOf(3),
	}
	got, rec, err := c.applyPreset(context.Background(), in, "owner-1")
	ck.NoError(err)
	ck.Nil(rec, "expected nil record, got")
	ck.EqDiff(in, got, "request changed without a preset:\n in")
}

func TestApplyPresetNotFoundListsGroupOnly(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1",
		presetFixture("local:a"), presetFixture("local:b"), presetFixture("default:a"),
	)
	c := presetController(store)
	_, _, err := c.applyPreset(context.Background(), protocol.SpawnRequest{Preset: "local:zzz"}, "owner-1")
	msg := wantInvalidArgs(t, err)
	ck.StrContains(msg, `no preset "local:zzz"`, "error does not name the missing preset")
	ck.StrContains(msg, "local:a, local:b", "error does not list the group's presets")
	ck.NotStrContains(msg, "default:", "error leaked another group's presets")
}

func TestApplyPresetNoStore(t *testing.T) {
	c := presetController(nil)
	_, _, err := c.applyPreset(context.Background(), protocol.SpawnRequest{Preset: "p"}, "owner-1")
	msg := wantInvalidArgs(t, err)
	assert.NewAborting(t).StrContains(msg, "no database", "error does not mention the missing database")
}

func TestApplyPresetKindFromPresetAndConflict(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1", presetFixture("p"))
	c := presetController(store)
	ctx := context.Background()

	// Empty kind takes the preset's.
	got, _, err := c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p"}, "owner-1")
	ck.NoError(err)
	ck.Eq(presets.KindFundi, got.Kind, "kind")

	// The same kind is confirmed, not an error.
	if _, _, err := c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p", Kind: presets.KindFundi}, "owner-1"); err != nil {
		t.Fatal(err)
	}

	// A different kind is a conflict.
	_, _, err = c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p", Kind: protocol.KindClaude}, "owner-1")
	msg := wantInvalidArgs(t, err)
	want := fmt.Sprintf("kind %q conflicts with preset %q (kind %q)", protocol.KindClaude, "p", presets.KindFundi)
	ck.Eq(want, msg, "conflict message")
}

func TestApplyPresetClaudeRejectsFundiFields(t *testing.T) {
	store := newFakePresetStore("owner-1", presets.Record{
		ID: 7, Name: "cp", Kind: presets.KindClaude, Labels: map[string]string{},
	})
	c := presetController(store)
	ctx := context.Background()

	// One subtest per field, in rule 5's order: each request sets exactly one
	// of the fields a claude preset cannot honour.
	for _, tc := range []struct {
		field string
		req   protocol.SpawnRequest
	}{
		{"thinking", protocol.SpawnRequest{Thinking: "high"}},
		{"tools", protocol.SpawnRequest{Tools: "read"}},
		{"tools", protocol.SpawnRequest{NoBuiltinTools: true}},
		{"skills", protocol.SpawnRequest{Skills: []string{"s"}}},
		{"skills", protocol.SpawnRequest{NoSkills: true}},
		{"mcp_servers", protocol.SpawnRequest{MCPServers: []string{"m"}}},
		{"mcp_servers", protocol.SpawnRequest{NoMCP: true}},
		{"context_files", protocol.SpawnRequest{NoContextFiles: true}},
		{"system_prompt", protocol.SpawnRequest{SystemPrompt: "sp"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			req := tc.req
			req.Preset = "cp"
			_, _, err := c.applyPreset(ctx, req, "owner-1")
			msg := wantInvalidArgs(t, err)
			want := fmt.Sprintf("field %q does not apply to a claude preset", tc.field)
			assert.NewAborting(t).Eq(want, msg, "message")
		})
	}
}

func intPtr(i int) *int { return &i }

func TestApplyPresetScriptRejectsFundiClaudeFields(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1", presets.Record{
		ID: 9, Name: "sp", Kind: presets.KindScript, Labels: map[string]string{},
	})
	c := presetController(store)
	ctx := context.Background()

	for _, tc := range []struct {
		field string
		req   protocol.SpawnRequest
	}{
		{"thinking", protocol.SpawnRequest{Thinking: "high"}},
		{"tools", protocol.SpawnRequest{Tools: "read"}},
		{"skills", protocol.SpawnRequest{Skills: []string{"s"}}},
		{"mcp_servers", protocol.SpawnRequest{MCPServers: []string{"m"}}},
		{"context_files", protocol.SpawnRequest{NoContextFiles: true}},
		{"system_prompt", protocol.SpawnRequest{SystemPrompt: "sp"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			req := tc.req
			req.Preset = "sp"
			req.Kind = protocol.KindScript
			_, _, err := c.applyPreset(ctx, req, "owner-1")
			msg := wantInvalidArgs(t, err)
			want := fmt.Sprintf("field %q does not apply to a script preset", tc.field)
			assert.NewAborting(t).Eq(want, msg, "message")
		})
	}

	// A clean script spawn through the preset: no kind conflict, budgets and
	// executor ride through.
	req := protocol.SpawnRequest{Preset: "sp", Kind: protocol.KindScript, MaxDepth: intPtr(2)}
	resolved, rec, err := c.applyPreset(ctx, req, "owner-1")
	ck.NoError(err, "a clean script spawn through a script preset")
	ck.False(rec == nil || rec.Kind != presets.KindScript, "resolved record = %+v, want the script preset", rec)
	ck.False(resolved.MaxDepth == nil || *resolved.MaxDepth != 2, "resolved maxDepth = %v, want the request's 2", resolved.MaxDepth)
}

func TestApplyPresetModelOverrideAndDefault(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1", presets.Record{
		ID: 7, Name: "p", Kind: presets.KindFundi, Labels: map[string]string{},
		Provider: "preset-provider", Model: "preset-model", Thinking: "high",
	})
	c := presetController(store)
	ctx := context.Background()

	// Empty fields take the preset's.
	got, _, err := c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p"}, "owner-1")
	ck.NoError(err)
	ck.False(got.Provider != "preset-provider" || got.Model != "preset-model" || got.Thinking != "high", "defaults not applied: %+v", got)

	// Non-empty request fields win.
	got, _, err = c.applyPreset(ctx, protocol.SpawnRequest{
		Preset: "p", Provider: "req-provider", Model: "req-model", Thinking: "low",
	}, "owner-1")
	ck.NoError(err)
	ck.False(got.Provider != "req-provider" || got.Model != "req-model" || got.Thinking != "low", "request fields did not win: %+v", got)

	// Mixed: each field decided independently.
	got, _, err = c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p", Model: "req-model"}, "owner-1")
	ck.NoError(err)
	ck.False(got.Provider != "preset-provider" || got.Model != "req-model", "mixed override wrong: %+v", got)
}

func TestApplyPresetLabelsMergeRequestWins(t *testing.T) {
	ck := assert.NewAborting(t)
	rec := presetFixture("p")
	rec.Labels = map[string]string{"team": "core", "tier": "2"}
	store := newFakePresetStore("owner-1", rec)
	c := presetController(store)

	got, _, err := c.applyPreset(context.Background(), protocol.SpawnRequest{
		Preset: "p",
		Labels: map[string]string{"team": "req", "extra": "1"},
	}, "owner-1")
	ck.NoError(err)
	want := map[string]string{"team": "req", "tier": "2", "extra": "1"}
	ck.EqDiff(want, got.Labels, "merged labels")
	// The record's own map is untouched.
	wantRec := map[string]string{"team": "core", "tier": "2"}
	ck.EqDiff(wantRec, rec.Labels, "rec.Labels mutated")
	// And so is the caller's map.
	// (rec was copied by value into the store's map; re-read through Get.)
	fresh, err := store.Get(context.Background(), "owner-1", "p")
	ck.NoError(err)
	ck.EqDiff(wantRec, fresh.Labels, "stored record's labels mutated")
}

func TestApplyPresetSystemPromptFixed(t *testing.T) {
	ck := assert.NewAborting(t)
	rec := presetFixture("p")
	rec.SystemPrompt = "be brief"
	store := newFakePresetStore("owner-1", rec)
	c := presetController(store)
	ctx := context.Background()

	_, _, err := c.applyPreset(ctx, protocol.SpawnRequest{
		Preset: "p", SystemPrompt: "custom",
	}, "owner-1")
	msg := wantInvalidArgs(t, err)
	ck.Eq(fmt.Sprintf("system_prompt is fixed by preset %q", "p"), msg, "message =")

	got, _, err := c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p"}, "owner-1")
	ck.NoError(err)
	ck.Eq(rec.SystemPrompt, got.SystemPrompt, "system prompt")
}

func TestApplyPresetAppendOrder(t *testing.T) {
	ck := assert.NewAborting(t)
	rec := presetFixture("p")
	rec.AppendSystemPrompt = "preset-text"
	store := newFakePresetStore("owner-1", rec)
	c := presetController(store)
	ctx := context.Background()

	// Preset text first, request text second, one blank line between.
	got, _, err := c.applyPreset(ctx, protocol.SpawnRequest{
		Preset: "p", AppendSystemPrompt: "request-text",
	}, "owner-1")
	ck.NoError(err)
	ck.Eq("preset-text\n\nrequest-text", got.AppendSystemPrompt, "append")

	// Each side alone is passed through.
	got, _, err = c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p"}, "owner-1")
	ck.NoError(err)
	ck.Eq("preset-text", got.AppendSystemPrompt, "append")

	rec2 := presetFixture("p2")
	store.Put(ctx, "owner-1", rec2) //nolint:errcheck // never fails
	got, _, err = c.applyPreset(ctx, protocol.SpawnRequest{
		Preset: "p2", AppendSystemPrompt: "request-text",
	}, "owner-1")
	ck.NoError(err)
	ck.Eq("request-text", got.AppendSystemPrompt, "append")
}

func TestApplyPresetBudgetsCopyNotAlias(t *testing.T) {
	ck := assert.NewAborting(t)
	rec := presetFixture("p")
	rec.MaxCost = f64ptr(2.0)
	rec.MaxDepth = iPtrOf(4)
	rec.MaxChildren = iPtrOf(5)
	store := newFakePresetStore("owner-1", rec)
	c := presetController(store)

	got, presetRec, err := c.applyPreset(context.Background(), protocol.SpawnRequest{Preset: "p"}, "owner-1")
	ck.NoError(err)
	ck.NotNil(presetRec, "expected a record")
	ck.False(got.MaxCost == nil || got.MaxDepth == nil || got.MaxChildren == nil, "budgets not filled from preset: %+v", got)
	if *got.MaxCost != 2.0 || *got.MaxDepth != 4 || *got.MaxChildren != 5 {
		t.Fatalf("budget values wrong: %v %v %v", *got.MaxCost, *got.MaxDepth, *got.MaxChildren)
	}

	// Mutating the returned request must not reach the record.
	*got.MaxCost = 9.0
	*got.MaxDepth = 9
	*got.MaxChildren = 9
	ck.False(*presetRec.MaxCost != 2.0 || *presetRec.MaxDepth != 4 || *presetRec.MaxChildren != 5, "record aliased into the request: %+v", presetRec)
	ck.False(*rec.MaxCost != 2.0 || *rec.MaxDepth != 4 || *rec.MaxChildren != 5, "stored record aliased into the request: %+v", rec)

	// A non-nil request value is kept as-is.
	got, _, err = c.applyPreset(context.Background(), protocol.SpawnRequest{
		Preset: "p", MaxCost: f64ptr(5.0),
	}, "owner-1")
	ck.NoError(err)
	ck.Eq(5.0, *got.MaxCost, "request budget not kept")
}

func TestPresetNarrowAllowlistTable(t *testing.T) {
	open := []string(nil)
	none := []string{}
	ab := []string{"a", "b"}
	for _, tc := range []struct {
		name       string
		presetList []string
		reqList    []string
		reqOff     bool
		wantList   []string
		wantOff    bool
		wantErr    string // substring; "" = no error
	}{
		{"request-off-wins", ab, ab, true, nil, true, ""},
		{"open-preset-request-picks", open, []string{"x"}, false, []string{"x"}, false, ""},
		{"open-preset-no-request", open, nil, false, nil, false, ""},
		{"none-preset-request-errors", none, []string{"x"}, false, nil, false, `"x" is not allowed by preset "p" (allows: none)`},
		{"none-preset-no-request", none, nil, false, nil, true, ""},
		{"list-preset-request-subset", ab, []string{"a"}, false, []string{"a"}, false, ""},
		{"list-preset-request-outside", ab, []string{"x"}, false, nil, false, `"x" is not allowed by preset "p" (allows: a, b)`},
		{"list-preset-no-request", ab, nil, false, []string{"a", "b"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			list, off, err := narrowAllowlist("tools", "p", tc.presetList, tc.reqList, tc.reqOff)
			if tc.wantErr != "" {
				c.Error(err, "expected error containing %q, got list=%v off=%v", tc.wantErr, list, off)
				c.StrContains(err.Error(), tc.wantErr, "error = %q, want containing", err)
				return
			}
			c.NoError(err)
			c.Eq(tc.wantOff, off, "off")
			if tc.wantList == nil {
				c.Nil(list, "list")
			} else if !reflect.DeepEqual(list, tc.wantList) {
				t.Fatalf("list = %v, want %v", list, tc.wantList)
			}
		})
	}
}

func TestApplyPresetToolsMapToSpawnFields(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()

	// Preset [] = none: the spawn runs with no built-in tools.
	store := newFakePresetStore("owner-1", presets.Record{
		ID: 7, Name: "none", Kind: presets.KindFundi, Labels: map[string]string{},
		Tools: []string{},
	})
	c := presetController(store)
	got, _, err := c.applyPreset(ctx, protocol.SpawnRequest{Preset: "none"}, "owner-1")
	ck.NoError(err)
	ck.True(got.NoBuiltinTools, "NoBuiltinTools = false, want true")
	ck.Eq("", got.Tools, "Tools")

	// Preset nil = open: the request's zero values survive untouched.
	store2 := newFakePresetStore("owner-1", presetFixture("open"))
	c2 := presetController(store2)
	got, _, err = c2.applyPreset(ctx, protocol.SpawnRequest{Preset: "open"}, "owner-1")
	ck.NoError(err)
	ck.False(got.NoBuiltinTools || got.Tools != "", "open preset changed tool fields: Tools=%q NoBuiltinTools=%v", got.Tools, got.NoBuiltinTools)

	// Preset list = exactly those, comma-joined into req.Tools.
	store3 := newFakePresetStore("owner-1", presets.Record{
		ID: 7, Name: "two", Kind: presets.KindFundi, Labels: map[string]string{},
		Tools: []string{"read", "bash"},
	})
	c3 := presetController(store3)
	got, _, err = c3.applyPreset(ctx, protocol.SpawnRequest{Preset: "two"}, "owner-1")
	ck.NoError(err)
	ck.Eq("read,bash", got.Tools, "Tools")
	ck.False(got.NoBuiltinTools, "NoBuiltinTools = true, want false")
}

func TestApplyPresetContextFiles(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()

	off := false
	on := true
	// Preset disables context files: the request cannot re-enable them.
	store := newFakePresetStore("owner-1", presets.Record{
		ID: 7, Name: "p", Kind: presets.KindFundi, Labels: map[string]string{},
		ContextFiles: &off,
	})
	c := presetController(store)
	got, _, err := c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p"}, "owner-1")
	ck.NoError(err)
	ck.True(got.NoContextFiles, "preset with context_files=false did not set NoContextFiles")

	// Preset enables them (or leaves them alone): the request's choice stands.
	for _, cf := range []*bool{&on, nil} {
		store2 := newFakePresetStore("owner-1", presets.Record{
			ID: 7, Name: "p", Kind: presets.KindFundi, Labels: map[string]string{},
			ContextFiles: cf,
		})
		c2 := presetController(store2)
		got, _, err := c2.applyPreset(ctx, protocol.SpawnRequest{Preset: "p"}, "owner-1")
		ck.NoError(err)
		ck.False(got.NoContextFiles, "ContextFiles=%v forced NoContextFiles", cf)
	}

	// A request that already disabled them stays disabled.
	got, _, err = c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p", NoContextFiles: true}, "owner-1")
	ck.NoError(err)
	ck.True(got.NoContextFiles, "request NoContextFiles was lost")
}

func TestApplyPresetExecutorOnlyWhenUnset(t *testing.T) {
	ck := assert.NewAborting(t)
	rec := presetFixture("p")
	rec.Executor = "env=work"
	store := newFakePresetStore("owner-1", rec)
	c := presetController(store)
	ctx := context.Background()

	// Neither selector nor ref: the preset's executor fills in.
	got, _, err := c.applyPreset(ctx, protocol.SpawnRequest{Preset: "p"}, "owner-1")
	ck.NoError(err)
	ck.Eq("env=work", got.ExecutorSelector, "ExecutorSelector")

	// The request's own selector survives, no preset selector on top.
	got, _, err = c.applyPreset(ctx, protocol.SpawnRequest{
		Preset: "p", ExecutorSelector: "env=fast",
	}, "owner-1")
	ck.NoError(err)
	ck.Eq("env=fast", got.ExecutorSelector, "ExecutorSelector")

	// So does the request's ref: no preset selector is added.
	got, _, err = c.applyPreset(ctx, protocol.SpawnRequest{
		Preset: "p", ExecutorRef: "greyshift",
	}, "owner-1")
	ck.NoError(err)
	ck.Eq("", got.ExecutorSelector, "ExecutorSelector")
	ck.Eq("greyshift", got.ExecutorRef, "ExecutorRef")
}

func TestApplyPresetUnknownToolWithOpenPreset(t *testing.T) {
	store := newFakePresetStore("owner-1", presetFixture("open"))
	c := presetController(store)
	_, _, err := c.applyPreset(context.Background(), protocol.SpawnRequest{
		Preset: "open", Tools: "nosuchtool",
	}, "owner-1")
	msg := wantInvalidArgs(t, err)
	assert.NewAborting(t).StrContains(msg, `unknown tool "nosuchtool"`, "message =")
}

func TestPresetLabelsStamped(t *testing.T) {
	c := assert.NewAborting(t)
	labels := map[string]string{}
	stampPresetLabels(labels, &presets.Record{Name: "local:r", ID: 42})
	c.Eq("local:r", labels["rafiki/preset"], "rafiki/preset")
	c.Eq("42", labels["rafiki/preset-id"], "rafiki/preset-id")

	empty := map[string]string{}
	stampPresetLabels(empty, nil)
	c.Empty(empty, "nil record stamped")
}

func TestPutPresetRejectsUnknownToolAndWrapsInvalid(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1")
	c := presetController(store)
	ctx := context.Background()

	tools := []string{"nosuchtool"}
	_, err := c.putPreset(ctx, "owner-1", "", presets.Spec{Name: "p", Tools: &tools})
	ck.Error(err, "expected an error for an unknown tool")
	ck.ErrorIs(err, errPresetInvalid, "error does not wrap errPresetInvalid")
	ck.StrContains(err.Error(), `unknown tool "nosuchtool"`, "error does not name the tool: %v", err)

	// A Validate failure wraps the same sentinel.
	_, err = c.putPreset(ctx, "owner-1", "", presets.Spec{Name: "has space"})
	ck.Error(err, "expected an error for an invalid name")
	ck.ErrorIs(err, errPresetInvalid, "error does not wrap errPresetInvalid")
}

func TestPutPresetStampsWrittenByChild(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1")
	c := presetController(store)
	ctx := context.Background()

	rec, err := c.putPreset(ctx, "owner-1", "child-9", presets.Spec{Name: "p"})
	ck.NoError(err)
	ck.Eq("child-9", rec.WrittenByChild, "WrittenByChild")

	// The stored row carries the same attribution.
	got, err := store.Get(ctx, "owner-1", "p")
	ck.NoError(err)
	ck.Eq("child-9", got.WrittenByChild, "stored WrittenByChild")

	// "" is the operator's attribution and is preserved as "".
	rec, err = c.putPreset(ctx, "owner-1", "", presets.Spec{Name: "op"})
	ck.NoError(err)
	ck.Eq("", rec.WrittenByChild, "WrittenByChild")
}

func TestPresetBindingOwnerAndAttribution(t *testing.T) {
	ck := assert.NewAborting(t)
	store := newFakePresetStore("owner-1")
	c := presetController(store)
	ctx := context.Background()

	// An agent caller's binding writes its child id as attribution.
	b := newPresetBinding(c, "owner-1", "child-1")
	rec, err := b.Put(ctx, presets.Spec{Name: "p"})
	ck.NoError(err)
	ck.Eq("child-1", rec.WrittenByChild, "WrittenByChild")

	// Get/List/History see it through the same binding.
	got, err := b.Get(ctx, "p")
	ck.NoError(err)
	ck.Eq("p", got.Name, "Get = %+v", got)
	listed, err := b.List(ctx, "")
	ck.NoError(err)
	ck.False(len(listed) != 1 || listed[0].Name != "p", "List = %+v", listed)
	hist, err := b.History(ctx, "p")
	ck.NoError(err)
	ck.False(len(hist) != 1 || hist[0].Name != "p", "History = %+v", hist)

	// Another owner's binding must not see owner-1's preset.
	other := newPresetBinding(c, "owner-2", "")
	if _, err := other.Get(ctx, "p"); !errors.Is(err, presets.ErrNotFound) {
		t.Fatalf("other owner's Get = %v, want ErrNotFound", err)
	}
	if listed, err := other.List(ctx, ""); err != nil || len(listed) != 0 {
		t.Fatalf("other owner's List = %+v, %v, want empty", listed, err)
	}

	ck.NoError(b.Delete(ctx, "p"))
	if _, err := b.Get(ctx, "p"); !errors.Is(err, presets.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}

	// A DB-less daemon's binding refuses everything.
	dbless := newPresetBinding(&Controller{}, "owner-1", "")
	if _, err := dbless.Get(ctx, "p"); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("dbless Get = %v, want the no-database refusal", err)
	}
	if _, err := dbless.Put(ctx, presets.Spec{Name: "p"}); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("dbless Put = %v, want the no-database refusal", err)
	}
}
