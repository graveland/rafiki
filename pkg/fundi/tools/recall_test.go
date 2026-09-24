// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/recall"
)

// fakeRecallBinding records what the tools hand it and returns canned
// results and errors, so a test can assert the mapping from tool input to
// RecallQuery fields, the required-field refusals, and the sentinel wrapping
// at the error boundary.
type fakeRecallBinding struct {
	recallQ   RecallQuery
	recallOut string
	recallErr error

	ctxHitID    string
	ctxSpan     [2]int // before, after
	ctxMaxChars int
	ctxOut      string
	ctxErr      error

	putPath, putName, putBody string
	putMeta                   json.RawMessage
	putRec                    recall.Memory
	putErr                    error

	getPath, getName string
	getRec           recall.Memory
	getErr           error

	treePath  string
	treeDepth int
	treeOut   string
	treeErr   error

	delPath, delName string
	delErr           error
}

func (f *fakeRecallBinding) Recall(_ context.Context, q RecallQuery) (string, error) {
	f.recallQ = q
	if f.recallErr != nil {
		return "", f.recallErr
	}
	return f.recallOut, nil
}

func (f *fakeRecallBinding) Context(_ context.Context, hitID string, before, after, maxChars int) (string, error) {
	f.ctxHitID = hitID
	f.ctxSpan = [2]int{before, after}
	f.ctxMaxChars = maxChars
	if f.ctxErr != nil {
		return "", f.ctxErr
	}
	return f.ctxOut, nil
}

func (f *fakeRecallBinding) MemoryPut(_ context.Context, path, name, body string, meta json.RawMessage) (recall.Memory, error) {
	f.putPath, f.putName, f.putBody, f.putMeta = path, name, body, meta
	if f.putErr != nil {
		return recall.Memory{}, f.putErr
	}
	return f.putRec, nil
}

func (f *fakeRecallBinding) MemoryGet(_ context.Context, path, name string) (recall.Memory, error) {
	f.getPath, f.getName = path, name
	return f.getRec, f.getErr
}

func (f *fakeRecallBinding) MemoryTree(_ context.Context, path string, depth int) (string, error) {
	f.treePath, f.treeDepth = path, depth
	if f.treeErr != nil {
		return "", f.treeErr
	}
	return f.treeOut, nil
}

func (f *fakeRecallBinding) MemoryDelete(_ context.Context, path, name string) error {
	f.delPath, f.delName = path, name
	return f.delErr
}

// With no binding configured all six blueprints decline: they never register
// as tools that can only answer "not configured". Same rule the preset_* and
// pymodule_* tools follow.
func TestRecallToolsDeclineWithoutBinding(t *testing.T) {
	for _, bp := range []Materializer{
		RecallBlueprint{}, RecallContextBlueprint{},
		MemoryPutBlueprint{}, MemoryGetBlueprint{}, MemoryTreeBlueprint{}, MemoryDeleteBlueprint{},
	} {
		tool, err := bp.Materialize(ToolOpts{})
		if tool != nil || err != nil {
			t.Errorf("%T.Materialize with nil Recall = (%v, %v), want (nil, nil)", bp, tool, err)
		}
	}
}

