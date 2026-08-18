package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"
)

// discardConn is a no-op Connection used in dispatch tests where event
// delivery is not under test.
type discardConn struct{}

func (discardConn) Deliver(_ []byte)         {}
func (discardConn) Identity() users.Identity { return users.Identity{} }
func (discardConn) Restricted() bool         { return false }

// ─── fakeController ───────────────────────────────────────────────────────────

type fakeController struct {
	listFn                  func(protocol.ListFilter) []childstore.Snapshot
	getFn                   func(string) (childstore.Snapshot, bool)
	getRecentFn             func(string, control.RecentQuery) (control.RecentResult, error)
	searchFn                func(control.SearchQuery) control.SearchResult
	statusFn                func() control.ControllerStatus
	spawnFn                 func(context.Context, protocol.SpawnRequest) (control.SpawnResult, error)
	resumeFn                func(context.Context, string, string) (control.SpawnResult, error)
	killFn                  func(context.Context, string, int64, int64) (control.KillResult, error)
	forgetFn                func(string) error
	forgetAllExitedFn       func(int64) (int, error)
	sendFn                  func(string, json.RawMessage) error
	subscribeFn             func(string, control.Connection, protocol.SubscribeFilter) error
	unsubscribeFn           func(string, control.Connection) error
	globalSubscribeFn       func(control.Connection) error
	globalUnsubscribeFn     func(control.Connection) error
	subscribeLabeledFn      func(control.Connection, map[string]string, []string, protocol.SubscribeFilter) error
	onConnectionCloseFn     func(control.Connection)
	executorEnrollFn        func(protocol.ExecutorEnrollRequest) (protocol.ExecutorEnrollResponseData, error)
	executorCreateFn        func(protocol.ExecutorCreateRequest) (protocol.ExecutorCreateResponseData, error)
	executorListFn          func(protocol.ExecutorListRequest) ([]executors.Executor, error)
	executorLabelFn         func(protocol.ExecutorLabelRequest) (executors.Executor, error)
	executorDisableFn       func(protocol.ExecutorDisableRequest) error
	executorEnableFn        func(protocol.ExecutorEnableRequest) error
	listModelsFn            func(context.Context, string) ([]protocol.ModelInfo, error)
	listPresetsFn           func(map[string]string, []string) ([]protocol.PresetInfo, error)
	contextWindowFn         func(string) (int, int, bool)
	modelInfoFn             func(string) protocol.ModelInfoResponseData
	getStreamsResult        control.GetStreamsResult
	getStreamsErr           error
	conversationStatsFn     func(context.Context, insights.StatsFilter) (*insights.Stats, error)
	conversationStatsByIDFn func(context.Context, string) (*insights.Stats, error)
	conversationSearchFn    func(context.Context, insights.SearchFilter) ([]insights.ConversationSummary, error)
	conversationExportFn    func(context.Context, string) (*insights.Transcript, error)
	lastUserListLimit       int
}

func (f *fakeController) List(filter protocol.ListFilter) []childstore.Snapshot {
	if f.listFn != nil {
		return f.listFn(filter)
	}
	return nil
}

func (f *fakeController) Get(childID string) (childstore.Snapshot, bool) {
	if f.getFn != nil {
		return f.getFn(childID)
	}
	return childstore.Snapshot{}, false
}

func (f *fakeController) GetRecent(childID string, q control.RecentQuery) (control.RecentResult, error) {
	if f.getRecentFn != nil {
		return f.getRecentFn(childID, q)
	}
	return control.RecentResult{}, nil
}

func (f *fakeController) GetStreams(childID string, which string) (control.GetStreamsResult, error) {
	if f.getStreamsErr != nil {
		return control.GetStreamsResult{}, f.getStreamsErr
	}
	return f.getStreamsResult, nil
}

func (f *fakeController) Search(q control.SearchQuery) control.SearchResult {
	if f.searchFn != nil {
		return f.searchFn(q)
	}
	return control.SearchResult{}
}

func (f *fakeController) Status() control.ControllerStatus {
	if f.statusFn != nil {
		return f.statusFn()
	}
	return control.ControllerStatus{}
}

func (f *fakeController) ConversationStats(ctx context.Context, filter insights.StatsFilter) (*insights.Stats, error) {
	if f.conversationStatsFn != nil {
		return f.conversationStatsFn(ctx, filter)
	}
	return &insights.Stats{}, nil
}

func (f *fakeController) ConversationStatsByID(ctx context.Context, id string) (*insights.Stats, error) {
	if f.conversationStatsByIDFn != nil {
		return f.conversationStatsByIDFn(ctx, id)
	}
	return &insights.Stats{}, nil
}

func (f *fakeController) ConversationSearch(ctx context.Context, filter insights.SearchFilter) ([]insights.ConversationSummary, error) {
	if f.conversationSearchFn != nil {
		return f.conversationSearchFn(ctx, filter)
	}
	return nil, nil
}

func (f *fakeController) ConversationExport(ctx context.Context, id string) (*insights.Transcript, error) {
	if f.conversationExportFn != nil {
		return f.conversationExportFn(ctx, id)
	}
	return &insights.Transcript{}, nil
}

func (f *fakeController) TaskList(ctx context.Context, req protocol.TaskListRequest) ([]tasks.Task, error) {
	return nil, nil
}

func (f *fakeController) Spawn(ctx context.Context, req protocol.SpawnRequest) (control.SpawnResult, error) {
	if f.spawnFn != nil {
		return f.spawnFn(ctx, req)
	}
	return control.SpawnResult{}, nil
}

func (f *fakeController) Resume(ctx context.Context, childID string, apiKey string) (control.SpawnResult, error) {
	if f.resumeFn != nil {
		return f.resumeFn(ctx, childID, apiKey)
	}
	return control.SpawnResult{}, nil
}

func (f *fakeController) Kill(ctx context.Context, childID string, shutdownMs, killMs int64) (control.KillResult, error) {
	if f.killFn != nil {
		return f.killFn(ctx, childID, shutdownMs, killMs)
	}
	return control.KillResult{}, nil
}

func (f *fakeController) Forget(childID string) error {
	if f.forgetFn != nil {
		return f.forgetFn(childID)
	}
	return nil
}

func (f *fakeController) ForgetAllExited(olderThanMs int64) (int, error) {
	if f.forgetAllExitedFn != nil {
		return f.forgetAllExitedFn(olderThanMs)
	}
	return 0, nil
}

func (f *fakeController) Send(childID string, frame json.RawMessage) error {
	if f.sendFn != nil {
		return f.sendFn(childID, frame)
	}
	return nil
}

func (f *fakeController) Subscribe(childID string, conn control.Connection, filter protocol.SubscribeFilter) error {
	if f.subscribeFn != nil {
		return f.subscribeFn(childID, conn, filter)
	}
	return nil
}

func (f *fakeController) Unsubscribe(childID string, conn control.Connection) error {
	if f.unsubscribeFn != nil {
		return f.unsubscribeFn(childID, conn)
	}
	return nil
}

func (f *fakeController) GlobalSubscribe(conn control.Connection) error {
	if f.globalSubscribeFn != nil {
		return f.globalSubscribeFn(conn)
	}
	return nil
}

func (f *fakeController) GlobalUnsubscribe(conn control.Connection) error {
	if f.globalUnsubscribeFn != nil {
		return f.globalUnsubscribeFn(conn)
	}
	return nil
}

