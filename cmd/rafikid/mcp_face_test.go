// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

// mcpStubQuota is a quota source for tests that need quota_status to
// materialize: a real *quota.Store is non-nil only over a pool.
type mcpStubQuota struct{}

func (mcpStubQuota) RateLimitStatus(context.Context) (tools.QuotaStatus, bool, error) {
	return tools.QuotaStatus{}, false, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mcpFaceFixture builds a face bound to a hand-populated Controller — no
// processes, no pool, no capture store — and returns the task ledger the
// task_* tools scope by, so tests can observe execution through it.
func mcpFaceFixture(t *testing.T) (*mcpFace, tasks.Store) {
	t.Helper()
	face := newMCPFace(discardLogger(), nil, nil, "test")
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
	return r.WithContext(server.WithIdentity(r.Context(), &server.Identity{UserID: userID, Username: userID, Via: server.ProvenanceUser}))
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
// included. The variadic hooks exist for the request shapes only the
// provenance tests need — X-Rafiki-Session riding alongside a credential —
// and no existing call site passes one.
func mcpPost(t *testing.T, mux *http.ServeMux, sessionID, token, body string, extra ...func(*http.Request)) (int, http.Header, []map[string]any) {
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
	for _, set := range extra {
		set(req)
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

	// A user token that ALSO carries X-Rafiki-Session stays ProvenanceUser —
	// provenance is a property of the credential, not the header — so a
	// hand-configured client sending the child-attribution header keeps the
	// full surface.
	withSession := func(r *http.Request) { r.Header.Set("X-Rafiki-Session", "c_child2") }
	code, hdr, _ := mcpPost(t, mux, "", "tok-alice", mcpInitializeBody, withSession)
	if code != http.StatusOK {
		t.Fatalf("user-token initialize with session header: got %d, want 200", code)
	}
	sid2 := hdr.Get("Mcp-Session-Id")
	if sid2 == "" {
		t.Fatal("user-token initialize with session header returned no Mcp-Session-Id")
	}
	mcpPost(t, mux, sid2, "tok-alice", mcpInitializedBody, withSession)
	code, _, msgs = mcpPost(t, mux, sid2, "tok-alice", mcpTaskAddBody, withSession)
	if code != http.StatusOK {
		t.Fatalf("user token with session header: got %d, want 200", code)
	}
	if text, isErr := mcpResultText(t, msgs, 3); isErr {
		t.Fatalf("user token with session header lost the surface: %s", text)
	}
	if n := mcpLedgerCount(t, ledger, "u-alice"); n != 2 {
		t.Fatalf("user token with session header did not execute: %d rows, want 2", n)
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

// TestUnentitledReturns403 covers the attribution path:
// the daemon wires childOwnerLookup so the child token + X-Rafiki-Session pair
// attributes to the owner — the path that bills the child's LLM turns. The
// resulting identity carries the owner's REAL UserID, so any non-empty-UserID
// check would admit it; provenance is what refuses it.
func TestUnentitledReturns403(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	tokenAuth := server.NewUserTokenAuth(&mcpStubUsers{}, "child-secret", time.Second)
	tokenAuth.SetChildOwnerLookup(func(string) (string, bool) { return "u-owner", true })
	mux := http.NewServeMux()
	h := &server.Handler{}
	h.MCPPath, h.MCP = face.Routes()
	h.Mount(mux, func(next http.Handler) http.Handler {
		return tokenAuth.Middleware(traceMiddleware(next))
	})
	withSession := func(r *http.Request) { r.Header.Set("X-Rafiki-Session", "c_child1") }
	code, _, _ := mcpPost(t, mux, "", "child-secret", mcpInitializeBody, withSession)
	if code != http.StatusForbidden {
		t.Fatalf("child-attributed initialize: got %d, want 403", code)
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

func TestNoControllerReturns503(t *testing.T) {
	face := newMCPFace(discardLogger(), nil, nil, "test")
	if srv := face.getServer(mcpRequestFor("u-alice")); srv != nil {
		t.Fatal("getServer built a server before SetController; the nil must reach Routes so it can answer 503")
	}
	// The status, not the nil server, is the answer an MCP client reads.
	mux := http.NewServeMux()
	h := &server.Handler{}
	h.MCPPath, h.MCP = face.Routes()
	h.Mount(mux, func(next http.Handler) http.Handler { return next })
	code, hdr, _ := mcpPost(t, mux, "", "tok-alice", mcpInitializeBody)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("initialize before the controller is wired: got %d, want 503", code)
	}
	if ra := hdr.Get("Retry-After"); ra == "" {
		t.Fatal("503 carried no Retry-After; a client cannot know to retry")
	}
}

// The exact names of the surface, sorted, compared against literals. A
// blueprint that begins declining under the face's ToolOpts — or a new one
// added to the table without being materialized — shows up here as a missing
// name. The fixture face is DB-less (no quota store), so quota_status is
// absent by its own blueprint's decline rule; the full set including it is
// pinned by TestMCPFaceMaterializesTheFullSetWhenAQuotaSourceExists.
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
	if slices.Contains(names, "quota_status") {
		t.Fatalf("DB-less face (nil quota store) materialized quota_status; the blueprint's decline rule must fire")
	}
}

// The complete surface — every blueprint, quota_status included — materializes
// when a quota source exists, so the DB-less decline above cannot hide a
// blueprint that stopped materializing for some other reason.
func TestMCPFaceMaterializesTheFullSetWhenAQuotaSourceExists(t *testing.T) {
	face, ledger := mcpFaceFixture(t)
	opts := tools.ToolOpts{
		Agents: newUserSpawner(face.controller(), users.Identity{UserID: "u-alice", Username: "alice"}),
		Tasks:  ledger,
		Quota:  mcpStubQuota{},
	}
	var names []string
	for _, tool := range mcpToolset(opts, discardLogger()) {
		names = append(names, tool.Name())
	}
	slices.Sort(names)
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
	if !slices.Equal(names, want) {
		t.Fatalf("full tool set = %v, want %v", names, want)
	}
}

// TestMCPFaceRejectsASessionIDPresentedByAnotherCaller pins the per-session
// identity binding. The SDK's own hijack guard never fires on this mount:
// sessInfo.userID is captured only from auth.TokenInfoFromContext, which only
// the SDK's RequireBearerToken middleware populates, and rafiki authenticates
// with UserTokenAuth instead — so the face binds each Mcp-Session-Id to the
// identity that initialized it and rejects mismatches before dispatch.
// TestMCPFaceRejectsASessionIDPresentedByAnotherCaller pins the per-session
// identity binding. The SDK's own hijack guard never fires on this mount:
// sessInfo.userID is captured only from auth.TokenInfoFromContext, which only
// the SDK's RequireBearerToken middleware populates, and rafiki authenticates
// with UserTokenAuth instead — so the face binds each Mcp-Session-Id to the
// identity that initialized it and rejects mismatches before dispatch.
func TestMCPFaceRejectsASessionIDPresentedByAnotherCaller(t *testing.T) {
	face, ledger := mcpFaceFixture(t)
	tokenAuth := server.NewUserTokenAuth(&mcpStubUsers{tokens: map[string]users.Identity{
		"tok-alice": {UserID: "u-alice", Username: "alice"},
		"tok-bob":   {UserID: "u-bob", Username: "bob"},
	}}, "child-secret", time.Second)
	mux := http.NewServeMux()
	h := &server.Handler{}
	h.MCPPath, h.MCP = face.Routes()
	h.Mount(mux, func(next http.Handler) http.Handler {
		return tokenAuth.Middleware(traceMiddleware(next))
	})
	sid := mcpHandshake(t, mux, "tok-alice")

	// bob's token + alice's session id: refused before any tool runs.
	code, _, _ := mcpPost(t, mux, sid, "tok-bob", mcpTaskAddBody)
	if code != http.StatusForbidden {
		t.Fatalf("bob on alice's session: got %d, want 403", code)
	}
	if n := mcpLedgerCount(t, ledger, "u-bob"); n != 0 {
		t.Fatalf("bob's session ride-along executed a tool: %d rows", n)
	}
	if n := mcpLedgerCount(t, ledger, "u-alice"); n != 0 {
		t.Fatalf("bob reached alice's ledger through her session: %d rows", n)
	}

	// Positive control: alice on her own session still executes.
	code, _, msgs := mcpPost(t, mux, sid, "tok-alice", mcpTaskAddBody)
	if code != http.StatusOK {
		t.Fatalf("alice on her own session: got %d, want 200", code)
	}
	if text, isErr := mcpResultText(t, msgs, 3); isErr {
		t.Fatalf("task_add failed for the session's owner: %s", text)
	}

	// A session id the face never bound is refused too — a map miss must
	// never become the one path that dispatches unvalidated.
	code, _, _ = mcpPost(t, mux, "mcp-not-a-real-session", "tok-bob", mcpListToolsBody)
	if code != http.StatusForbidden {
		t.Fatalf("unknown session id: got %d, want 403", code)
	}
}

// The overrides compose the blueprints' own text with the surface framing, so
// a wording change on the fundi side flows through here. agent_spawn is
// special: its blueprint text carries a settlement promise this surface cannot
// keep, so the composition excises it and the guard keys on the exact wording
// that promise uses — a vacuous check here is how the contradiction shipped
// once already.
func TestMCPFaceDescriptionsCarryTheBlueprintText(t *testing.T) {
	spawnDesc := mcpToolDescriptions["agent_spawn"]
	for _, phrase := range []string{
		"You will be notified",
		"Do not sleep, poll",
		"you are notified when a subagent settles",
	} {
		if strings.Contains(spawnDesc, phrase) {
			t.Errorf("agent_spawn: ships the fundi-only notification promise %q", phrase)
		}
	}
	if !strings.Contains(spawnDesc, mcpSpawnPrefix) {
		t.Errorf("agent_spawn: the mandated prefix no longer rides at the front")
	}
	if !strings.Contains(spawnDesc, mcpNotificationNote) {
		t.Errorf("agent_spawn: the conditional note is gone; there is no notification wording left")
	}
	if !strings.Contains(spawnDesc, mcpSpawnKeepDoing) {
		t.Errorf("agent_spawn: the blueprint remainder after the excision was cut too; cut only %q..%q", mcpSpawnNotifyStart, mcpSpawnKeepDoing)
	}

	pairs := map[string]tools.Tool{
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
	for name, desc := range mcpToolDescriptions {
		for _, phrase := range []string{"You will be notified", "Do not sleep, poll", "you are notified when a subagent settles"} {
			if strings.Contains(desc, phrase) {
				t.Errorf("%s: promises a settlement notification this surface does not deliver", name)
			}
		}
	}
}

// TestChildTokenGetsTwelveTools: a per-child token identity binds a
// controllerSpawner scoped to the child's own subtree, and the surface is the
// same twelve-tool set a user credential gets — scope comes from
// controllerSpawner.authorize, never a per-tool allowlist. The exact twelve
// names are the assertion of record (test/integration/mcp_test.go).
func TestChildTokenGetsTwelveTools(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	// The DB-less fixture's *quota.Store is nil, so quota_status declines by
	// its own rule; stub the reader for the full-set assertion, exactly as
	// TestMCPFaceMaterializesTheFullSetWhenAQuotaSourceExists does.
	opts := tools.ToolOpts{
		Agents: newControllerSpawner(face.controller(), "c-child"),
		Tasks:  face.taskStoreFor(face.controller()),
		Quota:  mcpStubQuota{},
	}
	var names []string
	for _, tool := range mcpToolset(opts, discardLogger()) {
		names = append(names, tool.Name())
	}
	slices.Sort(names)
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
	if !slices.Equal(names, want) {
		t.Fatalf("child-token tool set = %v, want %v", names, want)
	}

	// The identity, not just the spawner, drives the binding: a
	// ProvenanceChildToken request through getServer must materialize a
	// server (its only decline here would be the DB-less quota one, never a
	// credential refusal).
	r := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	ctx := server.WithIdentity(r.Context(), &server.Identity{UserID: "u-owner", ChildID: "c-child", Via: server.ProvenanceChildToken})
	if srv := face.getServer(r.WithContext(ctx)); srv == nil {
		t.Fatal("a ProvenanceChildToken identity got no server from getServer")
	}
}

// TestSiblingCannotUseSiblingSession pins the (UserID, ChildID) rekey: two
// children of one owner share a UserID, so a UserID-keyed session map let
// child B present child A's Mcp-Session-Id and execute against A's bound
// spawner — a privilege-escalation path between siblings.
func TestSiblingCannotUseSiblingSession(t *testing.T) {
	face, ledger := mcpFaceFixture(t)
	tokenAuth := server.NewUserTokenAuth(&mcpStubUsers{}, "child-secret", time.Second)
	tokenAuth.SetChildTokenLookup(func(token string) (string, string, bool) {
		switch token {
		case "child-secret-a":
			return "c-a", "u-owner", true
		case "child-secret-b":
			return "c-b", "u-owner", true
		}
		return "", "", false
	})
	mux := http.NewServeMux()
	h := &server.Handler{}
	h.MCPPath, h.MCP = face.Routes()
	h.Mount(mux, func(next http.Handler) http.Handler {
		return tokenAuth.Middleware(traceMiddleware(next))
	})

	sid := mcpHandshake(t, mux, "child-secret-a")

	// Child B, same owner, child A's session id: refused before any tool
	// runs, even though the UserIDs match exactly.
	code, _, _ := mcpPost(t, mux, sid, "child-secret-b", mcpTaskAddBody)
	if code != http.StatusForbidden {
		t.Fatalf("sibling on sibling's session: got %d, want 403", code)
	}
	if n := mcpLedgerCount(t, ledger, "u-owner"); n != 0 {
		t.Fatalf("sibling ride-along executed a tool: %d rows", n)
	}

	// Positive control: child A on its own session still executes.
	code, _, msgs := mcpPost(t, mux, sid, "child-secret-a", mcpTaskAddBody)
	if code != http.StatusOK {
		t.Fatalf("child A on its own session: got %d, want 200", code)
	}
	if text, isErr := mcpResultText(t, msgs, 3); isErr {
		t.Fatalf("task_add failed for the session's own child: %s", text)
	}
	if n := mcpLedgerCount(t, ledger, "u-owner"); n != 1 {
		t.Fatalf("child A's task_add did not reach the ledger: %d rows", n)
	}
}
