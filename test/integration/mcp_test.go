// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end tests for the MCP agent-control surface (POST /mcp inside the
// proxy face). Everything here drives a REAL daemon subprocess over real HTTP:
// the auth middleware, the per-request server construction and the tool
// implementations are unit-tested elsewhere; only the seam between them is
// exercised here.
//
// Two rules from this package, restated because they bite exactly here:
//   - every run must pass -count=1, because go test caching cannot see through
//     to a daemon subprocess;
//   - a proxy-listener failure surfaces as a missing connect.sock / dead MCP
//     endpoint rather than a meaningful error, so every fatal below carries the
//     daemon's captured stderr.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// mcpToolNames is the exact surface the MCP face must expose: the twelve
// agent-control blueprints (cmd/rafikid/mcp_face.go's mcpBlueprints), no more
// and no fewer. ListTools must agree with this list exactly.
var mcpToolNames = []string{
	"agent_spawn",
	"agent_list",
	"agent_view",
	"agent_send",
	"agent_kill",
	"agent_set_budget",
	"agent_models",
	"task_add",
	"task_update",
	"task_drop",
	"task_list",
	"quota_status",
}

// ─── harness: daemon with a known proxy port ─────────────────────────────────

// bootMCPDaemon boots the DB-backed daemon the MCP tests drive, over the
// shared bootDaemonDB path: migrations, an ephemeral loopback proxy port,
// stderr capture, and both readiness waits (control UDS accepting, proxy face
// announcing its resolved port on stderr — read back as d.proxyURL). What
// remains here is this file's business, not harness material: noRealProviderEnv,
// so a spawned child's turn can never reach (or spend against) a real
// provider, and removal of the state dir on cleanup, which bootDaemonDB
// deliberately skips so its restart tests can come back against the same tree.
func bootMCPDaemon(t *testing.T) *daemon {
	t.Helper()
	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	t.Cleanup(func() {
		// Stop first, then remove: this cleanup runs before bootDaemonDB's own
		// (LIFO), and stopping an already-stopped daemon is a no-op.
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})
	return d
}

// createMCPUser mints a user over the control UDS and returns its bearer
// token. The UDS is the daemon's trust boundary, so the frame needs no
// credential; the token is what the MCP face authenticates with.
func (d *daemon) createMCPUser(t *testing.T) string {
	t.Helper()
	username := fmt.Sprintf("mcp-it-%d", time.Now().UnixNano())
	raw := d.request(t, fmt.Sprintf(`{"type":"ctrl_user_create","id":"u1","username":%q}`, username))
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_user_create failed: %+v", r.Error)
	}
	var data protocol.UserCreateResponseData
	mustUnmarshal(t, r.Data, &data)
	if data.Token == "" {
		t.Fatal("ctrl_user_create returned no token")
	}
	// The token is shown exactly once by design, so there is nothing to
	// re-derive later; remove the user on cleanup so the shared test database
	// does not accumulate active test identities.
	t.Cleanup(func() {
		raw := d.request(t, fmt.Sprintf(`{"type":"ctrl_user_rm","id":"u2","username":%q}`, username))
		var rm protocol.Response
		if json.Unmarshal(raw, &rm) == nil && !rm.Success {
			t.Logf("ctrl_user_rm(%s): %+v", username, rm.Error)
		}
	})
	return data.Token
}

// ─── MCP client plumbing ─────────────────────────────────────────────────────

// mcpBearerTransport injects the caller's token on every request — the same
// pattern pkg/fundi/tools/mcp.go's headerRoundTripper uses, because
// StreamableClientTransport has no headers field of its own.
type mcpBearerTransport struct {
	token string
}

func (t mcpBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpConnect opens an MCP session over streamable HTTP against the daemon's
// /mcp mount. No http.Client.Timeout: the client holds a long-lived standalone
// SSE stream open, which a whole-client timeout would kill.
func mcpConnect(t *testing.T, proxyURL, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   proxyURL + "/mcp",
		HTTPClient: &http.Client{Transport: mcpBearerTransport{token: token}},
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "mcp-it", Version: "0.0.1"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// mcpCallTool invokes one tool and returns its flattened text along with the
// result, so callers can assert on IsError themselves (the MCP contract makes
// a tool failure a result with IsError, never a transport error).
func mcpCallTool(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	if res == nil {
		t.Fatalf("%s: nil result", name)
	}
	return res, mcpResultText(res)
}

// mcpOK calls a tool and fails the test if the tool reported an error,
// returning the result text on success.
func mcpOK(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, text := mcpCallTool(t, sess, name, args)
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, text)
	}
	return text
}

func mcpResultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// ─── the tests ───────────────────────────────────────────────────────────────

// TestMCPSurfaceEndToEnd drives the whole agent-control surface over real
// streamable HTTP against a real daemon: list the exact tool set, spawn a
// child through MCP, confirm it through the EXISTING Connect control plane
// (a surface that agrees with itself proves nothing), walk every read/steer
// verb and the task ledger, assert the parented-child budget refusal, and
// finish with a kill that reaches exited.
func TestMCPSurfaceEndToEnd(t *testing.T) {
	t.Parallel()
	d := bootMCPDaemon(t)
	token := d.createMCPUser(t)
	sess := mcpConnect(t, d.proxyURL, token)

	// 3. ListTools must be the exact twelve.
	list, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make([]string, 0, len(list.Tools))
	for _, tl := range list.Tools {
		got = append(got, tl.Name)
	}
	slices.Sort(got)
	want := slices.Clone(mcpToolNames)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("ListTools = %v, want exactly %v", got, want)
	}

	// 4. agent_spawn with an explicit absolute cwd. The model is the rest of
	// this suite's throwaway string: the daemon requires a resolvable provider
	// at spawn, and noRealProviderEnv blanks every key, so the child's one
	// turn fails fast at an upstream 401 without spending anything.
	spawnText := mcpOK(t, sess, "agent_spawn", map[string]any{
		"prompt": "One sentence on what 2+2 is, then stop. Integration-test transcript filler.",
		"name":   "mcp-e2e",
		"cwd":    "/tmp",
		"model":  "anthropic/sonnet-latest",
		"kind":   "fundi",
	})
	childID := mcpFirstAgentID(t, spawnText)
	if childID == "" {
		t.Fatalf("agent_spawn returned no agent id; output:\n%s", spawnText)
	}

	// Cross-check through the EXISTING Connect control plane over the UDS.
	confirmChild := func(wantStatus string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resp, err := d.connectClient().ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
		if err != nil {
			t.Fatalf("ListChildren: %v", err)
		}
		for _, c := range resp.Msg.GetChildren() {
			if c.GetChildId() != childID {
				continue
			}
			if wantStatus == "" && c.GetStatus() == "exited" {
				t.Fatalf("child %s unexpectedly exited before the kill step", childID)
			}
			if wantStatus != "" && c.GetStatus() != wantStatus {
				t.Fatalf("child %s status = %q, want %q", childID, c.GetStatus(), wantStatus)
			}
			if c.GetKind() != "fundi" || c.GetName() != "mcp-e2e" {
				t.Errorf("child %s = kind %q name %q, want fundi/mcp-e2e", childID, c.GetKind(), c.GetName())
			}
			return
		}
		t.Fatalf("child %s missing from Connect ListChildren", childID)
	}
	confirmChild("")

	// 5. Every read/steer verb and the task ledger, in order.
	listText := mcpOK(t, sess, "agent_list", nil)
	if !strings.Contains(listText, childID) {
		t.Fatalf("agent_list missing child %s; output:\n%s", childID, listText)
	}

	viewText := mcpOK(t, sess, "agent_view", map[string]any{"agent": childID})
	if strings.TrimSpace(viewText) == "" {
		t.Fatal("agent_view returned an empty transcript")
	}

	sendText := mcpOK(t, sess, "agent_send", map[string]any{"agent": childID, "message": "integration-test steer"})
	if !strings.Contains(sendText, "delivered") {
		t.Fatalf("agent_send: unexpected output %q", sendText)
	}

	mcpOK(t, sess, "agent_models", nil)
	mcpOK(t, sess, "quota_status", nil)

	addText := mcpOK(t, sess, "task_add", map[string]any{
		"items": []map[string]any{{"content": "Exercise the MCP surface", "active_form": "Exercising the MCP surface"}},
	})
	handle := mcpFirstTaskHandle(t, addText)
	if handle == "" {
		t.Fatalf("task_add returned no task handle; output:\n%s", addText)
	}
	listTasks := mcpOK(t, sess, "task_list", nil)
	if !strings.Contains(listTasks, handle) {
		t.Fatalf("task_list missing handle %s; output:\n%s", handle, listTasks)
	}
	updText := mcpOK(t, sess, "task_update", map[string]any{
		"changes": []map[string]any{{"handle": handle, "status": "completed"}},
	})
	if !strings.Contains(updText, handle) {
		t.Fatalf("task_update output missing handle %s; output:\n%s", handle, updText)
	}
	dropText := mcpOK(t, sess, "task_drop", map[string]any{
		"handle": handle,
		"reason": "integration test finished with it",
	})
	if !strings.Contains(dropText, "dropped") {
		t.Fatalf("task_drop output does not show a dropped task; output:\n%s", dropText)
	}

	// 6. Budgets: a top-level child's budget is the MCP caller's to change; a
	// parented child's belongs to the agent that spawned it, and the refusal
	// must name that parent.
	budgetText := mcpOK(t, sess, "agent_set_budget", map[string]any{"agent": childID, "max_cost": 5.0})
	if !strings.Contains(budgetText, "5.00") {
		t.Fatalf("agent_set_budget: unexpected output %q", budgetText)
	}

	kidID := d.spawnChildUnder(t, childID)
	res, kidText := mcpCallTool(t, sess, "agent_set_budget", map[string]any{"agent": kidID, "max_cost": 5.0})
	if !res.IsError {
		t.Fatalf("agent_set_budget on parented child %s succeeded; want IsError; output:\n%s", kidID, kidText)
	}
	if !strings.Contains(kidText, "was spawned by agent "+childID) {
		t.Fatalf("agent_set_budget refusal does not name the parent %s; output:\n%s", childID, kidText)
	}

	// 7. agent_kill waits for the shutdown to be recorded; the exit is then
	// confirmed through the Connect plane, not through MCP.
	killText := mcpOK(t, sess, "agent_kill", map[string]any{"agent": childID})
	if !strings.Contains(killText, childID) {
		t.Fatalf("agent_kill: unexpected output %q", killText)
	}
	confirmChild("exited")
}