func (f *fakeController) SubscribeLabeled(conn control.Connection, labels map[string]string, hasLabel []string, filter protocol.SubscribeFilter) error {
	if f.subscribeLabeledFn != nil {
		return f.subscribeLabeledFn(conn, labels, hasLabel, filter)
	}
	return nil
}

func (f *fakeController) OnConnectionClose(conn control.Connection) {
	if f.onConnectionCloseFn != nil {
		f.onConnectionCloseFn(conn)
	}
}

func (f *fakeController) ExecutorEnroll(req protocol.ExecutorEnrollRequest) (protocol.ExecutorEnrollResponseData, error) {
	if f.executorEnrollFn != nil {
		return f.executorEnrollFn(req)
	}
	return protocol.ExecutorEnrollResponseData{}, nil
}

func (f *fakeController) ExecutorCreate(req protocol.ExecutorCreateRequest) (protocol.ExecutorCreateResponseData, error) {
	if f.executorCreateFn != nil {
		return f.executorCreateFn(req)
	}
	return protocol.ExecutorCreateResponseData{ExecutorID: "exec-1", Credential: "cred"}, nil
}

func (f *fakeController) ExecutorList(req protocol.ExecutorListRequest) ([]executors.Executor, error) {
	if f.executorListFn != nil {
		return f.executorListFn(req)
	}
	return nil, nil
}

func (f *fakeController) ExecutorLabel(req protocol.ExecutorLabelRequest) (executors.Executor, error) {
	if f.executorLabelFn != nil {
		return f.executorLabelFn(req)
	}
	return executors.Executor{}, nil
}

func (f *fakeController) ExecutorDisable(req protocol.ExecutorDisableRequest) error {
	if f.executorDisableFn != nil {
		return f.executorDisableFn(req)
	}
	return nil
}

func (f *fakeController) ExecutorEnable(req protocol.ExecutorEnableRequest) error {
	if f.executorEnableFn != nil {
		return f.executorEnableFn(req)
	}
	return nil
}

func (f *fakeController) UserCreate(ctx context.Context, username string) (protocol.UserCreateResponseData, error) {
	if username == "taken" {
		return protocol.UserCreateResponseData{}, users.ErrUsernameTaken
	}
	return protocol.UserCreateResponseData{
		ID: "11111111-1111-1111-1111-111111111111", Username: username, Token: "rfk_tok",
	}, nil
}

func (f *fakeController) UserList(ctx context.Context, includeDeleted bool, limit int) ([]users.User, error) {
	f.lastUserListLimit = limit
	return []users.User{{ID: "u1", Username: "brent"}}, nil
}

func (f *fakeController) UserRm(ctx context.Context, username string) error {
	if username == "ghost" {
		return users.ErrNotFound
	}
	return nil
}

func (f *fakeController) SetLabels(childID string, set map[string]string, remove []string) (map[string]string, error) {
	return nil, &control.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
}

func (f *fakeController) ListModels(ctx context.Context, provider string) ([]protocol.ModelInfo, error) {
	if f.listModelsFn != nil {
		return f.listModelsFn(ctx, provider)
	}
	return nil, nil
}

func (f *fakeController) ListPresets(labels map[string]string, hasLabel []string) ([]protocol.PresetInfo, error) {
	if f.listPresetsFn != nil {
		return f.listPresetsFn(labels, hasLabel)
	}
	return nil, nil
}

func (f *fakeController) ContextWindow(model string) (contextLen, maxCompletion int, ok bool) {
	if f.contextWindowFn != nil {
		return f.contextWindowFn(model)
	}
	return 0, 0, false
}

func (f *fakeController) ModelInfo(model string) protocol.ModelInfoResponseData {
	if f.modelInfoFn != nil {
		return f.modelInfoFn(model)
	}
	return protocol.ModelInfoResponseData{Model: model}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// parseResponse unmarshals a ctrl_response frame into the envelope.
func parseResponse(t *testing.T, raw []byte) protocol.Response {
	t.Helper()
	var r protocol.Response
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal response: %v\nframe: %s", err, raw)
	}
	return r
}

// mustSuccess asserts Success==true and returns the response.
func mustSuccess(t *testing.T, raw []byte) protocol.Response {
	t.Helper()
	r := parseResponse(t, raw)
	if !r.Success {
		t.Fatalf("expected success, got error code=%s msg=%s\nframe: %s",
			r.Error.Code, r.Error.Message, raw)
	}
	return r
}

// mustError asserts Success==false and the expected error code.
func mustError(t *testing.T, raw []byte, wantCode string) {
	t.Helper()
	r := parseResponse(t, raw)
	if r.Success {
		t.Fatalf("expected error code=%s, got success\nframe: %s", wantCode, raw)
	}
	if r.Error == nil || r.Error.Code != wantCode {
		var got string
		if r.Error != nil {
			got = r.Error.Code
		}
		t.Fatalf("expected error code=%s, got %s\nframe: %s", wantCode, got, raw)
	}
}

// controllerErr returns a *control.ControllerError for use in fakes.
func controllerErr(code, msg string) error {
	return &control.ControllerError{Code: code, Message: msg}
}

// makeSnapshot builds a minimal childstore.Snapshot for testing.
func makeSnapshot(childID string, status protocol.Status) childstore.Snapshot {
	return childstore.Snapshot{
		ChildID:      childID,
		PID:          42,
		Cwd:          "/work",
		Name:         "test-child",
		Provider:     "anthropic",
		Model:        "claude-sonnet-4",
		SessionID:    "sess-abc",
		SessionFile:  "/tmp/sess.jsonl",
		Status:       status,
		StartedAt:    time.Unix(1716000000, 0),
		LastActivity: time.Unix(1716000001, 0),
	}
}

// ─── Error-routing tests ──────────────────────────────────────────────────────

func TestDispatch_MalformedJSON(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{not valid json`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_UnknownType(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_nonexistent","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_list ────────────────────────────────────────────────────────────────

func TestDispatch_List_Success(t *testing.T) {
	snap := makeSnapshot("c_001", protocol.StatusIdle)
	c := &fakeController{
		listFn: func(f protocol.ListFilter) []childstore.Snapshot {
			return []childstore.Snapshot{snap}
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list","id":"1"}`)))
	if r.Command != protocol.TypeCtrlList || r.ID != "1" {
		t.Fatalf("command=%s id=%s", r.Command, r.ID)
	}

	var data protocol.ListResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(data.Children))
	}
	ch := data.Children[0]
	if ch.ChildID != "c_001" {
		t.Errorf("childId: got %s", ch.ChildID)
	}
	if ch.Model != "anthropic/claude-sonnet-4" {
		t.Errorf("model: got %s", ch.Model)
	}
	if ch.Status != "idle" {
		t.Errorf("status: got %s", ch.Status)
	}
	// PID must be non-nil for a live child.
	if ch.PID == nil {
		t.Error("expected non-nil PID for live child")
	}
}

