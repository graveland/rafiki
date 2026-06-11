package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/protocol"
	"git.graveland.dev/brent/pi-controller/internal/server"
	"git.graveland.dev/brent/pi-controller/internal/store"
)

// discardConn is a no-op Connection used in dispatch tests where event
// delivery is not under test.
type discardConn struct{}

func (discardConn) Deliver(_ []byte) {}

// ─── fakeController ───────────────────────────────────────────────────────────

type fakeController struct {
	listFn                func(protocol.ListFilter) []store.Snapshot
	getFn                 func(string) (store.Snapshot, bool)
	getRecentFn           func(string, server.RecentQuery) (server.RecentResult, error)
	searchFn              func(server.SearchQuery) server.SearchResult
	statusFn              func() server.ControllerStatus
	spawnFn               func(context.Context, protocol.SpawnRequest) (server.SpawnResult, error)
	resumeFn              func(context.Context, string, string) (server.SpawnResult, error)
	killFn                func(context.Context, string, int64, int64) (server.KillResult, error)
	forgetFn              func(string) error
	forgetAllExitedFn     func(int64) (int, error)
	sendFn                func(string, json.RawMessage) error
	subscribeFn           func(string, server.Connection, protocol.SubscribeFilter) error
	unsubscribeFn         func(string, server.Connection) error
	globalSubscribeFn     func(server.Connection) error
	globalUnsubscribeFn   func(server.Connection) error
	subscribeLabeledFn    func(server.Connection, map[string]string, []string, protocol.SubscribeFilter) error
	onConnectionCloseFn   func(server.Connection)
	listModelsFn          func(context.Context, string) ([]protocol.ModelInfo, error)
	listPresetsFn         func(map[string]string, []string) ([]protocol.PresetInfo, error)
}

func (f *fakeController) List(filter protocol.ListFilter) []store.Snapshot {
	if f.listFn != nil {
		return f.listFn(filter)
	}
	return nil
}

func (f *fakeController) Get(childID string) (store.Snapshot, bool) {
	if f.getFn != nil {
		return f.getFn(childID)
	}
	return store.Snapshot{}, false
}

func (f *fakeController) GetRecent(childID string, q server.RecentQuery) (server.RecentResult, error) {
	if f.getRecentFn != nil {
		return f.getRecentFn(childID, q)
	}
	return server.RecentResult{}, nil
}

func (f *fakeController) Search(q server.SearchQuery) server.SearchResult {
	if f.searchFn != nil {
		return f.searchFn(q)
	}
	return server.SearchResult{}
}

func (f *fakeController) Status() server.ControllerStatus {
	if f.statusFn != nil {
		return f.statusFn()
	}
	return server.ControllerStatus{}
}

func (f *fakeController) Spawn(ctx context.Context, req protocol.SpawnRequest) (server.SpawnResult, error) {
	if f.spawnFn != nil {
		return f.spawnFn(ctx, req)
	}
	return server.SpawnResult{}, nil
}

func (f *fakeController) Resume(ctx context.Context, childID string, apiKey string) (server.SpawnResult, error) {
	if f.resumeFn != nil {
		return f.resumeFn(ctx, childID, apiKey)
	}
	return server.SpawnResult{}, nil
}