// mcpFirstAgentID extracts the agent id from RenderAgents output: the id is
// the first field of the first agent line (line two; line one is the count).
func mcpFirstAgentID(t *testing.T, text string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) < 2 {
		return ""
	}
	fields := strings.Fields(lines[1])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// mcpFirstTaskHandle extracts the first task handle from tasks.Render output,
// whose per-task lines begin with the handle.
func mcpFirstTaskHandle(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "☐") || strings.Contains(line, "●") || strings.Contains(line, "✓") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

// TestMCPSurfaceRejectsAMissingToken proves the face's authentication is the
// proxy face's own middleware and not something the MCP handler does later:
// with no credential, and with a garbage credential, the initialize request is
// answered 401 by the HTTP layer, the response is not a stream and carries no
// JSON-RPC payload — so no server object was ever built and no tool ran.
func TestMCPSurfaceRejectsAMissingToken(t *testing.T) {
	t.Parallel()
	d := bootMCPDaemon(t)

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-it","version":"0"}}}`
	for name, token := range map[string]string{"no credential": "", "garbage credential": "mcp-it-not-a-real-token"} {
		resp, body := mcpRawBody(t, d.proxyURL, token, "", initBody)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: initialize status = %d, want 401 (body: %.200s)", name, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("%s: rejected request answered with %q; an unauthenticated caller must never get a stream", name, ct)
		}
		if strings.Contains(body, `"result"`) {
			t.Errorf("%s: 401 body carries a JSON-RPC result; no tool may run without a credential (body: %.200s)", name, body)
		}
	}

	// The go-sdk client on the same credentials must fail to establish a
	// session at all, which is what a real MCP caller would experience.
	for name, token := range map[string]string{"no credential": "", "garbage credential": "mcp-it-not-a-real-token"} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		transport := &mcp.StreamableClientTransport{
			Endpoint:   d.proxyURL + "/mcp",
			HTTPClient: &http.Client{Transport: mcpBearerTransport{token: token}},
		}
		_, err := mcp.NewClient(&mcp.Implementation{Name: "mcp-it", Version: "0.0.1"}, nil).Connect(ctx, transport, nil)
		cancel()
		if err == nil {
			t.Errorf("%s: mcp client established a session without a valid credential", name)
		}
	}
}

// sseLegTimeout bounds how long a server→client SSE message may take. The
// daemon flushes each event as it is written; anything approaching this bound
// means the flush is lost, not slow.
const sseLegTimeout = 15 * time.Second

