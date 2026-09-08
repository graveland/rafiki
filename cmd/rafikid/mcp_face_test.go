// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"
)

// mcpStubUsers is the identity backend for the auth tests. Only Authenticate
// is ever reached through the face's middleware.
type mcpStubUsers struct {
	users.Store
	tokens map[string]users.Identity
}

func (s *mcpStubUsers) Authenticate(_ context.Context, token string) (users.Identity, error) {
	id, ok := s.tokens[token]
	if !ok {
		return users.Identity{}, users.ErrNotFound
	}
	return id, nil
}

// mcpFaceFixture builds a face bound to a hand-populated Controller — no
// processes, no pool, no capture store — and returns the task ledger the
// task_* tools scope by, so tests can observe execution through it.
func mcpFaceFixture(t *testing.T) (*mcpFace, tasks.Store) {
	t.Helper()
	face := newMCPFace(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "test")
	store := tasks.NewMemoryStore()
	ctrl := &Controller{st: childstore.New(), cm: newChildManager(), tasks: store}
	face.SetController(ctrl)
	return face, store
}

// mcpRequestFor is a request carrying the given identity the way
// UserTokenAuth.Middleware leaves one: on the context, not in a header.
func mcpRequestFor(userID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	if userID == "" {
		return r
	}
	return r.WithContext(server.WithIdentity(r.Context(), &server.Identity{UserID: userID, Username: userID}))
}

// mcpConnect drives the SDK's initialize handshake against srv over an
// in-memory transport and returns the client session.
func mcpConnect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-face-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func mcpToolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// mcpPost sends one authenticated JSON-RPC message to the mounted face and
// returns the raw status, headers and every data payload the SSE stream
// carried. The middleware requires a credential on EVERY request, sessions
// included.
func mcpPost(t *testing.T, mux *http.ServeMux, sessionID, token, body string) (int, http.Header, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, mcpFacePath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var msgs []map[string]any
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &m); err != nil {
			t.Fatalf("decode sse payload %q: %v", line, err)
		}
		msgs = append(msgs, m)
	}
	return rec.Code, rec.Header(), msgs
}