func TestDispatch_List_WithFilter(t *testing.T) {
	var capturedFilter protocol.ListFilter
	c := &fakeController{
		listFn: func(f protocol.ListFilter) []childstore.Snapshot {
			capturedFilter = f
			return nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_list","id":"2","filter":{"status":"streaming","nameContains":"afk"}}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data protocol.ListResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	// Children slice must be non-nil array even when empty.
	if data.Children == nil {
		t.Error("expected non-nil children slice")
	}
	if capturedFilter.Status != "streaming" || capturedFilter.NameContains != "afk" {
		t.Errorf("filter not passed through: %+v", capturedFilter)
	}
}

func TestDispatch_List_ExitedChildHasNilPID(t *testing.T) {
	snap := makeSnapshot("c_exited", protocol.StatusExited)
	c := &fakeController{
		listFn: func(_ protocol.ListFilter) []childstore.Snapshot { return []childstore.Snapshot{snap} },
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list","id":"3"}`)))

	var data protocol.ListResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Children[0].PID != nil {
		t.Error("expected nil PID for exited child")
	}
}

// ─── ctrl_get ─────────────────────────────────────────────────────────────────

func TestDispatch_Get_Success(t *testing.T) {
	snap := makeSnapshot("c_001", protocol.StatusStreaming)
	c := &fakeController{
		getFn: func(id string) (childstore.Snapshot, bool) {
			if id == "c_001" {
				return snap, true
			}
			return childstore.Snapshot{}, false
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_get","id":"2","childId":"c_001"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if r.Command != protocol.TypeCtrlGet {
		t.Errorf("command: %s", r.Command)
	}

	var ch protocol.ChildSummary
	if err := json.Unmarshal(r.Data, &ch); err != nil {
		t.Fatal(err)
	}
	if ch.ChildID != "c_001" || ch.Status != "streaming" {
		t.Errorf("got %+v", ch)
	}
}

// TestDispatch_Get_ContextWindow asserts ctrl_get consults Controller.ContextWindow
// with the combined "provider/model" string and carries its answer on the
// wire — the daemon's own model catalog, not whatever static list an
// attaching client's TUI might carry locally.
func TestDispatch_Get_ContextWindow(t *testing.T) {
	snap := makeSnapshot("c_001", protocol.StatusStreaming)
	var gotModel string
	c := &fakeController{
		getFn: func(id string) (childstore.Snapshot, bool) { return snap, true },
		contextWindowFn: func(model string) (int, int, bool) {
			gotModel = model
			return 200000, 64000, true
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"2","childId":"c_001"}`)))

	var ch protocol.ChildSummary
	if err := json.Unmarshal(r.Data, &ch); err != nil {
		t.Fatal(err)
	}
	if gotModel != "anthropic/claude-sonnet-4" {
		t.Errorf("ContextWindow queried with %q, want the combined provider/model string", gotModel)
	}
	if ch.ContextWindow != 200000 || ch.MaxCompletionTokens != 64000 {
		t.Errorf("ContextWindow=%d MaxCompletionTokens=%d, want 200000/64000", ch.ContextWindow, ch.MaxCompletionTokens)
	}
}

// TestDispatch_Get_ContextWindowOmittedWhenUnresolved asserts a catalog miss
// (ok=false) — or no contextWindowFn hook at all, mirroring a daemon with no
// catalog configured — leaves ContextWindow/MaxCompletionTokens at their zero
// value rather than publishing a false 0 as if the catalog actually said so.
func TestDispatch_Get_ContextWindowOmittedWhenUnresolved(t *testing.T) {
	snap := makeSnapshot("c_001", protocol.StatusStreaming)
	c := &fakeController{getFn: func(id string) (childstore.Snapshot, bool) { return snap, true }}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"2","childId":"c_001"}`)))

	var ch protocol.ChildSummary
	if err := json.Unmarshal(r.Data, &ch); err != nil {
		t.Fatal(err)
	}
	if ch.ContextWindow != 0 || ch.MaxCompletionTokens != 0 {
		t.Errorf("ContextWindow=%d MaxCompletionTokens=%d, want both 0 (no contextWindowFn hooked)", ch.ContextWindow, ch.MaxCompletionTokens)
	}
}

// TestDispatch_List_ContextWindow asserts ctrl_list carries the same
// ContextWindow/MaxCompletionTokens enrichment as ctrl_get — snapshotToSummary
// is shared between the two handlers.
func TestDispatch_List_ContextWindow(t *testing.T) {
	snap := makeSnapshot("c_001", protocol.StatusStreaming)
	c := &fakeController{
		listFn:          func(protocol.ListFilter) []childstore.Snapshot { return []childstore.Snapshot{snap} },
		contextWindowFn: func(model string) (int, int, bool) { return 1000000, 32000, true },
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list","id":"1"}`)))

	var data protocol.ListResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Children) != 1 || data.Children[0].ContextWindow != 1000000 || data.Children[0].MaxCompletionTokens != 32000 {
		t.Errorf("got %+v", data.Children)
	}
}

// TestDispatch_Get_ClaudeChildMetadata asserts a claude child's ctrl_get
// response carries kind:"claude" plus the sessionId/model/cwd/name fields the
// attach layer's ChildMetadata reads (fetchChildMetadata maps data.name →
// sessionName). claude has no session file, so sessionFile is empty.
func TestDispatch_Get_ClaudeChildMetadata(t *testing.T) {
	snap := childstore.Snapshot{
		ChildID:      "c_claude",
		PID:          77,
		Cwd:          "/proj",
		Name:         "review-bot",
		Kind:         "claude",
		Provider:     "", // claude carries a bare model id, no provider prefix
		Model:        "claude-opus-4-8",
		SessionID:    "sess-claude-1",
		SessionFile:  "", // claude has no session file
		Status:       protocol.StatusIdle,
		StartedAt:    time.Unix(1716000000, 0),
		LastActivity: time.Unix(1716000001, 0),
	}
	c := &fakeController{
		getFn: func(id string) (childstore.Snapshot, bool) {
			if id == "c_claude" {
				return snap, true
			}
			return childstore.Snapshot{}, false
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_get","id":"9","childId":"c_claude"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	// Decode into a generic map so we can assert the exact wire keys the attach
	// ChildMetadata reads (kind, sessionId, model, cwd, name).
	var m map[string]any
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatal(err)
	}
	if m["kind"] != "claude" {
		t.Fatalf("ctrl_get must carry kind:claude, got %v", m["kind"])
	}
	if m["sessionId"] != "sess-claude-1" {
		t.Fatalf("sessionId = %v, want sess-claude-1", m["sessionId"])
	}
	if m["model"] != "claude-opus-4-8" {
		t.Fatalf("model = %v, want claude-opus-4-8 (no provider prefix)", m["model"])
	}
	if m["cwd"] != "/proj" {
		t.Fatalf("cwd = %v", m["cwd"])
	}
	if m["name"] != "review-bot" {
		t.Fatalf("name (sessionName) = %v, want review-bot", m["name"])
	}

	// Also assert the typed ChildSummary.Kind round-trips.
	var ch protocol.ChildSummary
	if err := json.Unmarshal(r.Data, &ch); err != nil {
		t.Fatal(err)
	}
	if ch.Kind != "claude" {
		t.Fatalf("ChildSummary.Kind = %q, want claude", ch.Kind)
	}
}

// TestDispatch_Get_PiChildOmitsKind asserts a pi child (empty Kind) omits the
// kind field on the wire (omitempty), so existing pi clients see no change.
func TestDispatch_Get_PiChildOmitsKind(t *testing.T) {
	snap := makeSnapshot("c_pi", protocol.StatusIdle) // Kind unset ("")
	c := &fakeController{
		getFn: func(id string) (childstore.Snapshot, bool) { return snap, true },
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"10","childId":"c_pi"}`)))
	var m map[string]any
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["kind"]; present {
		t.Fatalf("pi child must omit kind on the wire, got %v", m["kind"])
	}
}

func TestDispatchGetCarriesSlashCommands(t *testing.T) {
	snap := childstore.Snapshot{
		ChildID:       "c1",
		Status:        protocol.StatusIdle,
		SlashCommands: []string{"compact", "review"},
	}
	c := &fakeController{
		getFn: func(id string) (childstore.Snapshot, bool) {
			if id == "c1" {
				return snap, true
			}
			return childstore.Snapshot{}, false
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(nil, []byte(`{"type":"ctrl_get","id":"1","childId":"c1"}`))
	var r protocol.Response
	_ = json.Unmarshal(resp, &r)
	var cs protocol.ChildSummary
	_ = json.Unmarshal(r.Data, &cs)
	if len(cs.SlashCommands) != 2 || cs.SlashCommands[0] != "compact" {
		t.Fatalf("SlashCommands = %v, want [compact review]", cs.SlashCommands)
	}
}

func TestDispatch_Get_NotFound(t *testing.T) {
	c := &fakeController{
		getFn: func(string) (childstore.Snapshot, bool) { return childstore.Snapshot{}, false },
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Get_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_status ─────────────────────────────────────────────────────────────

func TestDispatch_Status_Success(t *testing.T) {
	c := &fakeController{
		statusFn: func() control.ControllerStatus {
			return control.ControllerStatus{
				Version:   "0.1.0",
				StartedAt: 1716000000,
				Children:  protocol.ChildCounts{Live: 2, Exited: 1},
			}
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_status","id":"15"}`)))
	if r.Command != protocol.TypeCtrlStatus || r.ID != "15" {
		t.Fatalf("command=%s id=%s", r.Command, r.ID)
	}

	var data protocol.StatusResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Version != "0.1.0" {
		t.Errorf("version: %s", data.Version)
	}
	if data.Children.Live != 2 || data.Children.Exited != 1 {
		t.Errorf("children: %+v", data.Children)
	}
}