// TestMCPSurfaceFlushesTheSSELeg proves the SSE leg actually flushes, with a
// raw streamable-HTTP client (no SDK) so the wire is visible.
//
// What this actually proves: the daemon answers a POST carrying a JSON-RPC
// call with Content-Type text/event-stream, and the JSON-RPC response is
// delivered as an SSE data frame within a bounded wall-clock budget — for both
// initialize and a tools/list on the same session. A lost flush has NO error
// signature anywhere (the handler succeeds, the bytes sit in a buffer), and
// reads exactly as a client that never receives anything, which is why the
// assertion is on receipt time rather than on handler internals. It does not
// prove anything about server-initiated pushes on the standalone GET stream:
// this surface currently has no server→client notification it can produce on
// demand, so the POST response stream is the leg that can be exercised.
func TestMCPSurfaceFlushesTheSSELeg(t *testing.T) {
	t.Parallel()
	d := bootMCPDaemon(t)
	token := d.createMCPUser(t)

	// initialize — the response names the session and carries the result as a
	// flushed SSE data frame.
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-it-raw","version":"0"}}}`
	resp := mcpRawPost(t, d.proxyURL, token, "", initBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("initialize Content-Type = %q, want text/event-stream", ct)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize response carried no Mcp-Session-Id")
	}
	mcpReadSSEData(t, resp, 1)

	// The client's initialized notification (no call in it) is accepted
	// out-of-band with 202 and no stream.
	notifResp := mcpRawPost(t, d.proxyURL, token, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_, _ = io.Copy(io.Discard, notifResp.Body)
	_ = notifResp.Body.Close()
	if notifResp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized status = %d, want 202", notifResp.StatusCode)
	}

	// tools/list — its response must arrive over the stream within the bound.
	resp = mcpRawPost(t, d.proxyURL, token, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200", resp.StatusCode)
	}
	data := mcpReadSSEData(t, resp, 2)
	var payload struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode tools/list SSE frame: %v\ndata: %s", err, data)
	}
	if len(payload.Result.Tools) != len(mcpToolNames) {
		t.Fatalf("tools/list over the SSE leg returned %d tools, want %d", len(payload.Result.Tools), len(mcpToolNames))
	}
}

// mcpRawPost issues one raw streamable-HTTP POST against the /mcp mount and
// returns the response with its body UNREAD, so a streaming caller reads it as
// an SSE stream as it arrives. The caller closes the body.
func mcpRawPost(t *testing.T, proxyURL, token, sessionID, body string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sseLegTimeout)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	return resp
}

// mcpRawBody is mcpRawPost for callers that want the whole body (auth
// rejections, non-streaming responses) and closes it.
func mcpRawBody(t *testing.T, proxyURL, token, sessionID, body string) (*http.Response, string) {
	t.Helper()
	resp := mcpRawPost(t, proxyURL, token, sessionID, body)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, string(raw)
}

// mcpReadSSEData reads the response as an SSE stream to its end and returns
// the first data frame whose JSON-RPC id matches wantID, failing if none
// arrives before the stream (or its context) ends. Receipt time is measured
// from the start of the read: a lost flush shows up here as a timeout, not as
// an error from the handler. Reading to EOF (the server closes the POST
// response stream once its responses are delivered) also lets the daemon's own
// handler return normally, which is what records the session binding on the
// face.
func mcpReadSSEData(t *testing.T, resp *http.Response, wantID float64) json.RawMessage {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	start := time.Now()
	var found json.RawMessage
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok || found != nil {
			continue
		}
		data = strings.TrimSpace(data)
		var msg map[string]any
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			continue // SSE comments and keep-alives
		}
		if id, ok := msg["id"].(float64); ok && id == wantID {
			found = json.RawMessage(data)
			t.Logf("SSE data frame for id %v received after %s", wantID, time.Since(start).Round(time.Millisecond))
		}
	}
	if found != nil {
		return found
	}
	err := scanner.Err()
	if err == nil {
		err = fmt.Errorf("stream ended without a data frame for id %v", wantID)
	}
	if ctxErr := resp.Request.Context().Err(); ctxErr != nil {
		err = fmt.Errorf("%v (after %s; a lost flush reads as a client that never receives anything)", err, time.Since(start).Round(time.Millisecond))
	}
	t.Fatalf("reading SSE stream: %v", err)
	return nil
}