const (
	mcpInitializeBody  = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-face-test","version":"0"}}}`
	mcpInitializedBody = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	mcpListToolsBody   = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	mcpTaskAddBody     = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"task_add","arguments":{"items":[{"content":"from-mcp","active_form":"Adding from mcp"}]}}}`
)

// mcpHandshake completes the streamable-HTTP initialize dance and returns the
// session id the face bound to the caller's identity.
func mcpHandshake(t *testing.T, mux *http.ServeMux, token string) string {
	t.Helper()
	code, hdr, msgs := mcpPost(t, mux, "", token, mcpInitializeBody)
	if code != http.StatusOK {
		t.Fatalf("initialize: got %d, want 200", code)
	}
	if len(msgs) == 0 || msgs[0]["result"] == nil {
		t.Fatalf("initialize produced no result: %v", msgs)
	}
	sid := hdr.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize returned no Mcp-Session-Id")
	}
	code, _, _ = mcpPost(t, mux, sid, token, mcpInitializedBody)
	if code != http.StatusAccepted {
		t.Fatalf("notifications/initialized: got %d, want 202", code)
	}
	return sid
}

// mcpResponseFor returns the JSON-RPC result of the response carrying id.
func mcpResponseFor(t *testing.T, msgs []map[string]any, id float64) map[string]any {
	t.Helper()
	for _, m := range msgs {
		if m["id"] == id {
			res, ok := m["result"].(map[string]any)
			if !ok {
				t.Fatalf("response %v carries no result: %v", id, m)
			}
			return res
		}
	}
	t.Fatalf("no response with id %v among %d messages", id, len(msgs))
	return nil
}

// mcpResultText returns the text of the first content block of a tools/call
// result and its is_error flag.
func mcpResultText(t *testing.T, msgs []map[string]any, id float64) (string, bool) {
	t.Helper()
	res := mcpResponseFor(t, msgs, id)
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result carries no content: %v", res)
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content block is not an object: %v", content[0])
	}
	text, _ := block["text"].(string)
	return text, res["isError"] == true
}

// mcpListedToolNames decodes a tools/list result carried over HTTP.
func mcpListedToolNames(t *testing.T, msgs []map[string]any, id float64) []string {
	t.Helper()
	res := mcpResponseFor(t, msgs, id)
	raw, err := json.Marshal(res["tools"])
	if err != nil {
		t.Fatal(err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("decode tools/list result: %v", err)
	}
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	return names
}

func TestMCPFaceIsReachedThroughUserTokenAuth(t *testing.T) {
	face, ledger := mcpFaceFixture(t)
	tokenAuth := server.NewUserTokenAuth(&mcpStubUsers{tokens: map[string]users.Identity{
		"tok-alice": {UserID: "u-alice", Username: "alice"},
	}}, "child-secret", time.Second)
	mux := http.NewServeMux()
	h := &server.Handler{}
	h.MCPPath, h.MCP = face.Routes()
	h.Mount(mux, func(next http.Handler) http.Handler {
		return tokenAuth.Middleware(traceMiddleware(next))
	})

	// No credential: the middleware refuses before the face builds a server,
	// so nothing — least of all a tool — executes. The task ledger is the
	// flag: task_add is the call this request attempts.
	code, _, _ := mcpPost(t, mux, "", "", mcpTaskAddBody)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tools/call: got %d, want 401", code)
	}
	if n := mcpLedgerCount(t, ledger, "u-alice"); n != 0 {
		t.Fatalf("unauthenticated request executed a tool: %d rows in the caller's ledger", n)
	}

	// Positive control: the identical call authenticated executes and lands
	// in the caller's ledger — proving the flag above was settable and the
	// 401 happened before execution rather than the tool being broken.
	sid := mcpHandshake(t, mux, "tok-alice")
	code, _, msgs := mcpPost(t, mux, sid, "tok-alice", mcpTaskAddBody)
	if code != http.StatusOK {
		t.Fatalf("authenticated tools/call: got %d, want 200", code)
	}
	if text, isErr := mcpResultText(t, msgs, 3); isErr {
		t.Fatalf("task_add failed: %s", text)
	}
	if n := mcpLedgerCount(t, ledger, "u-alice"); n != 1 {
		t.Fatalf("authenticated task_add did not reach the ledger: %d rows", n)
	}
}

// mcpLedgerCount counts the rows one synthetic ledger key holds.
func mcpLedgerCount(t *testing.T, store tasks.Store, userID string) int {
	t.Helper()
	rows, err := store.List(context.Background(), tasks.ListFilter{ConversationID: "user:" + userID})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestMCPFaceAnonymousCallerGetsNoTools(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	// The per-boot child token authenticates against nothing: the identity it
	// resolves to carries an empty UserID, which is exactly the non-user
	// credential that must not get agent control.
	tokenAuth := server.NewUserTokenAuth(&mcpStubUsers{}, "child-secret", time.Second)
	mux := http.NewServeMux()
	h := &server.Handler{}
	h.MCPPath, h.MCP = face.Routes()
	h.Mount(mux, func(next http.Handler) http.Handler {
		return tokenAuth.Middleware(traceMiddleware(next))
	})

	req := httptest.NewRequest(http.MethodPost, mcpFacePath, strings.NewReader(mcpInitializeBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer child-secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("child-token initialize: got %d, want 200", rec.Code)
	}
	sid := rec.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("child-token initialize returned no Mcp-Session-Id")
	}
	mcpPost(t, mux, sid, "child-secret", mcpInitializedBody)
	_, _, msgs := mcpPost(t, mux, sid, "child-secret", mcpListToolsBody)
	if names := mcpListedToolNames(t, msgs, 2); len(names) != 0 {
		t.Fatalf("anonymous caller sees %v, want no tools", names)
	}
}

func TestMCPFaceServerIsBuiltPerRequest(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	s1 := face.getServer(mcpRequestFor("u-alice"))
	s2 := face.getServer(mcpRequestFor("u-bob"))
	if s1 == nil || s2 == nil {
		t.Fatal("getServer returned nil for an authenticated user")
	}
	if s1 == s2 {
		t.Fatal("two identities got the same *mcp.Server; the per-request binding is gone")
	}

	// The binding, made observable: the ledger each server resolves scopes by
	// the identity it was built for. (agent_list cannot show this — List is
	// deliberately unscoped on this surface, the plan's accepted constraint —
	// so the task ledger closure is the witness.)
	cs1 := mcpConnect(t, s1)
	cs2 := mcpConnect(t, s2)
	ctx := context.Background()
	if _, err := cs1.CallTool(ctx, &mcp.CallToolParams{Name: "task_add", Arguments: map[string]any{
		"items": []any{map[string]any{"content": "from-alice", "active_form": "Adding from alice"}},
	}}); err != nil {
		t.Fatal(err)
	}
	res, err := cs1.CallTool(ctx, &mcp.CallToolParams{Name: "task_list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpCallText(t, res), "from-alice") {
		t.Fatalf("alice's ledger lost the task: %s", mcpCallText(t, res))
	}
	res, err = cs2.CallTool(ctx, &mcp.CallToolParams{Name: "task_list"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mcpCallText(t, res), "from-alice") {
		t.Fatalf("bob's server read alice's ledger: %s", mcpCallText(t, res))
	}
}

func mcpCallText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool call failed: %v", res.Content)
	}
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content block is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

func TestMCPFaceWithoutControllerServesNoTools(t *testing.T) {
	face := newMCPFace(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, "test")
	srv := face.getServer(mcpRequestFor("u-alice"))
	if srv == nil {
		t.Fatal("getServer returned nil before SetController; StreamableHTTPHandler would answer 400")
	}
	names := mcpToolNames(t, mcpConnect(t, srv))
	if len(names) != 0 {
		t.Fatalf("toolless server exposed %v", names)
	}
}

// The exact twelve names of the surface, sorted. A blueprint that begins
// declining under the face's ToolOpts — or a new one added to the table
// without being materialized — shows up here as a missing name.
func TestMCPFaceExposesTheExpectedToolNames(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	names := mcpToolNames(t, mcpConnect(t, face.getServer(mcpRequestFor("u-alice"))))
	want := []string{
		"agent_kill",
		"agent_list",
		"agent_models",
		"agent_send",
		"agent_set_budget",
		"agent_spawn",
		"agent_view",
		"quota_status",
		"task_add",
		"task_drop",
		"task_list",
		"task_update",
	}
	if len(names) != len(want) {
		t.Fatalf("got %d tools %v, want %d", len(names), names, len(want))
	}
	for i, n := range names {
		if n != want[i] {
			t.Fatalf("tool %d: got %q, want %q (full list %v)", i, n, want[i], names)
		}
	}
}

// The overrides compose the blueprints' own text with the surface framing, so
// a wording change on the fundi side flows through here; and none of them
// promises a settlement notification this surface does not deliver.
func TestMCPFaceDescriptionsCarryTheBlueprintText(t *testing.T) {
	pairs := map[string]tools.Tool{
		"agent_spawn": &tools.AgentSpawnBlueprint{},
		"agent_send":  &tools.AgentSendBlueprint{},
		"agent_kill":  &tools.AgentKillBlueprint{},
		"task_add":    &tools.TaskAddBlueprint{},
		"task_update": &tools.TaskUpdateBlueprint{},
		"task_drop":   &tools.TaskDropBlueprint{},
		"task_list":   &tools.TaskListBlueprint{},
	}
	for name, bp := range pairs {
		desc, overridden := mcpToolDescriptions[name]
		if !overridden {
			continue
		}
		if !strings.Contains(desc, bp.Description()) {
			t.Errorf("%s: override no longer carries the blueprint's description; re-derive it instead of retyping", name)
		}
	}
	for name := range mcpToolDescriptions {
		var known bool
		for _, bp := range mcpBlueprints {
			if bp.Name() == name {
				known = true
			}
		}
		if !known {
			t.Errorf("mcpToolDescriptions[%q] names no blueprint; the override is silently dead", name)
		}
	}
	for _, name := range []string{"agent_list", "agent_view", "agent_spawn"} {
		if desc := mcpToolDescriptions[name]; strings.Contains(desc, "you are notified when a subagent settles") {
			t.Errorf("%s: promises a settlement notification this surface does not deliver", name)
		}
	}
}