// ─── ctrl_get_recent ─────────────────────────────────────────────────────────

func TestDispatch_GetRecent_Success(t *testing.T) {
	event := json.RawMessage(`{"type":"turn_end"}`)
	c := &fakeController{
		getRecentFn: func(childID string, q control.RecentQuery) (control.RecentResult, error) {
			if childID != "c_001" {
				return control.RecentResult{}, errors.New("wrong child")
			}
			return control.RecentResult{
				Events:           []json.RawMessage{event},
				TotalInBuffer:    1,
				OldestTimestamp:  1716000000,
				TruncatedByLimit: false,
			}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_get_recent","id":"10","childId":"c_001","limit":100}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data protocol.GetRecentResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(data.Events))
	}
	if data.TotalInBuffer != 1 {
		t.Errorf("totalInBuffer: %d", data.TotalInBuffer)
	}
}

func TestDispatch_GetRecent_EmptyEventsIsArray(t *testing.T) {
	c := &fakeController{
		getRecentFn: func(string, control.RecentQuery) (control.RecentResult, error) {
			return control.RecentResult{}, nil // Events is nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get_recent","id":"10","childId":"c_001"}`)))

	// "events" must be [] not null.
	if !json.Valid(r.Data) {
		t.Fatal("invalid data JSON")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["events"]) == "null" {
		t.Error("events should be [] not null")
	}
}

func TestDispatchGetRecentRenderedFlag(t *testing.T) {
	var got control.RecentQuery
	c := &fakeController{
		getRecentFn: func(_ string, q control.RecentQuery) (control.RecentResult, error) {
			got = q
			return control.RecentResult{}, nil
		},
	}
	d := control.NewDispatch(c)
	d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get_recent","id":"1","childId":"c1","rendered":true}`))
	if !got.Rendered {
		t.Fatal("Rendered flag did not propagate to RecentQuery")
	}
}

func TestDispatch_GetRecent_NotFound(t *testing.T) {
	c := &fakeController{
		getRecentFn: func(string, control.RecentQuery) (control.RecentResult, error) {
			return control.RecentResult{}, controllerErr(protocol.ErrChildNotFound, "child not found")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get_recent","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_GetRecent_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get_recent","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_search ─────────────────────────────────────────────────────────────

func TestDispatch_Search_Success(t *testing.T) {
	c := &fakeController{
		searchFn: func(q control.SearchQuery) control.SearchResult {
			if q.Query != "ublk_register" {
				return control.SearchResult{}
			}
			return control.SearchResult{
				Hits: []protocol.SearchHit{
					{ChildID: "c_001", Snippet: "calling ublk_register", MatchStart: 8, MatchEnd: 21},
				},
				TotalHits: 1,
				Scanned:   3,
				Elapsed:   42,
			}
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_search","id":"14","query":"ublk_register","limit":50}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data protocol.SearchResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(data.Hits))
	}
	if data.Hits[0].Snippet != "calling ublk_register" {
		t.Errorf("snippet: %s", data.Hits[0].Snippet)
	}
	if data.Scanned != 3 || data.Elapsed != 42 {
		t.Errorf("scanned=%d elapsed=%d", data.Scanned, data.Elapsed)
	}
}

func TestDispatch_Search_MissingQuery(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_search","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Search_EmptyHitsIsArray(t *testing.T) {
	c := &fakeController{
		searchFn: func(control.SearchQuery) control.SearchResult {
			return control.SearchResult{} // Hits is nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_search","id":"x","query":"needle"}`)))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["hits"]) == "null" {
		t.Error("hits should be [] not null")
	}
}

// ─── ctrl_conversation_stats ───────────────────────────────────────────────────

func TestDispatch_ConversationStats_Global_Success(t *testing.T) {
	c := &fakeController{
		conversationStatsFn: func(_ context.Context, f insights.StatsFilter) (*insights.Stats, error) {
			if f.Owner != "brent" {
				t.Fatalf("owner: %s", f.Owner)
			}
			return &insights.Stats{Volume: insights.VolumeStats{Conversations: 5, Turns: 20}}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_stats","id":"30","owner":"brent"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data insights.Stats
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Volume.Conversations != 5 || data.Volume.Turns != 20 {
		t.Errorf("volume: %+v", data.Volume)
	}
}

func TestDispatch_ConversationStats_Global_TimeAndPathConversion(t *testing.T) {
	c := &fakeController{
		conversationStatsFn: func(_ context.Context, f insights.StatsFilter) (*insights.Stats, error) {
			if f.Since == nil || f.Since.Unix() != 1716000000 {
				t.Fatalf("since: %v", f.Since)
			}
			if f.Until == nil || f.Until.Unix() != 1716100000 {
				t.Fatalf("until: %v", f.Until)
			}
			if f.Path != insights.PathDirect {
				t.Fatalf("path: %q", f.Path)
			}
			return &insights.Stats{}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_stats","id":"30b","sinceUnix":1716000000,"untilUnix":1716100000,"path":"direct"}`
	mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
}

func TestDispatch_ConversationStats_ByID_Success(t *testing.T) {
	c := &fakeController{
		conversationStatsByIDFn: func(_ context.Context, id string) (*insights.Stats, error) {
			if id != "conv-abc" {
				t.Fatalf("id: %s", id)
			}
			return &insights.Stats{Volume: insights.VolumeStats{Conversations: 1}}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_stats","id":"31","conversationId":"conv-abc"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data insights.Stats
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Volume.Conversations != 1 {
		t.Errorf("volume: %+v", data.Volume)
	}
}

func TestDispatch_ConversationStats_NoAgentDB(t *testing.T) {
	c := &fakeController{
		conversationStatsFn: func(context.Context, insights.StatsFilter) (*insights.Stats, error) {
			return nil, controllerErr(protocol.ErrNoAgentDB, "no agent database configured")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_conversation_stats","id":"32"}`))
	mustError(t, resp, protocol.ErrNoAgentDB)
}

// ─── ctrl_conversation_search ──────────────────────────────────────────────────

func TestDispatch_ConversationSearch_Success(t *testing.T) {
	c := &fakeController{
		conversationSearchFn: func(_ context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error) {
			if f.Text != "skill gap" {
				t.Fatalf("text: %s", f.Text)
			}
			return []insights.ConversationSummary{{ID: "conv-abc", Owner: "brent"}}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_search","id":"33","text":"skill gap"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data struct {
		Rows []insights.ConversationSummary `json:"rows"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Rows) != 1 || data.Rows[0].ID != "conv-abc" {
		t.Errorf("rows: %+v", data.Rows)
	}
}

func TestDispatch_ConversationSearch_EmptyRowsIsArray(t *testing.T) {
	c := &fakeController{
		conversationSearchFn: func(context.Context, insights.SearchFilter) ([]insights.ConversationSummary, error) {
			return nil, nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_conversation_search","id":"34"}`)))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["rows"]) == "null" {
		t.Error("rows should be [] not null")
	}
}

// ─── ctrl_conversation_export ──────────────────────────────────────────────────

func TestDispatch_ConversationExport_Success(t *testing.T) {
	c := &fakeController{
		conversationExportFn: func(_ context.Context, id string) (*insights.Transcript, error) {
			if id != "conv-abc" {
				t.Fatalf("id: %s", id)
			}
			return &insights.Transcript{ConversationID: "conv-abc", Owner: "brent"}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_export","id":"35","conversationId":"conv-abc"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data insights.Transcript
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.ConversationID != "conv-abc" {
		t.Errorf("conversationId: %s", data.ConversationID)
	}
}

func TestDispatch_ConversationExport_MissingConversationID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_conversation_export","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_spawn ───────────────────────────────────────────────────────────────

func TestDispatch_Spawn_Success(t *testing.T) {
	c := &fakeController{
		spawnFn: func(_ context.Context, req protocol.SpawnRequest) (control.SpawnResult, error) {
			if req.Cwd != "/work" {
				return control.SpawnResult{}, errors.New("unexpected cwd")
			}
			return control.SpawnResult{
				ChildID:     "c_new",
				SessionID:   "sess-new",
				SessionFile: "/tmp/new.jsonl",
				Model:       "anthropic/claude-sonnet-4",
				Stalled:     false,
			}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_spawn","id":"req-42","cwd":"/work","name":"afk-impl"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if r.ID != "req-42" {
		t.Errorf("id: %s", r.ID)
	}

	var data protocol.SpawnResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.ChildID != "c_new" {
		t.Errorf("childId: %s", data.ChildID)
	}
	if data.Stalled {
		t.Error("expected stalled=false")
	}
}

func TestDispatch_Spawn_MissingCwd(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_spawn","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Spawn_RelativeCwd(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_spawn","id":"x","cwd":"relative/path"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Spawn_Failure(t *testing.T) {
	c := &fakeController{
		spawnFn: func(_ context.Context, _ protocol.SpawnRequest) (control.SpawnResult, error) {
			return control.SpawnResult{}, controllerErr(protocol.ErrSpawnFailed, "pi binary not found")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_spawn","id":"x","cwd":"/work"}`))
	mustError(t, resp, protocol.ErrSpawnFailed)
}

// ─── ctrl_resume ─────────────────────────────────────────────────────────────

func TestDispatch_Resume_Success(t *testing.T) {
	c := &fakeController{
		resumeFn: func(_ context.Context, childID, apiKey string) (control.SpawnResult, error) {
			if childID != "c_001" {
				return control.SpawnResult{}, controllerErr(protocol.ErrNotFound, "not found")
			}
			return control.SpawnResult{ChildID: "c_001", SessionID: "sess-001"}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_resume","id":"r1","childId":"c_001"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data protocol.SpawnResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.ChildID != "c_001" {
		t.Errorf("childId: %s", data.ChildID)
	}
}

func TestDispatch_Resume_NotFound(t *testing.T) {
	c := &fakeController{
		resumeFn: func(_ context.Context, _ string, _ string) (control.SpawnResult, error) {
			return control.SpawnResult{}, controllerErr(protocol.ErrNotFound, "child not found")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_resume","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrNotFound)
}

func TestDispatch_Resume_NotResumable(t *testing.T) {
	c := &fakeController{
		resumeFn: func(_ context.Context, _ string, _ string) (control.SpawnResult, error) {
			return control.SpawnResult{}, controllerErr(protocol.ErrNotResumable, "child is not exited")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_resume","id":"x","childId":"c_live"}`))
	mustError(t, resp, protocol.ErrNotResumable)
}

func TestDispatch_Resume_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_resume","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_kill ────────────────────────────────────────────────────────────────

func TestDispatch_Kill_Success(t *testing.T) {
	exitCode := 0
	c := &fakeController{
		killFn: func(_ context.Context, childID string, shutdownMs, killMs int64) (control.KillResult, error) {
			if childID != "c_001" {
				return control.KillResult{}, errors.New("wrong child")
			}
			if shutdownMs != 180000 || killMs != 30000 {
				return control.KillResult{}, errors.New("wrong timeouts")
			}
			return control.KillResult{
				ExitCode:   &exitCode,
				DurationMs: 1247,
				Escalated:  false,
			}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_kill","id":"5","childId":"c_001","shutdownTimeoutMs":180000,"killTimeoutMs":30000}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data protocol.KillResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.ExitCode == nil || *data.ExitCode != 0 {
		t.Errorf("exitCode: %v", data.ExitCode)
	}
	if data.DurationMs != 1247 {
		t.Errorf("durationMs: %d", data.DurationMs)
	}
	if data.Escalated {
		t.Error("expected escalated=false")
	}
}

func TestDispatch_Kill_NotFound(t *testing.T) {
	c := &fakeController{
		killFn: func(_ context.Context, _ string, _, _ int64) (control.KillResult, error) {
			return control.KillResult{}, controllerErr(protocol.ErrChildNotFound, "child not found")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_kill","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Kill_AlreadyExited(t *testing.T) {
	c := &fakeController{
		killFn: func(_ context.Context, _ string, _, _ int64) (control.KillResult, error) {
			return control.KillResult{}, controllerErr(protocol.ErrChildExited, "child already exited")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_kill","id":"x","childId":"c_dead"}`))
	mustError(t, resp, protocol.ErrChildExited)
}

func TestDispatch_Kill_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_kill","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_forget ─────────────────────────────────────────────────────────────

func TestDispatch_Forget_Success(t *testing.T) {
	var forgotten string
	c := &fakeController{
		forgetFn: func(childID string) error {
			forgotten = childID
			return nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_forget","id":"12","childId":"c_001"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if r.Command != protocol.TypeCtrlForget {
		t.Errorf("command: %s", r.Command)
	}
	if forgotten != "c_001" {
		t.Errorf("forgotten: %s", forgotten)
	}
	// Success response has no data field.
	if len(r.Data) > 0 {
		t.Errorf("expected no data, got %s", r.Data)
	}
}

func TestDispatch_Forget_NotFound(t *testing.T) {
	c := &fakeController{
		forgetFn: func(string) error {
			return controllerErr(protocol.ErrNotFound, "not found")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_forget","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrNotFound)
}

func TestDispatch_Forget_NotExited(t *testing.T) {
	c := &fakeController{
		forgetFn: func(string) error {
			return controllerErr(protocol.ErrNotExited, "child is still running")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_forget","id":"x","childId":"c_live"}`))
	mustError(t, resp, protocol.ErrNotExited)
}

func TestDispatch_Forget_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_forget","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_forget_all_exited ───────────────────────────────────────────────────

func TestDispatch_ForgetAllExited_Success(t *testing.T) {
	c := &fakeController{
		forgetAllExitedFn: func(olderThanMs int64) (int, error) {
			if olderThanMs != 3600000 {
				return 0, errors.New("wrong age filter")
			}
			return 5, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_forget_all_exited","id":"13","olderThanMs":3600000}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data protocol.ForgetAllExitedResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Count != 5 {
		t.Errorf("count: %d", data.Count)
	}
}

func TestDispatch_ForgetAllExited_ZeroAge(t *testing.T) {
	// olderThanMs=0 means forget all exited.
	var captured int64 = -1
	c := &fakeController{
		forgetAllExitedFn: func(olderThanMs int64) (int, error) {
			captured = olderThanMs
			return 0, nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_forget_all_exited","id":"x"}`)))
	if captured != 0 {
		t.Errorf("olderThanMs should be 0, got %d", captured)
	}
	var data protocol.ForgetAllExitedResponseData
	_ = json.Unmarshal(r.Data, &data)
	if data.Count != 0 {
		t.Errorf("count: %d", data.Count)
	}
}

// ─── ctrl_send ────────────────────────────────────────────────────────────────

func TestDispatch_Send_Success(t *testing.T) {
	var sentFrame json.RawMessage
	c := &fakeController{
		sendFn: func(childID string, frame json.RawMessage) error {
			if childID != "c_001" {
				return errors.New("wrong child")
			}
			sentFrame = frame
			return nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_send","id":"11","childId":"c_001","frame":{"type":"prompt","message":"hello"}}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if r.Command != protocol.TypeCtrlSend {
		t.Errorf("command: %s", r.Command)
	}
	// No data in send response.
	if len(r.Data) > 0 {
		t.Errorf("expected no data, got %s", r.Data)
	}
	// Verify the inner frame was extracted correctly.
	var inner map[string]any
	if err := json.Unmarshal(sentFrame, &inner); err != nil {
		t.Fatalf("sent frame invalid JSON: %v", err)
	}
	if inner["type"] != "prompt" {
		t.Errorf("inner frame type: %v", inner["type"])
	}
}

func TestDispatch_Send_ChildNotFound(t *testing.T) {
	c := &fakeController{
		sendFn: func(string, json.RawMessage) error {
			return controllerErr(protocol.ErrChildNotFound, "child not found")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_missing","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Send_Backpressure(t *testing.T) {
	c := &fakeController{
		sendFn: func(string, json.RawMessage) error {
			return controllerErr(protocol.ErrBackpressure, "channel full")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_001","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrBackpressure)
}

func TestDispatch_Send_ShuttingDown(t *testing.T) {
	c := &fakeController{
		sendFn: func(string, json.RawMessage) error {
			return controllerErr(protocol.ErrChildShuttingDown, "child is shutting down")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_001","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrChildShuttingDown)
}

func TestDispatch_Send_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Send_MissingFrame(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_001"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_subscribe ───────────────────────────────────────────────────────────

func TestDispatch_Subscribe_Success(t *testing.T) {
	var capturedChildID string
	var capturedFilter protocol.SubscribeFilter
	c := &fakeController{
		subscribeFn: func(childID string, _ control.Connection, filter protocol.SubscribeFilter) error {
			capturedChildID = childID
			capturedFilter = filter
			return nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_subscribe","id":"6","childId":"c_001","filter":{"profile":"coarse"}}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if r.Command != protocol.TypeCtrlSubscribe {
		t.Errorf("command: %s", r.Command)
	}
	if capturedChildID != "c_001" {
		t.Errorf("childID: %s", capturedChildID)
	}
	if capturedFilter.Profile != "coarse" {
		t.Errorf("filter.profile: %s", capturedFilter.Profile)
	}
}

func TestDispatch_Subscribe_NotFound(t *testing.T) {
	c := &fakeController{
		subscribeFn: func(string, control.Connection, protocol.SubscribeFilter) error {
			return controllerErr(protocol.ErrChildNotFound, "child not found")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_subscribe","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Subscribe_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_subscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Subscribe_LabelFiltered_Success(t *testing.T) {
	var gotLabels map[string]string
	var gotHasLabel []string
	var gotFilter protocol.SubscribeFilter
	c := &fakeController{
		subscribeLabeledFn: func(_ control.Connection, labels map[string]string, hasLabel []string, filter protocol.SubscribeFilter) error {
			gotLabels = labels
			gotHasLabel = hasLabel
			gotFilter = filter
			return nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_subscribe","id":"lf-1","labels":{"context":"work"},"hasLabel":["team"]}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if r.Command != protocol.TypeCtrlSubscribe {
		t.Errorf("command: %s", r.Command)
	}
	if gotLabels["context"] != "work" {
		t.Errorf("labels: %v", gotLabels)
	}
	if len(gotHasLabel) != 1 || gotHasLabel[0] != "team" {
		t.Errorf("hasLabel: %v", gotHasLabel)
	}
	_ = gotFilter
}

func TestDispatch_Subscribe_LabelFiltered_WithFilter(t *testing.T) {
	var gotFilter protocol.SubscribeFilter
	c := &fakeController{
		subscribeLabeledFn: func(_ control.Connection, _ map[string]string, _ []string, filter protocol.SubscribeFilter) error {
			gotFilter = filter
			return nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_subscribe","id":"lf-2","labels":{"env":"prod"},"filter":{"profile":"coarse"}}`
	mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if gotFilter.Profile != "coarse" {
		t.Errorf("filter.profile: %s", gotFilter.Profile)
	}
}

func TestDispatch_Subscribe_ChildIDAndLabels_MutuallyExclusive(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	frame := `{"type":"ctrl_subscribe","id":"lf-3","childId":"c_001","labels":{"env":"prod"}}`
	resp := d.HandleFrame(discardConn{}, []byte(frame))
	mustError(t, resp, protocol.ErrInvalidArgs)
	// Verify the error message is specific.
	r := parseResponse(t, resp)
	if r.Error.Message != "subscribe: childId and labels are mutually exclusive" {
		t.Errorf("unexpected message: %s", r.Error.Message)
	}
}

func TestDispatch_Subscribe_HasLabelOnly(t *testing.T) {
	called := false
	c := &fakeController{
		subscribeLabeledFn: func(_ control.Connection, _ map[string]string, hasLabel []string, _ protocol.SubscribeFilter) error {
			called = true
			if len(hasLabel) != 1 || hasLabel[0] != "tier" {
				return &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: "wrong hasLabel"}
			}
			return nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_subscribe","id":"lf-4","hasLabel":["tier"]}`
	mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if !called {
		t.Error("SubscribeLabeled was not called")
	}
}

// ─── ctrl_unsubscribe ────────────────────────────────────────────────────────

func TestDispatch_Unsubscribe_Success(t *testing.T) {
	var capturedChildID string
	c := &fakeController{
		unsubscribeFn: func(childID string, _ control.Connection) error {
			capturedChildID = childID
			return nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_unsubscribe","id":"7","childId":"c_001"}`)))
	if r.Command != protocol.TypeCtrlUnsubscribe {
		t.Errorf("command: %s", r.Command)
	}
	if capturedChildID != "c_001" {
		t.Errorf("childID: %s", capturedChildID)
	}
}

func TestDispatch_Unsubscribe_MissingChildID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_unsubscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_global_subscribe / ctrl_global_unsubscribe ─────────────────────────

func TestDispatch_GlobalSubscribe_Success(t *testing.T) {
	called := false
	c := &fakeController{
		globalSubscribeFn: func(control.Connection) error {
			called = true
			return nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_global_subscribe","id":"8"}`)))
	if r.Command != protocol.TypeCtrlGlobalSubscribe {
		t.Errorf("command: %s", r.Command)
	}
	if !called {
		t.Error("GlobalSubscribe was not called")
	}
}

func TestDispatch_GlobalUnsubscribe_Success(t *testing.T) {
	called := false
	c := &fakeController{
		globalUnsubscribeFn: func(control.Connection) error {
			called = true
			return nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_global_unsubscribe","id":"9"}`)))
	if r.Command != protocol.TypeCtrlGlobalUnsubscribe {
		t.Errorf("command: %s", r.Command)
	}
	if !called {
		t.Error("GlobalUnsubscribe was not called")
	}
}

func TestDispatch_GlobalSubscribe_Error(t *testing.T) {
	c := &fakeController{
		globalSubscribeFn: func(control.Connection) error {
			return controllerErr(protocol.ErrInternal, "boom")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_global_subscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInternal)
}

func TestDispatch_GlobalUnsubscribe_Error(t *testing.T) {
	c := &fakeController{
		globalUnsubscribeFn: func(control.Connection) error {
			return controllerErr(protocol.ErrInternal, "boom")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_global_unsubscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInternal)
}

// ─── ID propagation ───────────────────────────────────────────────────────────

func TestDispatch_IDPropagatedInErrorResponse(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"req-99","childId":"c_missing"}`))
	r := parseResponse(t, resp)
	if r.ID != "req-99" {
		t.Errorf("id not propagated in error response: got %s", r.ID)
	}
}

func TestDispatch_IDPropagatedInSuccessResponse(t *testing.T) {
	c := &fakeController{
		statusFn: func() control.ControllerStatus { return control.ControllerStatus{} },
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_status","id":"abc-123"}`)))
	if r.ID != "abc-123" {
		t.Errorf("id not propagated in success response: got %s", r.ID)
	}
}

// ─── Concurrent safety ────────────────────────────────────────────────────────

func TestDispatch_ConcurrentSafe(t *testing.T) {
	c := &fakeController{
		statusFn: func() control.ControllerStatus { return control.ControllerStatus{Version: "0.1.0"} },
		listFn:   func(protocol.ListFilter) []childstore.Snapshot { return nil },
	}
	d := control.NewDispatch(c)
	frames := [][]byte{
		[]byte(`{"type":"ctrl_status","id":"1"}`),
		[]byte(`{"type":"ctrl_list","id":"2"}`),
		[]byte(`{"type":"ctrl_status","id":"3"}`),
	}
	done := make(chan struct{})
	for _, f := range frames {
		f := f
		go func() {
			defer func() { done <- struct{}{} }()
			for range 100 {
				resp := d.HandleFrame(discardConn{}, f)
				if resp == nil {
					t.Error("nil response")
				}
			}
		}()
	}
	for range frames {
		<-done
	}
}

// ─── ctrl_list_models ─────────────────────────────────────────────────────────

func TestDispatch_ListModels_Success(t *testing.T) {
	c := &fakeController{
		listModelsFn: func(_ context.Context, provider string) ([]protocol.ModelInfo, error) {
			all := []protocol.ModelInfo{
				{ID: "anthropic/claude-sonnet-4-5", Provider: "anthropic", Model: "claude-sonnet-4-5", Source: "builtin"},
				{ID: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o", Source: "builtin"},
			}
			if provider == "" {
				return all, nil
			}
			var out []protocol.ModelInfo
			for _, m := range all {
				if m.Provider == provider {
					out = append(out, m)
				}
			}
			return out, nil
		},
	}
	d := control.NewDispatch(c)

	// No filter — all models returned.
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list_models","id":"m1"}`)))
	if r.Command != protocol.TypeCtrlListModels || r.ID != "m1" {
		t.Fatalf("command=%s id=%s", r.Command, r.ID)
	}
	var data protocol.ListModelsResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(data.Models))
	}

	// Provider filter — only openai.
	r = mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list_models","id":"m2","provider":"openai"}`)))
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Models) != 1 || data.Models[0].Provider != "openai" {
		t.Errorf("provider filter: got %+v", data.Models)
	}
}

func TestDispatch_ListModels_EmptyIsArray(t *testing.T) {
	c := &fakeController{
		listModelsFn: func(context.Context, string) ([]protocol.ModelInfo, error) {
			return nil, nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list_models","id":"m3"}`)))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["models"]) == "null" {
		t.Error("models should be [] not null")
	}
}

func TestDispatch_ListModels_MalformedRequest(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{not valid json`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_list_presets ────────────────────────────────────────────────────────

func TestDispatch_ListPresets_Success(t *testing.T) {
	c := &fakeController{
		listPresetsFn: func(labels map[string]string, hasLabel []string) ([]protocol.PresetInfo, error) {
			all := []protocol.PresetInfo{
				{Name: "work", Model: "anthropic/claude-sonnet-4-5", Labels: map[string]string{"context": "work"}},
				{Name: "cheap", Model: "ollama/llama3.1:8b", Labels: map[string]string{"context": "cheap"}},
			}
			// Apply label filter.
			if len(labels) == 0 && len(hasLabel) == 0 {
				return all, nil
			}
			var out []protocol.PresetInfo
			for _, p := range all {
				ok := true
				for k, v := range labels {
					if p.Labels[k] != v {
						ok = false
						break
					}
				}
				if ok {
					out = append(out, p)
				}
			}
			return out, nil
		},
	}
	d := control.NewDispatch(c)

	// No filter — all presets.
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list_presets","id":"p1"}`)))
	if r.Command != protocol.TypeCtrlListPresets || r.ID != "p1" {
		t.Fatalf("command=%s id=%s", r.Command, r.ID)
	}
	var data protocol.ListPresetsResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Presets) != 2 {
		t.Fatalf("expected 2 presets, got %d", len(data.Presets))
	}

	// Label filter — only "work" context.
	frame := `{"type":"ctrl_list_presets","id":"p2","labels":{"context":"work"}}`
	r = mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Presets) != 1 || data.Presets[0].Name != "work" {
		t.Errorf("label filter: got %+v", data.Presets)
	}
}

func TestDispatch_ListPresets_EmptyIsArray(t *testing.T) {
	c := &fakeController{
		listPresetsFn: func(map[string]string, []string) ([]protocol.PresetInfo, error) {
			return nil, nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_list_presets","id":"p3"}`)))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["presets"]) == "null" {
		t.Error("presets should be [] not null")
	}
}

func TestDispatch_ListPresets_HasLabelFilter(t *testing.T) {
	var capturedHasLabel []string
	c := &fakeController{
		listPresetsFn: func(_ map[string]string, hasLabel []string) ([]protocol.PresetInfo, error) {
			capturedHasLabel = hasLabel
			return []protocol.PresetInfo{{Name: "work"}}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_list_presets","id":"p4","hasLabel":["team"]}`
	mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if len(capturedHasLabel) != 1 || capturedHasLabel[0] != "team" {
		t.Errorf("hasLabel not passed through: %v", capturedHasLabel)
	}
}

// ─── ctrl_model_info ──────────────────────────────────────────────────────

// TestDispatch_ModelInfo_BarePayload pins the response shape: the value is the
// bare ModelInfoResponseData, not wrapped like ctrl_conversation_search's
// {"rows":[...]}. The client decodes into protocol.ModelInfoResponseData, so a
// shape mismatch here is a runtime unmarshal error a client-side unit test
// (whose fixture encodes the same assumption) would not catch.
func TestDispatch_ModelInfo_BarePayload(t *testing.T) {
	c := &fakeController{
		modelInfoFn: func(model string) protocol.ModelInfoResponseData {
			return protocol.ModelInfoResponseData{
				Model:               model,
				ResolvedID:          "anthropic/claude-opus-5",
				ContextWindow:       200000,
				MaxCompletionTokens: 8192,
				AutoCompactWindow:   190000,
				Known:               true,
			}
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_model_info","id":"mi1","model":"anthropic/claude-opus-5"}`)))
	if r.Command != protocol.TypeCtrlModelInfo {
		t.Fatalf("command = %s", r.Command)
	}
	var data protocol.ModelInfoResponseData
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("decode bare ModelInfoResponseData: %v", err)
	}
	if data.Known != true || data.AutoCompactWindow != 190000 || data.ContextWindow != 200000 {
		t.Fatalf("unexpected data: %+v", data)
	}
}

func TestDispatch_ModelInfo_MissingModelIsInvalidArgs(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	mustError(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_model_info","id":"mi2"}`)), protocol.ErrInvalidArgs)
}

func TestDispatchGetStreams(t *testing.T) {
	// Success/all round-trip: the fake echoes whatever result it is given, so
	// this exercises dispatch wiring and payload serialization end to end.
	t.Run("success_all", func(t *testing.T) {
		fc := &fakeController{getStreamsResult: control.GetStreamsResult{
			Alive: true,
			In:    [][]byte{[]byte(`{"type":"user_input"}`)},
			Err:   []byte("boom\n"),
		}}
		d := control.NewDispatch(fc)
		req := []byte(`{"type":"ctrl_get_streams","id":"x1","childId":"child-1","which":"all"}`)
		got := mustSuccess(t, d.HandleFrame(nil, req))
		var data protocol.GetStreamsResponseData
		if err := json.Unmarshal(got.Data, &data); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if !data.Alive || len(data.In) != 1 || string(data.Err) != "boom\n" {
			t.Fatalf("unexpected data: %+v", data)
		}
	})

	t.Run("alive_false_round_trip", func(t *testing.T) {
		fc := &fakeController{getStreamsResult: control.GetStreamsResult{Alive: false}}
		d := control.NewDispatch(fc)
		req := []byte(`{"type":"ctrl_get_streams","id":"x2","childId":"child-1"}`)
		got := mustSuccess(t, d.HandleFrame(nil, req))
		var data protocol.GetStreamsResponseData
		if err := json.Unmarshal(got.Data, &data); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if data.Alive {
			t.Fatalf("expected alive=false, got %+v", data)
		}
	})

	t.Run("missing_childId", func(t *testing.T) {
		d := control.NewDispatch(&fakeController{})
		req := []byte(`{"type":"ctrl_get_streams","id":"x3","which":"all"}`)
		mustError(t, d.HandleFrame(nil, req), protocol.ErrInvalidArgs)
	})

	t.Run("invalid_which", func(t *testing.T) {
		d := control.NewDispatch(&fakeController{})
		req := []byte(`{"type":"ctrl_get_streams","id":"x4","childId":"child-1","which":"bogus"}`)
		mustError(t, d.HandleFrame(nil, req), protocol.ErrInvalidArgs)
	})

	t.Run("controller_error_maps_code", func(t *testing.T) {
		fc := &fakeController{getStreamsErr: &control.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: child-1",
		}}
		d := control.NewDispatch(fc)
		req := []byte(`{"type":"ctrl_get_streams","id":"x5","childId":"child-1"}`)
		mustError(t, d.HandleFrame(nil, req), protocol.ErrChildNotFound)
	})
}

// ─── ctrl_user_* ────────────────────────────────────────────────────────────

func TestUserCreateReturnsThePlaintextTokenOnce(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	req := []byte(`{"type":"ctrl_user_create","id":"1","username":"brent"}`)
	got := mustSuccess(t, d.HandleFrame(nil, req))
	var data protocol.UserCreateResponseData
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data.Token != "rfk_tok" || data.Username != "brent" {
		t.Fatalf("data = %+v", data)
	}
}

func TestUserCreateMapsDuplicateNameToAnInvalidArgsError(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	req := []byte(`{"type":"ctrl_user_create","id":"1","username":"taken"}`)
	mustError(t, d.HandleFrame(nil, req), protocol.ErrInvalidArgs)
}

// The response is capped at MaxFrameBytes, so an unbounded limit is a
// protocol break, not just a slow query. 500 mirrors the unexported
// maxUserListLimit in dispatch.go, which this external test package cannot
// reference directly.
func TestUserListClampsItsLimit(t *testing.T) {
	const wantClamp = 500
	f := &fakeController{}
	d := control.NewDispatch(f)
	req := []byte(`{"type":"ctrl_user_list","id":"1","limit":100000}`)
	got := mustSuccess(t, d.HandleFrame(nil, req))
	if f.lastUserListLimit != wantClamp {
		t.Fatalf("limit = %d, want clamp to %d", f.lastUserListLimit, wantClamp)
	}
	// The rows are wrapped, matching ctrl_conversation_search — read
	// dispatch.go before assuming a shape, the verbs are not uniform.
	var payload struct {
		Users []users.User `json:"users"`
	}
	if err := json.Unmarshal(got.Data, &payload); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(payload.Users) != 1 || payload.Users[0].Username != "brent" {
		t.Fatalf("users = %+v", payload.Users)
	}
}

func TestUserRmUnknownNameIsNotFound(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	req := []byte(`{"type":"ctrl_user_rm","id":"1","username":"ghost"}`)
	mustError(t, d.HandleFrame(nil, req), protocol.ErrNotFound)
}