// The JSON input maps onto RecallQuery field for field, and limit is
// normalized: omitted means the recall default (10), anything above the cap
// clamps to it. An explicit in-range limit passes through untouched.
func TestRecallToolPassesQuery(t *testing.T) {
	fake := &fakeRecallBinding{recallOut: "m:abc  saved fact"}
	tool, err := RecallBlueprint{}.Materialize(ToolOpts{Recall: fake})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}

	res, err := tool.Execute(context.Background(), ToolInput(
		`{"query":"dial timeout","sources":["memory","window"],"under":"projects.rafiki",`+
			`"repo":"rafiki","since_unix":100,"until_unix":200}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Text != "m:abc  saved fact" {
		t.Errorf("result text = %q, want the binding's formatted hits verbatim", res.Text)
	}
	want := RecallQuery{
		Query:     "dial timeout",
		Sources:   []string{"memory", "window"},
		Under:     "projects.rafiki",
		Repo:      "rafiki",
		SinceUnix: 100,
		UntilUnix: 200,
		Limit:     recall.RecallDefaultLimit,
	}
	if !reflect.DeepEqual(fake.recallQ, want) || len(fake.recallQ.Sources) != 2 {
		t.Errorf("query forwarded = %+v\n        want %+v", fake.recallQ, want)
	}

	for _, tc := range []struct {
		limit int
		in    string
	}{
		{recall.RecallDefaultLimit, `{"query":"x"}`},
		{7, `{"query":"x","limit":7}`},
		{recall.RecallMaxLimit, `{"query":"x","limit":99}`},
		{recall.RecallMaxLimit, `{"query":"x","limit":1000000}`},
	} {
		if _, err := tool.Execute(context.Background(), ToolInput(tc.in)); err != nil {
			t.Fatalf("Execute(%s): %v", tc.in, err)
		}
		if fake.recallQ.Limit != tc.limit {
			t.Errorf("Execute(%s): limit = %d, want %d", tc.in, fake.recallQ.Limit, tc.limit)
		}
	}
}

func TestRecallToolRequiresQuery(t *testing.T) {
	tool, err := RecallBlueprint{}.Materialize(ToolOpts{Recall: &fakeRecallBinding{}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("Execute without query returned no error")
	} else if want := "recall: query is required"; err.Error() != want {
		t.Errorf("error = %q, want exactly %q", err.Error(), want)
	}
	def := tool.InputSchema()
	if len(def.Required) != 1 || def.Required[0] != "query" {
		t.Errorf("schema.Required = %v, want [query]", def.Required)
	}
}

// Each required field of memory_put refuses on its own, naming the field.
func TestMemoryToolPutRequiresPathNameBody(t *testing.T) {
	tool, err := MemoryPutBlueprint{}.Materialize(ToolOpts{Recall: &fakeRecallBinding{}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	for _, tc := range []struct{ in, want string }{
		{`{}`, "memory_put: path is required"},
		{`{"name":"x","body":"b"}`, "memory_put: path is required"},
		{`{"path":"p","body":"b"}`, "memory_put: name is required"},
		{`{"path":"p","name":"x"}`, "memory_put: body is required"},
	} {
		_, err := tool.Execute(context.Background(), ToolInput(tc.in))
		if err == nil {
			t.Errorf("Execute(%s) returned no error, want %q", tc.in, tc.want)
		} else if err.Error() != tc.want {
			t.Errorf("Execute(%s) error = %q, want exactly %q", tc.in, err.Error(), tc.want)
		}
	}
}

// A not-found from the store must keep wrapping recall.ErrNotFound so a
// caller can errors.Is against the domain sentinel, while reading as the
// tool's own sentence -- for memory_get, and for memory_delete, which hits
// the same sentinel for a memory that is not there.
func TestMemoryToolGetNotFoundWrapsSentinel(t *testing.T) {
	getTool, err := MemoryGetBlueprint{}.Materialize(ToolOpts{Recall: &fakeRecallBinding{getErr: recall.ErrNotFound}})
	if err != nil || getTool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", getTool, err)
	}
	_, err = getTool.Execute(context.Background(), ToolInput(`{"path":"projects.rafiki","name":"nope"}`))
	if err == nil || !errors.Is(err, recall.ErrNotFound) {
		t.Fatalf("Execute(nope) = %v, want an error wrapping recall.ErrNotFound", err)
	}
	if want := "memory_get: no memory projects.rafiki/nope"; err.Error() != want {
		t.Errorf("error = %q, want exactly %q", err.Error(), want)
	}

	delTool, err := MemoryDeleteBlueprint{}.Materialize(ToolOpts{Recall: &fakeRecallBinding{delErr: recall.ErrNotFound}})
	if err != nil || delTool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", delTool, err)
	}
	_, err = delTool.Execute(context.Background(), ToolInput(`{"path":"a.b","name":"nope"}`))
	if err == nil || !errors.Is(err, recall.ErrNotFound) {
		t.Fatalf("memory_delete Execute(nope) = %v, want an error wrapping recall.ErrNotFound", err)
	}
	if want := "memory_delete: no memory a.b/nope"; err.Error() != want {
		t.Errorf("error = %q, want exactly %q", err.Error(), want)
	}
}

// An invalid path is the domain's own sentence: it names the offending label
// and must reach the model without being rewritten into a not-found.
func TestMemoryToolInvalidPathPassesThrough(t *testing.T) {
	invalid := fmt.Errorf("x: %w", recall.ErrInvalidPath)
	tool, err := MemoryPutBlueprint{}.Materialize(ToolOpts{Recall: &fakeRecallBinding{putErr: invalid}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	_, err = tool.Execute(context.Background(), ToolInput(`{"path":"projects.a b","name":"x","body":"b"}`))
	if !errors.Is(err, recall.ErrInvalidPath) || !strings.Contains(err.Error(), "memory_put: ") {
		t.Fatalf("Execute = %v, want memory_put-prefixed error wrapping recall.ErrInvalidPath", err)
	}
	if strings.Contains(err.Error(), recall.ErrNotFound.Error()) {
		t.Errorf("error = %q, must not leak the not-found sentinel", err.Error())
	}
}

func TestMemoryToolPutForwardsAndReports(t *testing.T) {
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rec := recall.Memory{
		ID: "mem-1", Path: "projects.rafiki", Name: "dial-timeout", Body: "b",
		Meta: json.RawMessage(`{"k":"v"}`), CreatedAt: fixed, UpdatedAt: fixed,
	}
	fake := &fakeRecallBinding{putRec: rec, getRec: rec}
	put, err := MemoryPutBlueprint{}.Materialize(ToolOpts{Recall: fake})
	if err != nil || put == nil {
		t.Fatalf("Materialize: tool=%v err=%v", put, err)
	}
	res, err := put.Execute(context.Background(), ToolInput(
		`{"path":"projects.rafiki","name":"dial-timeout","body":"b","meta":{"k":"v"}}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.putPath != "projects.rafiki" || fake.putName != "dial-timeout" || fake.putBody != "b" {
		t.Errorf("put forwarded = %q/%q/%q, want path, name and body intact", fake.putPath, fake.putName, fake.putBody)
	}
	if string(fake.putMeta) != `{"k":"v"}` {
		t.Errorf("meta forwarded = %s, want the raw object", fake.putMeta)
	}
	if want := "saved memory projects.rafiki/dial-timeout"; res.Text != want {
		t.Errorf("result = %q, want %q", res.Text, want)
	}

	get, err := MemoryGetBlueprint{}.Materialize(ToolOpts{Recall: fake})
	if err != nil || get == nil {
		t.Fatalf("Materialize(get): tool=%v err=%v", get, err)
	}
	res, err = get.Execute(context.Background(), ToolInput(`{"path":"projects.rafiki","name":"dial-timeout"}`))
	if err != nil {
		t.Fatalf("get Execute: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(res.Text), &obj); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, res.Text)
	}
	if obj["id"] != "mem-1" || obj["body"] != "b" || obj["created_at"] != fixed.UTC().Format(time.RFC3339) {
		t.Errorf("memory JSON = %v, want id, body and an RFC3339 created_at", obj)
	}
	if _, ok := obj["owner_user_id"]; ok {
		t.Error("memory JSON must not carry owner_user_id: the binding is owner-scoped already")
	}

	tree, err := MemoryTreeBlueprint{}.Materialize(ToolOpts{Recall: fake})
	if err != nil || tree == nil {
		t.Fatalf("Materialize(tree): tool=%v err=%v", tree, err)
	}
	fake.treeOut = "projects.rafiki/dial-timeout"
	res, err = tree.Execute(context.Background(), ToolInput(`{"path":"projects.rafiki"}`))
	if err != nil {
		t.Fatalf("tree Execute: %v", err)
	}
	if res.Text != fake.treeOut || fake.treePath != "projects.rafiki" || fake.treeDepth != 0 {
		t.Errorf("tree forwarded = %q/%d, output = %q, want pass-through", fake.treePath, fake.treeDepth, res.Text)
	}

	del, err := MemoryDeleteBlueprint{}.Materialize(ToolOpts{Recall: fake})
	if err != nil || del == nil {
		t.Fatalf("Materialize(del): tool=%v err=%v", del, err)
	}
	res, err = del.Execute(context.Background(), ToolInput(`{"path":"a.b","name":"x"}`))
	if err != nil {
		t.Fatalf("delete Execute: %v", err)
	}
	if want := "deleted memory a.b/x"; res.Text != want {
		t.Errorf("delete result = %q, want %q", res.Text, want)
	}
	if fake.delPath != "a.b" || fake.delName != "x" {
		t.Errorf("delete forwarded = %q/%q, want path and name", fake.delPath, fake.delName)
	}
}

func TestMemoryToolTreeRequiresPath(t *testing.T) {
	tool, err := MemoryTreeBlueprint{}.Materialize(ToolOpts{Recall: &fakeRecallBinding{}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{"depth":2}`)); err == nil {
		t.Fatal("Execute without path returned no error")
	} else if want := "memory_tree: path is required"; err.Error() != want {
		t.Errorf("error = %q, want exactly %q", err.Error(), want)
	}
}

// before/after default to the window span (3) and max_chars to the context
// budget (8000) when omitted, and explicit values pass through.
func TestRecallContextDefaults(t *testing.T) {
	fake := &fakeRecallBinding{ctxOut: "...window text..."}
	tool, err := RecallContextBlueprint{}.Materialize(ToolOpts{Recall: fake})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}

	if _, err := tool.Execute(context.Background(), ToolInput(`{"id":"w:1"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.ctxHitID != "w:1" {
		t.Errorf("hit id = %q, want w:1", fake.ctxHitID)
	}
	if fake.ctxSpan != [2]int{recallContextDefaultSpan, recallContextDefaultSpan} {
		t.Errorf("span = %v, want before=3 after=3", fake.ctxSpan)
	}
	if fake.ctxMaxChars != recall.ContextDefaultMaxChars {
		t.Errorf("max_chars = %d, want %d", fake.ctxMaxChars, recall.ContextDefaultMaxChars)
	}

	if _, err := tool.Execute(context.Background(), ToolInput(
		`{"id":"s:9","before":1,"after":2,"max_chars":500}`)); err != nil {
		t.Fatalf("Execute(explicit): %v", err)
	}
	if fake.ctxSpan != [2]int{1, 2} || fake.ctxMaxChars != 500 {
		t.Errorf("explicit span/max_chars = %v/%d, want 1,2 and 500", fake.ctxSpan, fake.ctxMaxChars)
	}
}

func TestRecallContextRequiresID(t *testing.T) {
	tool, err := RecallContextBlueprint{}.Materialize(ToolOpts{Recall: &fakeRecallBinding{}})
	if err != nil || tool == nil {
		t.Fatalf("Materialize: tool=%v err=%v", tool, err)
	}
	if _, err := tool.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("Execute without id returned no error")
	} else if want := "recall_context: id is required"; err.Error() != want {
		t.Errorf("error = %q, want exactly %q", err.Error(), want)
	}
}

// All six recall/memory tools are daemon tier: they read the daemon's
// captured conversations and the caller's own memories and touch nothing in
// the workspace. Pinned here so a reclassification is a deliberate edit to
// executor_routing.go and this test, never a silent one.
func TestTierRecallToolsAreDaemon(t *testing.T) {
	for _, name := range []string{
		"recall", "recall_context", "memory_put", "memory_get", "memory_tree", "memory_delete",
	} {
		tier, ok := TierOf(name)
		if !ok {
			t.Errorf("%q has no tier — add it to tierByTool in executor_routing.go", name)
			continue
		}
		if tier != TierDaemon {
			t.Errorf("%q tier = %v, want TierDaemon", name, tier)
		}
	}
}

// The six tools materialize from nothing but the binding, so a registry built
// with a binding carries them and one built without it does not -- the same
// materialize/decline symmetry the decline test checks blueprint by blueprint,
// seen end to end through MaterializeAll.
func TestRecallToolsMaterializeAllOrNone(t *testing.T) {
	want := map[string]bool{
		"recall": true, "recall_context": true,
		"memory_put": true, "memory_get": true, "memory_tree": true, "memory_delete": true,
	}
	with := registryNames(DefaultBlueprint.MaterializeAll(ToolOpts{Recall: &fakeRecallBinding{}}))
	for name := range want {
		if !with[name] {
			t.Errorf("%q declined with a binding configured", name)
		}
	}
	without := registryNames(DefaultBlueprint.MaterializeAll(ToolOpts{}))
	for name := range want {
		if without[name] {
			t.Errorf("%q materialized with no binding", name)
		}
	}
}