func (f *fakeController) Kill(ctx context.Context, childID string, shutdownMs, killMs int64) (server.KillResult, error) {
	if f.killFn != nil {
		return f.killFn(ctx, childID, shutdownMs, killMs)
	}
	return server.KillResult{}, nil
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

func (f *fakeController) Subscribe(childID string, conn server.Connection, filter protocol.SubscribeFilter) error {
	if f.subscribeFn != nil {
		return f.subscribeFn(childID, conn, filter)
	}
	return nil
}

func (f *fakeController) Unsubscribe(childID string, conn server.Connection) error {
	if f.unsubscribeFn != nil {
		return f.unsubscribeFn(childID, conn)
	}
	return nil
}

func (f *fakeController) GlobalSubscribe(conn server.Connection) error {
	if f.globalSubscribeFn != nil {
		return f.globalSubscribeFn(conn)
	}
	return nil
}

func (f *fakeController) GlobalUnsubscribe(conn server.Connection) error {
	if f.globalUnsubscribeFn != nil {
		return f.globalUnsubscribeFn(conn)
	}
	return nil
}

func (f *fakeController) SubscribeLabeled(conn server.Connection, labels map[string]string, hasLabel []string, filter protocol.SubscribeFilter) error {
	if f.subscribeLabeledFn != nil {
		return f.subscribeLabeledFn(conn, labels, hasLabel, filter)
	}
	return nil
}

func (f *fakeController) OnConnectionClose(conn server.Connection) {
	if f.onConnectionCloseFn != nil {
		f.onConnectionCloseFn(conn)
	}
}

func (f *fakeController) SetLabels(childID string, set map[string]string, remove []string) (map[string]string, error) {
	return nil, &server.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
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

// controllerErr returns a *server.ControllerError for use in fakes.
func controllerErr(code, msg string) error {
	return &server.ControllerError{Code: code, Message: msg}
}

// makeSnapshot builds a minimal store.Snapshot for testing.
func makeSnapshot(childID string, status protocol.Status) store.Snapshot {
	return store.Snapshot{
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
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{not valid json`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_UnknownType(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_nonexistent","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_list ────────────────────────────────────────────────────────────────

func TestDispatch_List_Success(t *testing.T) {
	snap := makeSnapshot("c_001", protocol.StatusIdle)
	c := &fakeController{
		listFn: func(f protocol.ListFilter) []store.Snapshot {
			return []store.Snapshot{snap}
		},
	}
	d := server.NewDispatch(c)
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
		listFn: func(f protocol.ListFilter) []store.Snapshot {
			capturedFilter = f
			return nil
		},
	}
	d := server.NewDispatch(c)
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
		listFn: func(_ protocol.ListFilter) []store.Snapshot { return []store.Snapshot{snap} },
	}
	d := server.NewDispatch(c)
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
		getFn: func(id string) (store.Snapshot, bool) {
			if id == "c_001" {
				return snap, true
			}
			return store.Snapshot{}, false
		},
	}
	d := server.NewDispatch(c)
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

// TestDispatch_Get_ClaudeChildMetadata asserts a claude child's ctrl_get
// response carries kind:"claude" plus the sessionId/model/cwd/name fields the
// attach layer's ChildMetadata reads (fetchChildMetadata maps data.name →
// sessionName). claude has no session file, so sessionFile is empty.
func TestDispatch_Get_ClaudeChildMetadata(t *testing.T) {
	snap := store.Snapshot{
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
		getFn: func(id string) (store.Snapshot, bool) {
			if id == "c_claude" {
				return snap, true
			}
			return store.Snapshot{}, false
		},
	}
	d := server.NewDispatch(c)
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
		getFn: func(id string) (store.Snapshot, bool) { return snap, true },
	}
	d := server.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"10","childId":"c_pi"}`)))
	var m map[string]any
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["kind"]; present {
		t.Fatalf("pi child must omit kind on the wire, got %v", m["kind"])
	}
}

func TestDispatch_Get_NotFound(t *testing.T) {
	c := &fakeController{
		getFn: func(string) (store.Snapshot, bool) { return store.Snapshot{}, false },
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Get_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_status ─────────────────────────────────────────────────────────────

func TestDispatch_Status_Success(t *testing.T) {
	c := &fakeController{
		statusFn: func() server.ControllerStatus {
			return server.ControllerStatus{
				Version:   "0.1.0",
				StartedAt: 1716000000,
				Children:  protocol.ChildCounts{Live: 2, Exited: 1},
			}
		},
	}
	d := server.NewDispatch(c)
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
		getRecentFn: func(childID string, q server.RecentQuery) (server.RecentResult, error) {
			if childID != "c_001" {
				return server.RecentResult{}, errors.New("wrong child")
			}
			return server.RecentResult{
				Events:           []json.RawMessage{event},
				TotalInBuffer:    1,
				OldestTimestamp:  1716000000,
				TruncatedByLimit: false,
			}, nil
		},
	}
	d := server.NewDispatch(c)
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
		getRecentFn: func(string, server.RecentQuery) (server.RecentResult, error) {
			return server.RecentResult{}, nil // Events is nil
		},
	}
	d := server.NewDispatch(c)
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

func TestDispatch_GetRecent_NotFound(t *testing.T) {
	c := &fakeController{
		getRecentFn: func(string, server.RecentQuery) (server.RecentResult, error) {
			return server.RecentResult{}, controllerErr(protocol.ErrChildNotFound, "child not found")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get_recent","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_GetRecent_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get_recent","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_search ─────────────────────────────────────────────────────────────

func TestDispatch_Search_Success(t *testing.T) {
	c := &fakeController{
		searchFn: func(q server.SearchQuery) server.SearchResult {
			if q.Query != "ublk_register" {
				return server.SearchResult{}
			}
			return server.SearchResult{
				Hits: []protocol.SearchHit{
					{ChildID: "c_001", Snippet: "calling ublk_register", MatchStart: 8, MatchEnd: 21},
				},
				TotalHits: 1,
				Scanned:   3,
				Elapsed:   42,
			}
		},
	}
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_search","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Search_EmptyHitsIsArray(t *testing.T) {
	c := &fakeController{
		searchFn: func(server.SearchQuery) server.SearchResult {
			return server.SearchResult{} // Hits is nil
		},
	}
	d := server.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_search","id":"x","query":"needle"}`)))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["hits"]) == "null" {
		t.Error("hits should be [] not null")
	}
}

// ─── ctrl_spawn ───────────────────────────────────────────────────────────────

func TestDispatch_Spawn_Success(t *testing.T) {
	c := &fakeController{
		spawnFn: func(_ context.Context, req protocol.SpawnRequest) (server.SpawnResult, error) {
			if req.Cwd != "/work" {
				return server.SpawnResult{}, errors.New("unexpected cwd")
			}
			return server.SpawnResult{
				ChildID:     "c_new",
				SessionID:   "sess-new",
				SessionFile: "/tmp/new.jsonl",
				Model:       "anthropic/claude-sonnet-4",
				Stalled:     false,
			}, nil
		},
	}
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_spawn","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Spawn_RelativeCwd(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_spawn","id":"x","cwd":"relative/path"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Spawn_Failure(t *testing.T) {
	c := &fakeController{
		spawnFn: func(_ context.Context, _ protocol.SpawnRequest) (server.SpawnResult, error) {
			return server.SpawnResult{}, controllerErr(protocol.ErrSpawnFailed, "pi binary not found")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_spawn","id":"x","cwd":"/work"}`))
	mustError(t, resp, protocol.ErrSpawnFailed)
}

// ─── ctrl_resume ─────────────────────────────────────────────────────────────

func TestDispatch_Resume_Success(t *testing.T) {
	c := &fakeController{
		resumeFn: func(_ context.Context, childID, apiKey string) (server.SpawnResult, error) {
			if childID != "c_001" {
				return server.SpawnResult{}, controllerErr(protocol.ErrNotFound, "not found")
			}
			return server.SpawnResult{ChildID: "c_001", SessionID: "sess-001"}, nil
		},
	}
	d := server.NewDispatch(c)
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
		resumeFn: func(_ context.Context, _ string, _ string) (server.SpawnResult, error) {
			return server.SpawnResult{}, controllerErr(protocol.ErrNotFound, "child not found")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_resume","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrNotFound)
}

func TestDispatch_Resume_NotResumable(t *testing.T) {
	c := &fakeController{
		resumeFn: func(_ context.Context, _ string, _ string) (server.SpawnResult, error) {
			return server.SpawnResult{}, controllerErr(protocol.ErrNotResumable, "child is not exited")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_resume","id":"x","childId":"c_live"}`))
	mustError(t, resp, protocol.ErrNotResumable)
}

func TestDispatch_Resume_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_resume","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_kill ────────────────────────────────────────────────────────────────

func TestDispatch_Kill_Success(t *testing.T) {
	exitCode := 0
	c := &fakeController{
		killFn: func(_ context.Context, childID string, shutdownMs, killMs int64) (server.KillResult, error) {
			if childID != "c_001" {
				return server.KillResult{}, errors.New("wrong child")
			}
			if shutdownMs != 180000 || killMs != 30000 {
				return server.KillResult{}, errors.New("wrong timeouts")
			}
			return server.KillResult{
				ExitCode:   &exitCode,
				DurationMs: 1247,
				Escalated:  false,
			}, nil
		},
	}
	d := server.NewDispatch(c)
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
		killFn: func(_ context.Context, _ string, _, _ int64) (server.KillResult, error) {
			return server.KillResult{}, controllerErr(protocol.ErrChildNotFound, "child not found")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_kill","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Kill_AlreadyExited(t *testing.T) {
	c := &fakeController{
		killFn: func(_ context.Context, _ string, _, _ int64) (server.KillResult, error) {
			return server.KillResult{}, controllerErr(protocol.ErrChildExited, "child already exited")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_kill","id":"x","childId":"c_dead"}`))
	mustError(t, resp, protocol.ErrChildExited)
}

func TestDispatch_Kill_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
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
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_forget","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrNotFound)
}

func TestDispatch_Forget_NotExited(t *testing.T) {
	c := &fakeController{
		forgetFn: func(string) error {
			return controllerErr(protocol.ErrNotExited, "child is still running")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_forget","id":"x","childId":"c_live"}`))
	mustError(t, resp, protocol.ErrNotExited)
}

func TestDispatch_Forget_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
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
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_missing","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Send_Backpressure(t *testing.T) {
	c := &fakeController{
		sendFn: func(string, json.RawMessage) error {
			return controllerErr(protocol.ErrBackpressure, "channel full")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_001","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrBackpressure)
}

func TestDispatch_Send_ShuttingDown(t *testing.T) {
	c := &fakeController{
		sendFn: func(string, json.RawMessage) error {
			return controllerErr(protocol.ErrChildShuttingDown, "child is shutting down")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_001","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrChildShuttingDown)
}

func TestDispatch_Send_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","frame":{"type":"prompt"}}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Send_MissingFrame(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_send","id":"x","childId":"c_001"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_subscribe ───────────────────────────────────────────────────────────

func TestDispatch_Subscribe_Success(t *testing.T) {
	var capturedChildID string
	var capturedFilter protocol.SubscribeFilter
	c := &fakeController{
		subscribeFn: func(childID string, _ server.Connection, filter protocol.SubscribeFilter) error {
			capturedChildID = childID
			capturedFilter = filter
			return nil
		},
	}
	d := server.NewDispatch(c)
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
		subscribeFn: func(string, server.Connection, protocol.SubscribeFilter) error {
			return controllerErr(protocol.ErrChildNotFound, "child not found")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_subscribe","id":"x","childId":"c_missing"}`))
	mustError(t, resp, protocol.ErrChildNotFound)
}

func TestDispatch_Subscribe_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_subscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

func TestDispatch_Subscribe_LabelFiltered_Success(t *testing.T) {
	var gotLabels map[string]string
	var gotHasLabel []string
	var gotFilter protocol.SubscribeFilter
	c := &fakeController{
		subscribeLabeledFn: func(_ server.Connection, labels map[string]string, hasLabel []string, filter protocol.SubscribeFilter) error {
			gotLabels = labels
			gotHasLabel = hasLabel
			gotFilter = filter
			return nil
		},
	}
	d := server.NewDispatch(c)
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
		subscribeLabeledFn: func(_ server.Connection, _ map[string]string, _ []string, filter protocol.SubscribeFilter) error {
			gotFilter = filter
			return nil
		},
	}
	d := server.NewDispatch(c)
	frame := `{"type":"ctrl_subscribe","id":"lf-2","labels":{"env":"prod"},"filter":{"profile":"coarse"}}`
	mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if gotFilter.Profile != "coarse" {
		t.Errorf("filter.profile: %s", gotFilter.Profile)
	}
}

func TestDispatch_Subscribe_ChildIDAndLabels_MutuallyExclusive(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
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
		subscribeLabeledFn: func(_ server.Connection, _ map[string]string, hasLabel []string, _ protocol.SubscribeFilter) error {
			called = true
			if len(hasLabel) != 1 || hasLabel[0] != "tier" {
				return &server.ControllerError{Code: protocol.ErrInvalidArgs, Message: "wrong hasLabel"}
			}
			return nil
		},
	}
	d := server.NewDispatch(c)
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
		unsubscribeFn: func(childID string, _ server.Connection) error {
			capturedChildID = childID
			return nil
		},
	}
	d := server.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_unsubscribe","id":"7","childId":"c_001"}`)))
	if r.Command != protocol.TypeCtrlUnsubscribe {
		t.Errorf("command: %s", r.Command)
	}
	if capturedChildID != "c_001" {
		t.Errorf("childID: %s", capturedChildID)
	}
}

func TestDispatch_Unsubscribe_MissingChildID(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_unsubscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}

// ─── ctrl_global_subscribe / ctrl_global_unsubscribe ─────────────────────────

func TestDispatch_GlobalSubscribe_Success(t *testing.T) {
	called := false
	c := &fakeController{
		globalSubscribeFn: func(server.Connection) error {
			called = true
			return nil
		},
	}
	d := server.NewDispatch(c)
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
		globalUnsubscribeFn: func(server.Connection) error {
			called = true
			return nil
		},
	}
	d := server.NewDispatch(c)
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
		globalSubscribeFn: func(server.Connection) error {
			return controllerErr(protocol.ErrInternal, "boom")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_global_subscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInternal)
}

func TestDispatch_GlobalUnsubscribe_Error(t *testing.T) {
	c := &fakeController{
		globalUnsubscribeFn: func(server.Connection) error {
			return controllerErr(protocol.ErrInternal, "boom")
		},
	}
	d := server.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_global_unsubscribe","id":"x"}`))
	mustError(t, resp, protocol.ErrInternal)
}

// ─── ID propagation ───────────────────────────────────────────────────────────

func TestDispatch_IDPropagatedInErrorResponse(t *testing.T) {
	d := server.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_get","id":"req-99","childId":"c_missing"}`))
	r := parseResponse(t, resp)
	if r.ID != "req-99" {
		t.Errorf("id not propagated in error response: got %s", r.ID)
	}
}

func TestDispatch_IDPropagatedInSuccessResponse(t *testing.T) {
	c := &fakeController{
		statusFn: func() server.ControllerStatus { return server.ControllerStatus{} },
	}
	d := server.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_status","id":"abc-123"}`)))
	if r.ID != "abc-123" {
		t.Errorf("id not propagated in success response: got %s", r.ID)
	}
}

// ─── Concurrent safety ────────────────────────────────────────────────────────

func TestDispatch_ConcurrentSafe(t *testing.T) {
	c := &fakeController{
		statusFn: func() server.ControllerStatus { return server.ControllerStatus{Version: "0.1.0"} },
		listFn:   func(protocol.ListFilter) []store.Snapshot { return nil },
	}
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(c)

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
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(&fakeController{})
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
	d := server.NewDispatch(c)

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
	d := server.NewDispatch(c)
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
	d := server.NewDispatch(c)
	frame := `{"type":"ctrl_list_presets","id":"p4","hasLabel":["team"]}`
	mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))
	if len(capturedHasLabel) != 1 || capturedHasLabel[0] != "team" {
		t.Errorf("hasLabel not passed through: %v", capturedHasLabel)
	}
}
