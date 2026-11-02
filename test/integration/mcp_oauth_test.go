// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end and adversarial tests for daemon-brokered MCP OAuth.
//
// What only these tests can prove, because they drive a real rafikid, the real
// `rafiki` CLI, a real fundi child and a real MCP SDK session against a real
// (fake) authorization server:
//
//   - an explicit login through the CLI stores a grant the daemon's own fundi
//     runtime then SPENDS on a real tool invocation — the bearer on the wire is
//     the persisted access token, not a token source a test inspected;
//   - after the access token expires the daemon refreshes once, with the RFC
//     8707 resource and the registration it persisted, rotates the single-use
//     refresh token, and the NEXT tool call presents the rotation — read from
//     the database, not from memory;
//   - a logout means the next acquisition fails, the child gets the
//     `rafiki mcp login` hint, and an explicit later login restores use;
//   - no token ever crosses the wire to a client, a CLI process, a child's
//     argv/environment/staged config, or a discovery/registration request;
//   - the exact-endpoint binding cannot be borrowed by another endpoint, by a
//     metadata claim, or by an alias naming the same host;
//   - foreign owners, an admin caller and ownerless credentials get nothing;
//   - a store outage refuses the dial instead of going anonymous, and a
//     refresh, a logout, a user removal and an older login are fenced by
//     persisted generations under real row locks (with TWO independent pools).
//
// Same package rules as the other MCP tests: always -count=1 (go test caching
// cannot see through to a daemon subprocess), and every fatal on a daemon
// interaction carries the daemon's captured stderr.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// ─── shared helpers ─────────────────────────────────────────────────────────

// fakeTurnsToolCalls scripts n prompts, each answering with one tool call
// followed by a clean end_turn. One prompt consumes exactly two scripted
// responses, so this is the queue a child walks as it is prompted n times.
func fakeTurnsToolCalls(t *testing.T, tool string, n int) string {
	t.Helper()
	c := assert.NewAborting(t)
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines,
			fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":"claude-x","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu_%d","name":%q,"input":{}}],"usage":{"input_tokens":10,"output_tokens":5}}`, i*2+1, i, tool),
			fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"done %d"}],"usage":{"input_tokens":4,"output_tokens":2}}`, i*2+2, i),
		)
	}
	path := filepath.Join(t.TempDir(), "fake-turns.ndjson")
	c.NoError(os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600), "write fake-turns script")
	return path
}

// spawnFundiOwningMCP spawns a fundi child AS the user whose token it is given,
// with the MCP config under test and the scripted turns. The user ownership is
// the whole point: the daemon binds the credential factory to the credential
// owner, and an ownerless (bare socket) spawn would get no factory at all.
func spawnFundiOwningMCP(t *testing.T, d *daemon, token, cfgPath, script string) string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := faceClient(t, d, token).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:       t.TempDir(),
		Kind:      protocol.KindFundi,
		Model:     "anthropic/claude-x",
		McpConfig: cfgPath,
		ExtraArgs: []string{"--fake-turns", script},
	}))
	c.NoError(err, "spawn with the OAuth MCP config\nstderr: %s", d.stderr.tail(4000))
	c.NotEq("", resp.Msg.GetChildId(), "spawn returned no child id")
	return resp.Msg.GetChildId()
}

// sendChildPrompt delivers one prompt frame to a child.
func sendChildPrompt(t *testing.T, client rafikiv1connect.ControlClient, childID, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := client.SendFrame(ctx, connect.NewRequest(&rafikiv1.SendFrameRequest{
		ChildId:   childID,
		FrameJson: fmt.Sprintf(`{"type":"prompt","message":%q}`, text),
	}))
	assert.NewAborting(t).NoError(err, "prompt child %s", childID)
}

// expireGrantInDB moves a grant's stored access expiry into the past, which is
// the deterministic way to force the store's refresh path: `access_expiry` is a
// plain column, and the store re-reads it under its row lock on EVERY
// acquisition. The sealed credential blob is untouched — the access token
// itself is still the one the authorization server issued, so a request after
// this can only succeed by refreshing.
func expireGrantInDB(t *testing.T, resource string) {
	t.Helper()
	c := assert.NewAborting(t)
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()
	tag, err := pool.Exec(context.Background(),
		`UPDATE conversations.mcp_credential
		    SET access_expiry = now() - interval '1 minute'
		  WHERE resource = $1 AND invalidated_at IS NULL`, resource)
	c.NoError(err, "expire the stored access token")
	c.Eq(int64(1), tag.RowsAffected(), "expire the stored access token: rows matched (resource %s)", resource)
}

// toolCallCount is how many real `tools/call <name>` invocations the fixture's
// MCP endpoint has served.
func toolCallCount(fx *fakeMCPOAuth, name string) int {
	n := 0
	for _, m := range fx.mcpMethodLog() {
		if m == "tools/call "+name {
			n++
		}
	}
	return n
}

// mcpLoginCLI runs `rafiki mcp login` against the daemon with --no-browser and
// plays the browser: it waits for the consent URL on stderr, asks the fake
// authorization server for the redirect, and delivers that redirect to the
// CLI's loopback listener. It returns the process's stdout and stderr.
//
// The listener is bound BEFORE the URL is printed (loopbackCallback binds, then
// announces), so the redirect can be delivered the moment the URL appears with
// no race against a half-started server. A second round (a continuation) prints
// a second URL and is served the same way.
func mcpLoginCLI(t *testing.T, d *daemon, fx *fakeMCPOAuth, configDir, server, cfgPath string) (string, string) {
	t.Helper()
	c := assert.NewAborting(t)
	cmd := cliCmdIn(t, d, configDir, "mcp", "login", server, "--mcp-config", cfgPath, "--no-browser")
	// stdin EOF retires the paste path; the listener is what answers here.
	cmd.Stdin = strings.NewReader("")
	var out strings.Builder
	cmd.Stdout = &out
	sink := &stderrBuf{}
	pr, pw := io.Pipe()
	cmd.Stderr = io.MultiWriter(sink, pw)
	c.NoError(cmd.Start(), "start `rafiki mcp login`")

	consent := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
				select {
				case consent <- line:
				default:
				}
			}
		}
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case authorize := <-consent:
			location := fx.browse(t, authorize)
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(location.String())
			if err != nil {
				_ = cmd.Process.Kill()
				_ = pw.Close()
				t.Fatalf("deliver the callback to the CLI listener: %v\nstderr:\n%s", err, sink.String())
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		case err := <-waited:
			_ = pw.Close()
			if err != nil {
				t.Fatalf("`rafiki mcp login` failed: %v\nstderr:\n%s", err, sink.String())
			}
			return out.String(), sink.String()
		case <-time.After(time.Until(deadline)):
			_ = cmd.Process.Kill()
			_ = pw.Close()
			t.Fatalf("`rafiki mcp login` did not finish within 60s\nstderr:\n%s", sink.String())
		}
	}
}

// assertNoSecret fails when any of secrets appears in any of the strings.
func assertNoSecret(t *testing.T, what string, secrets []string, haystacks ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, hay := range haystacks {
			if strings.Contains(hay, secret) {
				t.Fatalf("%s carries the credential %q; it must never leave the daemon", what, secret)
			}
		}
	}
}

// ─── the end-to-end test ────────────────────────────────────────────────────

// TestMcpOAuthEndToEnd is the whole feature over the wire and through a real
// child: DCR → Begin URL → state/iss callback → Complete (verifying tools) → a
// fundi tool call carrying the persisted access token → expiry → one rotation
// → the NEXT call presenting the rotation → logout → a failed acquisition plus
// the passive re-login hint on a new spawn → an explicit later login restoring
// use.
func TestMcpOAuthEndToEnd(t *testing.T) {
	c := assert.NewAborting(t)
	fx := newFakeMCPOAuth(t)
	d := bootMCPOAuthDaemon(t)
	token, configDir := scriptUser(t, d)
	cfgPath := writeMCPJSON(t, map[string]any{
		"fixture": map[string]any{"url": fx.endpoint()},
	})

	// ── the explicit login, through the CLI ────────────────────────────────
	stdout, stderr := mcpLoginCLI(t, d, fx, configDir, "fixture", cfgPath)
	c.StrContains(stderr, "logged in to fixture", "login stderr:\n%s", stderr)
	c.StrContains(stderr, "resource "+fx.endpoint(), "the login names the resource it bound:\n%s", stderr)
	c.StrContains(stderr, "2 tool(s)", "the login verified the server's tools:\n%s", stderr)

	access, refresh := fx.issued()
	c.Eq(1, len(access), "one access token was issued, got %v", access)
	c.Eq(1, len(refresh), "one refresh token was issued, got %v", refresh)

	// No token, client secret or callback code reaches this process. The code
	// travelled only inside Complete's REQUEST, and the fixture still holds it.
	assertNoSecret(t, "the login's stdout", append(access, refresh...), stdout)
	assertNoSecret(t, "the login's stderr", append(access, refresh...), stderr)
	c.NotStrContains(stdout+stderr, "code-1", "the CLI printed the authorization code")

	// ── what the login asked the authorization server for ───────────────────
	dcr := fx.dcrBodySnapshot()
	c.Require().Eq(1, len(dcr), "dynamic client registrations")
	c.Eq("none", dcr[0]["token_endpoint_auth_method"], "DCR must register a public client")
	c.Eq("native", dcr[0]["application_type"], "DCR: a loopback redirect is a native client")
	c.Eq(fmt.Sprint([]any{"authorization_code", "refresh_token"}), fmt.Sprint(dcr[0]["grant_types"]), "DCR grant types")
	c.Eq(fmt.Sprint([]any{mcpoauth.CallbackURL}), fmt.Sprint(dcr[0]["redirect_uris"]), "DCR redirect URIs")

	beginParams := fx.authorizeParamsSnapshot()
	c.Require().Eq(1, len(beginParams), "authorization requests")
	c.Eq("S256", beginParams[0].Get("code_challenge_method"), "PKCE method on the consent URL")
	c.NotEq("", beginParams[0].Get("code_challenge"), "PKCE challenge on the consent URL")
	c.Eq("", beginParams[0].Get("code_verifier"), "the verifier must never be in the authorization request")
	c.NotEq("", beginParams[0].Get("state"), "state on the consent URL")
	c.Eq(fx.endpoint(), beginParams[0].Get("resource"), "RFC 8707 resource on the consent URL")
	c.Eq(mcpoauth.CallbackURL, beginParams[0].Get("redirect_uri"), "the fixed loopback redirect")
	c.StrContains(beginParams[0].Get("scope"), "read", "the requested scope carries the resource's advertised scope")

	exchange := fx.tokenFormSnapshot()
	c.Require().Eq(1, len(exchange), "token requests")
	c.Eq("authorization_code", exchange[0].Get("grant_type"))
	c.NotEq("", exchange[0].Get("code_verifier"), "PKCE verifier on the exchange")
	c.Eq(fx.endpoint(), exchange[0].Get("resource"), "RFC 8707 resource on the exchange")
	c.Eq("", exchange[0].Get("client_secret"), "a public client sends no client secret")

	// Nothing on the OAuth legs carried an Authorization header: a project's
	// configured headers never reach discovery, registration or the token
	// endpoint, and this client has no bearer of its own.
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/register", "/authorize", "/token"} {
		for _, h := range fx.headersOnPath(path) {
			c.Eq("", h.Get("Authorization"), "%s was sent an Authorization header", path)
		}
	}

	// ── a real tool call presents the persisted access token ────────────────
	client := faceClient(t, d, token)
	script := fakeTurnsToolCalls(t, "mcp__fixture__echo", 4)
	childID := spawnFundiOwningMCP(t, d, token, cfgPath, script)
	es := d.openEvents(t, childEventsSubject(childID), map[string]int32{childID: -1}, 0)

	sendChildPrompt(t, client, childID, "call one")
	es.waitEvent(t, userTextContains(childID, "echo ok"), 90*time.Second)
	c.Eq("Bearer "+access[0], fx.waitToolCall(t, "echo", 1, 30*time.Second),
		"the fundi tool call's bearer is not the token the login persisted")
	c.Eq(0, fx.refreshCount(), "the first tool call refreshed a credential it should have used")

	// ── expire the access token: one refresh, RFC 8707, rotated ─────────────
	expireGrantInDB(t, fx.endpoint())
	sendChildPrompt(t, client, childID, "call two")
	fx.waitToolCall(t, "echo", 2, 90*time.Second)

	access, refresh = fx.issued()
	c.Require().Eq(2, len(access), "the refresh issued no new access token: %v", access)
	c.Require().Eq(2, len(refresh), "the refresh issued no rotated refresh token: %v", refresh)
	refreshed := fx.refreshFormSnapshot()
	c.Require().Eq(1, len(refreshed), "refreshes after one expiry")
	c.Eq(refresh[0], refreshed[0].Get("refresh_token"), "the refresh spent a token other than the stored one")
	c.Eq(fx.endpoint(), refreshed[0].Get("resource"), "RFC 8707 resource on the refresh")
	c.Eq("", refreshed[0].Get("client_secret"), "the persisted registration is a public client")
	c.Eq("refresh_token", refreshed[0].Get("grant_type"))
	c.Eq(fx.bearerOnMCP()[len(fx.bearerOnMCP())-1], "Bearer "+access[1],
		"the request after the refresh did not present the rotated token")

	// ── a LATER request uses the persisted rotation, without refreshing ─────
	sendChildPrompt(t, client, childID, "call three")
	c.Eq("Bearer "+access[1], fx.waitToolCall(t, "echo", 3, 60*time.Second),
		"a later request did not use the persisted rotated token")
	c.Eq(1, fx.refreshCount(), "a later request refreshed a still-valid token")

	// ── logout: the next acquisition fails, and the child learns the hint ───
	logoutCmd := cliCmdIn(t, d, configDir, "mcp", "logout", "fixture", "--mcp-config", cfgPath)
	logoutOut, err := logoutCmd.CombinedOutput()
	c.NoError(err, "mcp logout: %v\n%s", err, logoutOut)
	c.StrContains(string(logoutOut), "logged out of fixture", "logout output")

	// The logout's upstream revocation is best effort and already attempted
	// (bounded, and never allowed to undo the local invalidation): the live
	// rotation is the token offered back to the authorization server.
	revoked := fx.waitRevocation(t, refresh[1], 5*time.Second)

	before := toolCallCount(fx, "echo")
	sendChildPrompt(t, client, childID, "call four")
	// The tool IS attempted and refused before any request leaves the daemon:
	// the token source now answers "needs a login" and the transport surfaces
	// that to the model instead of sending a request.
	es.waitEvent(t, userTextContains(childID, "needs a login: run `rafiki mcp login fixture`"), 90*time.Second)
	time.Sleep(500 * time.Millisecond)
	c.Eq(before, toolCallCount(fx, "echo"), "a tool call reached the server after logout")
	c.True(revoked, "the logout's best-effort revocation never reached the fixture")

	// ── a NEW spawn produces the passive re-login hint ─────────────────────
	_ = spawnFundiOwningMCP(t, d, token, cfgPath, fakeTurnsToolCalls(t, "mcp__fixture__echo", 1))
	waitStderrContains(t, d, "needs a login: run `rafiki mcp login fixture`", 60*time.Second)

	// ── an explicit later login restores use ───────────────────────────────
	_, stderr = mcpLoginCLI(t, d, fx, configDir, "fixture", cfgPath)
	c.StrContains(stderr, "logged in to fixture", "the second login's stderr:\n%s", stderr)
	access, refresh = fx.issued()
	c.Require().Eq(3, len(access), "the second login issued no token: %v", access)
	assertNoSecret(t, "the second login's stderr", append(access, refresh...), stderr)

	recovered := spawnFundiOwningMCP(t, d, token, cfgPath, fakeTurnsToolCalls(t, "mcp__fixture__echo", 1))
	recoveredEvents := d.openEvents(t, childEventsSubject(recovered), map[string]int32{recovered: -1}, 0)
	sendChildPrompt(t, client, recovered, "call five")
	recoveredEvents.waitEvent(t, userTextContains(recovered, "echo ok"), 90*time.Second)
	c.Eq("Bearer "+access[2], fx.waitToolCall(t, "echo", 4, 30*time.Second),
		"after the second login the child did not present the new token")
}

// ─── the wire descriptor and the callback contract ──────────────────────────

// TestMcpOAuthLoginDescriptorAndCallback pins the three fixed callback values,
// the owner binding, the one-identical-NotFound rule for unknown logins, and
// that a response carries no credential at all.
func TestMcpOAuthLoginDescriptorAndCallback(t *testing.T) {
	c := assert.NewAborting(t)
	fx := newFakeMCPOAuth(t)
	d := bootMCPOAuthDaemon(t)
	token := d.createMCPUser(t)
	client := faceClient(t, d, token)
	cfgPath := writeMCPJSON(t, map[string]any{"fixture": map[string]any{"url": fx.endpoint()}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	begin, err := client.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.NoError(err, "BeginMcpLogin")

	c.Eq(uint32(1456), begin.Msg.GetRedirectPort(), "the callback port is fixed")
	c.Eq("/mcp/oauth/callback", begin.Msg.GetCallbackPath(), "the callback path is fixed")
	c.Eq(fmt.Sprint([]string{"127.0.0.1"}), fmt.Sprint(begin.Msg.GetRedirectHosts()), "the callback hosts are loopback only")
	c.NotEq("", begin.Msg.GetLoginId(), "the descriptor carries a login id")
	c.NotStrContains(begin.Msg.GetAuthorizeUrl(), "code_verifier", "the authorize URL carries the PKCE verifier")

	authorize, err := url.Parse(begin.Msg.GetAuthorizeUrl())
	c.NoError(err, "authorize URL")
	c.Eq(fx.endpoint(), authorize.Query().Get("resource"), "resource on the consent URL")
	c.Eq("S256", authorize.Query().Get("code_challenge_method"), "PKCE method")

	// No response field carries a credential, by value: every string of the
	// message is checked against the token material the fixture holds.
	wire, err := protojson.Marshal(begin.Msg)
	c.NoError(err, "marshal the begin response")
	access, refresh := fx.issued()
	assertNoSecret(t, "the BeginMcpLogin response", append(access, refresh...), string(wire))

	// Unknown, bogus and (below) foreign login ids answer ONE identical NotFound.
	_, unknownErr := client.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: "no-such-login", CallbackQuery: "code=x&state=y",
	}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(unknownErr), "unknown login id: %v", unknownErr)

	// A wrong state is refused and the login is over — not left half-open.
	cb := url.Values{"code": {"code-1"}, "state": {"not-the-state"}}
	_, stateErr := client.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: begin.Msg.GetLoginId(), CallbackQuery: cb.Encode(),
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(stateErr), "wrong state: %v", stateErr)
	c.StrContains(stateErr.Error(), "start the login again", "a rejected consent round says the login is over: %v", stateErr)
	_, replayErr := client.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: begin.Msg.GetLoginId(), CallbackQuery: cb.Encode(),
	}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(replayErr), "the refused login must be gone: %v", replayErr)

	// A good round completes and reports sorted tool NAMES only.
	begin2, err := client.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.NoError(err, "second BeginMcpLogin")
	query := fx.callbackQuery(t, begin2.Msg.GetAuthorizeUrl())
	done, err := client.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: begin2.Msg.GetLoginId(), CallbackQuery: query,
	}))
	c.NoError(err, "CompleteMcpLogin")
	c.Nil(done.Msg.GetContinuation(), "unexpected continuation")
	c.Eq("fixture", done.Msg.GetServer(), "the daemon names the verified server")
	c.Eq(fx.endpoint(), done.Msg.GetResource(), "the resource the login bound")
	c.Eq(fmt.Sprint([]string{"echo", "whoami"}), fmt.Sprint(done.Msg.GetTools()), "the verified tool names, sorted")
	c.Eq("", done.Msg.GetAccountId(), "no validated source exists for an account id")

	doneWire, err := protojson.Marshal(done.Msg)
	c.NoError(err, "marshal the complete response")
	access, refresh = fx.issued()
	assertNoSecret(t, "the CompleteMcpLogin response", append(access, refresh...), string(doneWire))
	c.NotStrContains(string(doneWire), "code-1", "the Complete response echoed the callback code")
	c.NotStrContains(d.stderr.String(), "code-1", "the daemon logged the callback code")

	// The credential is sealed at rest: no column, and no part of the blob,
	// holds a usable token in plaintext.
	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()
	var blob, clientID, tokenEndpoint string
	err = pool.QueryRow(context.Background(), `
		SELECT encode(credential_sealed, 'escape'), client_id, token_endpoint
		  FROM conversations.mcp_credential WHERE resource = $1`, fx.endpoint()).Scan(&blob, &clientID, &tokenEndpoint)
	c.NoError(err, "read the stored credential row")
	assertNoSecret(t, "the sealed credential column", append(access, refresh...), blob)
	c.NotEq("", clientID, "the row carries the public client id")
	c.Eq(fx.srv.URL+"/token", tokenEndpoint, "the row carries the validated token endpoint")
}

// ─── ownership: foreign callers, an admin, ownerless and child credentials ───

// TestMcpOAuthOwnerIsolation proves the grant is the CALLER's, on every verb:
// another user's login id answers the same NotFound as a nonexistent one, an
// admin caller gets only its own grants, and the credentials that carry no user
// identity (anonymous, a bare loopback socket, a notify-scoped token) get
// nothing at all.
func TestMcpOAuthOwnerIsolation(t *testing.T) {
	c := assert.NewAborting(t)
	fx := newFakeMCPOAuth(t)
	d := bootMCPOAuthDaemon(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	pool := openPool(t, dsn)
	t.Cleanup(pool.Close)

	userA := "mcp-oauth-a-" + uniqueSuffix()
	_, tokenA := createRecallUser(t, pool, userA, false)
	_, tokenB := createRecallUser(t, pool, "mcp-oauth-b-"+uniqueSuffix(), false)
	_, tokenAdmin := createRecallUser(t, pool, "mcp-oauth-admin-"+uniqueSuffix(), true)

	cfgPath := writeMCPJSON(t, map[string]any{"fixture": map[string]any{"url": fx.endpoint()}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	faceA, faceB := faceClient(t, d, tokenA), faceClient(t, d, tokenB)

	begin, err := faceA.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.NoError(err, "A begins a login")

	// B cannot finish A's login, and cannot tell it apart from one that never
	// existed: one identical NotFound for both.
	_, foreign := faceB.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: begin.Msg.GetLoginId(), CallbackQuery: "code-1&state=x",
	}))
	_, bogus := faceB.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: "never-existed", CallbackQuery: "code-1&state=x",
	}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(foreign), "a foreign login id: %v", foreign)
	c.Eq(connect.CodeNotFound, connect.CodeOf(bogus), "an unknown login id: %v", bogus)
	c.Eq(bogus.Error(), foreign.Error(), "a foreign login id must be indistinguishable from an unknown one")

	// A finishes it.
	_, err = faceA.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: begin.Msg.GetLoginId(), CallbackQuery: fx.callbackQuery(t, begin.Msg.GetAuthorizeUrl()),
	}))
	c.NoError(err, "A completes its own login")

	// B cannot remove A's grant, by resource...
	_, err = faceB.DeleteMcpCredential(ctx, connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{
		Resource: fx.endpoint(),
	}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "B deleting A's grant: %v", err)

	// ... and neither can an admin. The owner is derived from the connection,
	// never named in the request, so an admin holds only its own grants.
	faceAdmin := faceClient(t, d, tokenAdmin)
	_, err = faceAdmin.DeleteMcpCredential(ctx, connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{
		Resource: fx.endpoint(),
	}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "an admin deleting another user's grant: %v", err)

	// The admin's own login works and is separate: removing it leaves A's.
	adminBegin, err := faceAdmin.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.NoError(err, "the admin begins its own login")
	_, err = faceAdmin.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: adminBegin.Msg.GetLoginId(), CallbackQuery: fx.callbackQuery(t, adminBegin.Msg.GetAuthorizeUrl()),
	}))
	c.NoError(err, "the admin completes its own login")
	_, err = faceAdmin.DeleteMcpCredential(ctx, connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{
		Resource: fx.endpoint(),
	}))
	c.NoError(err, "the admin removes its own grant")
	_, err = faceA.DeleteMcpCredential(ctx, connect.NewRequest(&rafikiv1.DeleteMcpCredentialRequest{
		Resource: fx.endpoint(),
	}))
	c.NoError(err, "A's grant survived another owner's login and logout")

	// Credentials with no user identity reach none of the three verbs.
	anonymous := rafikiv1connect.NewControlClient(
		&http.Client{Transport: epochTransport{base: http.DefaultTransport}}, d.proxyURL)
	_, err = anonymous.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.Eq(connect.CodeUnauthenticated, connect.CodeOf(err), "an anonymous caller on the proxy face: %v", err)

	_, err = d.connectClient().BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "a bare loopback-socket identity: %v", err)

	notifyToken := tokenLine(t, rafikidUser(t, dsn, "token", "mint", userA,
		"--capability", "notify", "--name", "mcp-oauth-notify-"+uniqueSuffix()))
	_, err = faceClient(t, d, notifyToken).BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "a notify-scoped credential: %v", err)
}

// ─── no credential reaches a child process ─────────────────────────────────

// TestMcpOAuthNoBearerReachesAChild drives a CLAUDE child (the fake-claude
// fixture, which records its own launch) against a config naming an
// OAuth-backed server while a grant exists, and asserts the access and refresh
// tokens appear in neither the child's argv, its environment, nor the staged
// MCP config file. A claude child is the strongest case: its config is
// materialized into a file on disk and its argv is world-readable via `ps`.
func TestMcpOAuthNoBearerReachesAChild(t *testing.T) {
	c := assert.NewAborting(t)
	fx := newFakeMCPOAuth(t)
	dumps := t.TempDir()
	d := bootMCPOAuthDaemon(t,
		"CLAUDE_BINARY="+fakeClaudeBin(t),
		"FAKE_CLAUDE_DIR="+dumps,
	)
	token := d.createMCPUser(t)
	cfgPath := writeMCPJSON(t, map[string]any{"fixture": map[string]any{"url": fx.endpoint()}})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	face := faceClient(t, d, token)
	begin, err := face.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: cfgPath, Server: "fixture",
	}))
	c.NoError(err, "BeginMcpLogin")
	_, err = face.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
		LoginId: begin.Msg.GetLoginId(), CallbackQuery: fx.callbackQuery(t, begin.Msg.GetAuthorizeUrl()),
	}))
	c.NoError(err, "CompleteMcpLogin")
	access, refresh := fx.issued()
	c.Require().Eq(1, len(access), "the login issued a token")

	resp, err := face.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:       t.TempDir(),
		Kind:      protocol.KindClaude,
		Model:     "anthropic/sonnet-latest",
		McpConfig: cfgPath,
	}))
	c.NoError(err, "spawn a claude child\nstderr: %s", d.stderr.tail(4000))
	childID := resp.Msg.GetChildId()
	c.Require().NotEq("", childID, "spawn returned no child id")

	dump := waitClaudeDump(t, d, dumps, childID)
	secrets := append(slices.Clone(access), refresh...)
	assertNoSecret(t, "the claude child's argv", secrets, strings.Join(dump.argv, "\n"))
	assertNoSecret(t, "the claude child's environment", secrets, strings.Join(dump.env, "\n"))

	var mcpPath string
	for _, a := range dump.argv {
		if v, ok := strings.CutPrefix(a, "--mcp-config="); ok {
			mcpPath = v
		}
	}
	c.Require().NotEq("", mcpPath, "the claude child's argv carries no --mcp-config")
	staged, err := os.ReadFile(mcpPath)
	c.NoError(err, "read the staged MCP config %s", mcpPath)
	assertNoSecret(t, "the claude child's staged MCP config", secrets, string(staged))
}

// ─── via_executor never receives a stored credential ───────────────────────

// TestMcpOAuthViaExecutorIsRefusedBeforeAnyRequest covers both halves of the
// via_executor rule: the daemon refuses a login for such an entry before any
// network work (and the CLI says so), and the fundi runtime never consults the
// credential factory for one — so no stored bearer can ever be attached.
func TestMcpOAuthViaExecutorIsRefusedBeforeAnyRequest(t *testing.T) {
	c := assert.NewAborting(t)
	fx := newFakeMCPOAuth(t)
	d := bootMCPOAuthDaemon(t)
	token := d.createMCPUser(t)
	face := faceClient(t, d, token)

	cfgPath := writeMCPJSON(t, map[string]any{
		"exec_cmd": map[string]any{
			"via_executor": map[string]any{"selector": "env=home", "command": "fixture"},
		},
		"exec_proxy": map[string]any{
			"via_executor": map[string]any{"selector": "env=home", "proxy": "fixture-http"},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, server := range []string{"exec_cmd", "exec_proxy"} {
		_, err := face.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
			ConfigPath: cfgPath, Server: server,
		}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "BeginMcpLogin(%s): %v", server, err)
		c.StrContains(err.Error(), "via_executor", "the refusal names the shape it refused: %v", err)
	}
	c.Eq(0, len(fx.requestLog()), "a refused via_executor login reached the network: %v", fx.requestLog())

	// The CLI reports the same refusal, and its logout form points at --resource
	// (the way to remove a grant without the config file).
	configDir := t.TempDir()
	writeCliProfile(t, configDir, d.socketPath)
	writeProfileToken(t, configDir, token)
	logout := cliCmdIn(t, d, configDir, "mcp", "logout", "exec_cmd", "--mcp-config", cfgPath)
	out, err := logout.CombinedOutput()
	c.Error(err, "logging out a via_executor entry must fail; output: %s", out)
	c.StrContains(string(out), "via_executor", "the CLI refusal names the shape: %s", out)
	c.StrContains(string(out), "--resource", "the logout refusal offers the config-free escape: %s", out)

	// The runtime half: ConnectMCP must not consult the credential factory for
	// either via_executor shape. The dialer is what serves them, and it refuses
	// here because nothing is enrolled on this host.
	cfg, err := tools.LoadMCPConfig(cfgPath)
	c.NoError(err, "LoadMCPConfig")
	factoryCalls := 0
	factory := func(context.Context, string, string, map[string]string) (sdkauth.OAuthHandler, *http.Client, error) {
		factoryCalls++
		return nil, nil, errors.New("the credential factory must not be consulted for a via_executor entry")
	}
	dial := func(context.Context, string, tools.MCPServerConfig) (mcp.Transport, error) {
		return nil, errors.New("fixture: no executor is enrolled")
	}
	registry := tools.NewRegistry()
	shutdown, err := tools.ConnectMCP(ctx, registry, cfg, tools.OutputPolicy{}, nil, dial, factory)
	c.NoError(err, "ConnectMCP")
	defer shutdown()
	c.Eq(0, factoryCalls, "the credential factory was consulted for a via_executor entry")
	c.Eq(0, len(registry.Definitions()), "a via_executor entry produced tools without an executor: %v", registry.Definitions())
}

// ─── exact endpoint binding ────────────────────────────────────────────────

// TestMcpOAuthEndpointBindingsAreExact is the binding rule: a passive dial uses
// the exact canonical endpoint's stored binding, never a resource derived from
// the endpoint's origin, and a differently-spelled endpoint of the same origin
// cannot borrow the credential.
func TestMcpOAuthEndpointBindingsAreExact(t *testing.T) {
	t.Run("same alias, different endpoint", func(t *testing.T) {
		c := assert.NewAborting(t)
		h := newMcpOAuthHarness(t)
		fx := newFakeMCPOAuth(t)
		res := h.login(fx, "fixture", fx.endpoint())
		c.Eq(fx.endpoint(), res.Resource, "the bound resource")

		other := fx.endpointAt("other")
		_, err := h.lookup(other)
		c.True(errors.Is(err, mcpoauth.ErrMissing), "an endpoint with no login must be ErrMissing, got %v", err)

		handler, client, err := h.passive(other, nil)
		c.NoError(err, "the passive factory still hands back a handler for a public server")
		ts, err := handler.TokenSource(context.Background())
		c.NoError(err, "TokenSource")
		c.Nil(ts, "an unbound endpoint was given a token source")

		// A real dial to the unbound endpoint presents NO bearer and gets the
		// named hint instead of a credential borrowed from the other endpoint.
		_, err = connectMCP(t, other, handler, client)
		c.Error(err, "the unbound endpoint accepted a session")
		c.StrContains(err.Error(), "server fixture needs a login: run `rafiki mcp login fixture`", "the hint")
		for _, bearer := range fx.bearerOnPath("/mcp/other") {
			if bearer != "" {
				t.Fatalf("a bearer reached the unbound endpoint: %q", bearer)
			}
		}
	})

	t.Run("endpoint is not its origin", func(t *testing.T) {
		c := assert.NewAborting(t)
		h := newMcpOAuthHarness(t)
		fx := newFakeMCPOAuth(t)
		fx.set(func(f *fakeMCPOAuth) { f.servePRMPath = false })

		res := h.login(fx, "fixture", fx.endpoint())
		root := fx.canonicalRoot(t)
		c.Eq(root, res.Resource, "the root metadata document's resource is what the login binds")
		c.NotEq(fx.endpoint(), res.Resource, "the resource is not the endpoint")

		// The ENDPOINT is what is bound, and a passive dial works.
		binding := h.binding(fx.endpoint())
		c.Eq(root, binding.Resource, "the binding names the resource")
		handler, client, err := h.passive(fx.endpoint(), nil)
		c.NoError(err, "passive")
		sess, err := connectMCP(t, fx.endpoint(), handler, client)
		c.NoError(err, "a passive dial through the bound endpoint")
		toolsList, err := sess.ListTools(context.Background(), nil)
		c.NoError(err, "ListTools")
		c.NotNil(toolsList)

		// The RESOURCE's spelling is not an endpoint: nothing is bound to it, so
		// it gets no credential — the daemon never substitutes one for the other.
		_, err = h.lookup(root)
		c.True(errors.Is(err, mcpoauth.ErrMissing), "the resource spelling must not resolve as an endpoint, got %v", err)
		rootHandler, _, err := h.passive(root, nil)
		c.NoError(err, "passive for the resource spelling")
		rootTS, err := rootHandler.TokenSource(context.Background())
		c.NoError(err, "TokenSource")
		c.Nil(rootTS, "the resource spelling was handed a token source")
	})
}

// TestMcpOAuthLogoutRemovesEveryGrantBinding covers both logout selectors at
// the generation level: a resource-form logout clears the bound endpoint AND
// every endpoint pending for that resource, an endpoint-form logout weeds a
// grant out from a second endpoint that merely shares its origin, and a newer
// login's grant is never silently adopted by an older endpoint binding.
func TestMcpOAuthLogoutRemovesEveryGrantBinding(t *testing.T) {
	c := assert.NewAborting(t)
	h := newMcpOAuthHarness(t)
	fx := newFakeMCPOAuth(t)
	// Both endpoints discover the ROOT as their resource, which is what makes
	// them two bindings of ONE grant rather than two grants.
	fx.set(func(f *fakeMCPOAuth) { f.servePRMPath = false })
	root := fx.canonicalRoot(t)

	res := h.login(fx, "fixture", fx.endpoint())
	c.Eq(root, res.Resource, "the shared resource")

	// A second login, begun but never completed: its fence is pending for the
	// same resource.
	pending := h.begin(fx, "fixture", fx.endpointAt("other"))

	// Logout by resource: the live binding goes, and the pending login is
	// fenced so it can never commit a grant after the logout.
	c.NoError(h.engine.Delete(context.Background(), h.owner, mcpoauth.DeleteSelector{Resource: root}), "logout by resource")
	_, err := h.lookup(fx.endpoint())
	c.True(errors.Is(err, mcpoauth.ErrMissing), "the bound endpoint after a logout: %v", err)
	_, err = h.engine.Complete(context.Background(), h.owner, pending.LoginID, fx.callbackQuery(t, pending.AuthorizeURL))
	c.Error(err, "a login pending at logout time completed anyway")
	_, err = h.lookup(fx.endpointAt("other"))
	c.True(errors.Is(err, mcpoauth.ErrMissing), "the pending endpoint after a logout: %v", err)

	// An absent resource is ErrMissing, and an unbound endpoint logout is an
	// idempotent local success.
	err = h.engine.Delete(context.Background(), h.owner, mcpoauth.DeleteSelector{Resource: "https://never.invalid/mcp"})
	c.True(errors.Is(err, mcpoauth.ErrMissing), "an unknown resource: %v", err)
	c.NoError(h.engine.Delete(context.Background(), h.owner, mcpoauth.DeleteSelector{Endpoint: fx.endpointAt("never")}),
		"logging out an endpoint nothing is bound to")

	// Two completed logins for one resource: the second advances the grant
	// generation, so the FIRST endpoint's binding is refused rather than
	// silently adopting the replacement.
	h.login(fx, "fixture", fx.endpoint())
	second := h.login(fx, "fixture", fx.endpointAt("other"))
	c.Eq(root, second.Resource, "the replacement keeps the same resource")
	h.binding(fx.endpointAt("other"))
	_, err = h.lookup(fx.endpoint())
	c.True(errors.Is(err, mcpoauth.ErrLoginRequired),
		"the older endpoint must not adopt a replacement grant, got %v", err)

	// Logging the second endpoint out invalidates the grant, so EVERY binding
	// to it is gone.
	c.NoError(h.engine.Delete(context.Background(), h.owner, mcpoauth.DeleteSelector{Endpoint: fx.endpointAt("other")}), "logout by endpoint")
	for _, endpoint := range []string{fx.endpoint(), fx.endpointAt("other")} {
		_, err := h.lookup(endpoint)
		c.True(errors.Is(err, mcpoauth.ErrMissing), "%s after the grant was invalidated: %v", endpoint, err)
	}
}

// ─── header precedence ─────────────────────────────────────────────────────

// TestMcpOAuthAuthorizationHeaderPrecedence pins the precedence rule at the
// passive seam: ANY spelling of an explicit Authorization header — including an
// empty value — disables stored OAuth for that entry, and a refusal there is
// never the login hint. An unrelated header changes nothing.
func TestMcpOAuthAuthorizationHeaderPrecedence(t *testing.T) {
	c := assert.NewAborting(t)
	h := newMcpOAuthHarness(t)
	fx := newFakeMCPOAuth(t)
	h.login(fx, "fixture", fx.endpoint())

	// An unrelated header coexists with the stored credential.
	handler, _, err := h.passive(fx.endpoint(), map[string]string{"X-Tenant": "acme"})
	c.NoError(err, "passive with an unrelated header")
	ts, err := handler.TokenSource(context.Background())
	c.NoError(err, "TokenSource")
	c.NotNil(ts, "an unrelated header disabled the stored credential")

	// An explicit Authorization header, in any case and even empty, disables it.
	for _, headers := range []map[string]string{
		{"authorization": ""},
		{"AUTHORIZATION": "Bearer configured-value"},
		{"Authorization": "Bearer configured-value"},
	} {
		handler, _, err := h.passive(fx.endpoint(), headers)
		c.NoError(err, "passive with %v", headers)
		ts, err := handler.TokenSource(context.Background())
		c.NoError(err, "TokenSource")
		c.Nil(ts, "headers %v did not disable stored OAuth", headers)

		// The refusal is authored and NOT the login hint: running a login could
		// not help an entry whose own header decided the credential.
		resp := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(""))}
		authErr := handler.Authorize(context.Background(), &http.Request{}, resp)
		c.Error(authErr, "an explicit-Authorization entry must refuse an unauthenticated request")
		c.NotStrContains(authErr.Error(), "needs a login", "an explicit-Authorization entry was told to log in")
	}
}

// TestMcpOAuthPassiveNeverDiscovers covers the passive insufficient_scope case:
// a credential the server refuses for SCOPE is answered with the named hint,
// locally — no discovery, no registration, no consent URL, no browser.
func TestMcpOAuthPassiveNeverDiscovers(t *testing.T) {
	c := assert.NewAborting(t)
	h := newMcpOAuthHarness(t)
	fx := newFakeMCPOAuth(t)
	h.login(fx, "fixture", fx.endpoint())
	before := len(fx.oauthRequests())
	authorizeBefore := len(fx.authorizeParamsSnapshot())

	// The access token is valid; it simply does not carry the scope the server
	// now demands, which is the shape a step-up would have handled interactively.
	fx.set(func(f *fakeMCPOAuth) { f.requireScopes = []string{"write"} })

	handler, client, err := h.passive(fx.endpoint(), nil)
	c.NoError(err, "passive")
	_, err = connectMCP(t, fx.endpoint(), handler, client)
	c.Error(err, "a session opened against a server that refused the credential's scopes")
	c.StrContains(err.Error(), "needs a login: run `rafiki mcp login fixture`", "the hint")

	c.Eq(before, len(fx.oauthRequests()), "a passive refusal performed discovery: %v", fx.oauthRequests()[before:])
	c.Eq(authorizeBefore, len(fx.authorizeParamsSnapshot()), "a passive refusal asked for consent")
}

// ─── a store outage is never an anonymous credential ────────────────────────

// TestMcpOAuthStoreOutageRefusesTheDial is the fail-closed rule at the passive
// seam: when the credential store cannot answer, the runtime is handed an error
// and no handler at all, so the server is refused (logged and skipped) instead
// of being dialled without a credential. "Cannot read the credential" must
// never be reported as "there is no credential".
func TestMcpOAuthStoreOutageRefusesTheDial(t *testing.T) {
	c := assert.NewAborting(t)
	h := newMcpOAuthHarness(t)
	fx := newFakeMCPOAuth(t)
	h.login(fx, "fixture", fx.endpoint())

	requestsBefore := len(fx.requestLog())
	// The pool the store reads through goes away. Nothing else about the engine
	// changes, which is exactly the shape of a database outage.
	h.pool.Close()

	handler, client, err := h.passive(fx.endpoint(), nil)
	c.Error(err, "a store outage handed back a handler")
	c.Nil(handler, "a store outage handed back a credential handler")
	c.Nil(client, "a store outage handed back a client")
	c.True(errors.Is(err, mcpoauth.ErrUnavailable), "the outage must stay distinguishable from a missing credential, got %v", err)
	c.NotStrContains(err.Error(), "needs a login", "a store outage must not be reported as a missing login")

	_, err = h.store.Acquire(context.Background(), h.owner, fx.endpoint(), 1, nil)
	c.True(errors.Is(err, mcpoauth.ErrUnavailable), "Acquire during an outage: %v", err)

	// And nothing reached the server: the refusal happened before any request.
	c.Eq(requestsBefore, len(fx.requestLog()), "a store outage still reached the server: %v", fx.requestLog()[requestsBefore:])
}

// ─── concurrency: one rotation per version, independent sessions ────────────

// TestMcpOAuthConcurrentRefreshSpendsOneRotation drives two INDEPENDENT stores
// (separate pools, so nothing is serialized by sharing a connection) at one
// expired grant. The first commits the durable refresh intent and blocks in the
// upstream call; the second must refuse rather than spend the single-use token
// again; the rotation the first persisted is what a later acquisition uses.
func TestMcpOAuthConcurrentRefreshSpendsOneRotation(t *testing.T) {
	c := assert.NewAborting(t)
	h := newMcpOAuthHarness(t)
	fx := newFakeMCPOAuth(t)
	h.login(fx, "fixture", fx.endpoint())
	generation := h.binding(fx.endpoint()).GrantGeneration
	expireGrantInDB(t, fx.endpoint())

	second, secondPool := h.secondStore()
	c.NotNil(secondPool, "an independent store")

	started := make(chan struct{})
	release := make(chan struct{})
	refreshCalls := 0
	first := make(chan struct {
		grant mcpoauth.Grant
		err   error
	}, 1)
	go func() {
		grant, err := h.store.Acquire(context.Background(), h.owner, fx.endpoint(), generation,
			func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
				refreshCalls++
				close(started)
				<-release
				return mcpoauth.RefreshResult{
					AccessToken:  "rotated-access-1",
					RefreshToken: "rotated-refresh-1",
					TokenType:    "Bearer",
					Expiry:       time.Now().Add(time.Hour),
				}, nil
			})
		first <- struct {
			grant mcpoauth.Grant
			err   error
		}{grant, err}
	}()

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first acquisition never reached its refresh")
	}

	// The independent session sees the committed intent and refuses. It must not
	// spend the refresh token again — its own refresh func fails the test.
	_, err := second.Acquire(context.Background(), h.owner, fx.endpoint(), generation,
		func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
			t.Error("a second session refreshed the same version")
			return mcpoauth.RefreshResult{}, errors.New("unreachable")
		})
	c.True(errors.Is(err, mcpoauth.ErrLoginRequired), "a concurrent acquisition: %v", err)

	close(release)
	got := <-first
	c.NoError(got.err, "the first acquisition")
	c.Eq("rotated-access-1", got.grant.Token.AccessToken, "the first acquisition returns the rotation")
	c.Eq(1, refreshCalls, "upstream refreshes for one version")

	// The rotation is PERSISTED: another session reads it with no refresh at all.
	later, err := second.Acquire(context.Background(), h.owner, fx.endpoint(), generation, nil)
	c.NoError(err, "a later acquisition after the rotation")
	c.Eq("rotated-access-1", later.Token.AccessToken, "the persisted access token")
	c.Eq("rotated-refresh-1", later.Token.RefreshToken, "the persisted rotation")
	c.Eq(1, refreshCalls, "a later acquisition refreshed a valid token")
}

// ─── fencing: logout, user removal, and an older login ─────────────────────

// TestMcpOAuthFencesBeatRefreshAndOlderLogins is the tombstone/generation rule
// under real concurrency, with two independent stores:
//
//   - a logout that commits while a refresh is in flight makes that refresh's
//     finalize CAS fail, so no token is ever handed out for a logged-out grant;
//   - a login that completes after a logout is refused (stale fence), so an
//     older pending flow cannot recreate the grant;
//   - removing the user mid-refresh makes the finalize refuse too, and the
//     destructive cleanup still works for an already-tombstoned owner.
func TestMcpOAuthFencesBeatRefreshAndOlderLogins(t *testing.T) {
	c := assert.NewAborting(t)
	h := newMcpOAuthHarness(t)
	fx := newFakeMCPOAuth(t)
	second, _ := h.secondStore()

	t.Run("logout during a refresh", func(t *testing.T) {
		h.login(fx, "fixture", fx.endpoint())
		generation := h.binding(fx.endpoint()).GrantGeneration
		expireGrantInDB(t, fx.endpoint())

		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := h.store.Acquire(context.Background(), h.owner, fx.endpoint(), generation,
				func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
					close(started)
					<-release
					return mcpoauth.RefreshResult{
						AccessToken: "must-never-be-used", RefreshToken: "must-never-be-used",
						Expiry: time.Now().Add(time.Hour),
					}, nil
				})
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("the acquisition never reached its refresh")
		}

		// The logout commits while the upstream call is in flight. It must not
		// block on that call: the intent committed and released its lock.
		_, err := second.InvalidateEndpoint(context.Background(), h.owner, fx.endpoint())
		c.NoError(err, "logout during a refresh")
		close(release)
		err = <-done
		c.Error(err, "a refresh that finished after a logout returned a usable token")
		c.True(errors.Is(err, mcpoauth.ErrLoginRequired) || errors.Is(err, mcpoauth.ErrStaleFence),
			"the refresh's outcome: %v", err)
		_, lookupErr := h.lookup(fx.endpoint())
		c.True(errors.Is(lookupErr, mcpoauth.ErrMissing), "the binding after the logout: %v", lookupErr)
	})

	t.Run("an older login cannot recreate a logged-out grant", func(t *testing.T) {
		endpoint := fx.endpointAt("older")
		fence, err := h.store.BeginFence(context.Background(), h.owner, endpoint)
		c.NoError(err, "BeginFence")
		fence, err = h.store.BindFence(context.Background(), fence, fx.canonicalRoot(t), fx.issuer())
		c.NoError(err, "BindFence")

		_, err = second.InvalidateEndpoint(context.Background(), h.owner, endpoint)
		c.NoError(err, "logout while the login is pending")

		_, err = h.store.CommitLogin(context.Background(), fence, mcpoauth.Grant{
			OwnerUserID: h.owner,
			Resource:    fence.Resource,
			Issuer:      fence.Issuer,
			Registration: mcpoauth.Registration{
				ClientID:              "client-1",
				AuthStyle:             mcpoauth.AuthStyleInParams,
				AuthorizationEndpoint: fx.issuer() + "/authorize",
				TokenEndpoint:         fx.issuer() + "/token",
			},
			Token: mcpoauth.Token{AccessToken: "late", RefreshToken: "late", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
		})
		c.True(errors.Is(err, mcpoauth.ErrStaleFence), "an older login committed after a logout: %v", err)
		_, lookupErr := h.lookup(endpoint)
		c.True(errors.Is(lookupErr, mcpoauth.ErrMissing), "the logged-out endpoint: %v", lookupErr)
	})

	t.Run("user removal during a refresh", func(t *testing.T) {
		h.login(fx, "fixture", fx.endpointAt("removal"))
		endpoint := fx.endpointAt("removal")
		generation := h.binding(endpoint).GrantGeneration
		// This login discovered the ROOT as its resource, so that is the grant
		// row whose expiry has to move.
		expireGrantInDB(t, fx.canonicalRoot(t))

		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := h.store.Acquire(context.Background(), h.owner, endpoint, generation,
				func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
					close(started)
					<-release
					return mcpoauth.RefreshResult{AccessToken: "late", RefreshToken: "late", Expiry: time.Now().Add(time.Hour)}, nil
				})
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("the acquisition never reached its refresh")
		}

		// Remove the user: the tombstone is what every read and write re-checks.
		_, err := h.pool.Exec(context.Background(),
			`UPDATE conversations.users SET deleted_at = now() WHERE id = $1::uuid`, h.owner)
		c.NoError(err, "tombstone the owner")
		close(release)
		err = <-done
		c.Error(err, "a refresh completed for a removed user")
		c.True(errors.Is(err, mcpoauth.ErrLoginRequired), "a removed owner's acquisition: %v", err)

		// The destructive cleanup needs no live owner: it still wipes the rows.
		revocations, err := second.InvalidateOwner(context.Background(), h.owner)
		c.NoError(err, "InvalidateOwner for an already-tombstoned owner")
		c.NotNil(revocations, "the detached revocation payload")
		_, lookupErr := h.lookup(endpoint)
		c.True(errors.Is(lookupErr, mcpoauth.ErrLoginRequired) || errors.Is(lookupErr, mcpoauth.ErrMissing),
			"a removed owner's binding: %v", lookupErr)
	})
}

// ─── cancellation and expiry free pending work ─────────────────────────────

// TestMcpOAuthPendingWorkIsFreed proves no login outlives the daemon or its own
// TTL: closing the engine cancels a login that is waiting for a browser, a
// login waiting in discovery has its REQUEST cancelled (not merely abandoned),
// and an expired pending login is gone rather than lingering in the table.
func TestMcpOAuthPendingWorkIsFreed(t *testing.T) {
	c := assert.NewAborting(t)
	h := newMcpOAuthHarness(t)
	fx := newFakeMCPOAuth(t)

	t.Run("close cancels a login waiting for a browser", func(t *testing.T) {
		engine := h.newEngineWith(context.Background(), nil)
		descriptor, err := engine.Begin(context.Background(), mcpoauth.BeginRequest{
			OwnerUserID: h.owner, Server: "fixture", Endpoint: fx.endpoint(),
		})
		c.NoError(err, "Begin")
		c.NoError(engine.Close(), "engine.Close")

		started := time.Now()
		_, err = engine.Complete(context.Background(), h.owner, descriptor.LoginID, fx.callbackQuery(t, descriptor.AuthorizeURL))
		c.Error(err, "a closed engine completed a login")
		c.True(errors.Is(err, mcpoauth.ErrMissing) || errors.Is(err, mcpoauth.ErrClosed), "the outcome: %v", err)
		c.True(time.Since(started) < 5*time.Second, "Complete blocked for %s after the engine closed", time.Since(started))
	})

	t.Run("shutdown does not wait out a gated discovery request", func(t *testing.T) {
		// The authorization server holds its metadata response open. If shutdown
		// merely stopped waiting — leaving the round trip to run out the 30s
		// operation bound — Close would take that long. It must not: the flow's
		// context is what the request is issued under.
		gate := make(chan struct{})
		fx.set(func(f *fakeMCPOAuth) { f.prmGate = gate })
		t.Cleanup(func() {
			fx.set(func(f *fakeMCPOAuth) { f.prmGate = nil })
			close(gate)
		})

		engine := h.newEngineWith(context.Background(), nil)
		beginErr := make(chan error, 1)
		go func() {
			_, err := engine.Begin(context.Background(), mcpoauth.BeginRequest{
				OwnerUserID: h.owner, Server: "fixture", Endpoint: fx.endpointAt("gated"),
			})
			beginErr <- err
		}()

		// Wait until the metadata request is genuinely in flight at the server.
		deadline := time.Now().Add(30 * time.Second)
		for fx.prmHitsFor("/.well-known/oauth-protected-resource/mcp") == 0 {
			if !time.Now().Before(deadline) {
				t.Fatal("the login never reached protected-resource discovery")
			}
			time.Sleep(10 * time.Millisecond)
		}

		start := time.Now()
		c.NoError(engine.Close(), "engine.Close")
		elapsed := time.Since(start)
		c.True(elapsed < 10*time.Second, "engine.Close waited %s for a gated discovery request: the pending work was not freed", elapsed)
		select {
		case err := <-beginErr:
			c.Error(err, "a cancelled Begin returned a descriptor")
		case <-time.After(10 * time.Second):
			t.Fatal("Begin did not return after the engine closed")
		}
	})

	t.Run("an expired pending login is gone", func(t *testing.T) {
		after := func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		}
		engine := h.newEngineWith(context.Background(), after)
		// The TTL fires immediately, so the descriptor may or may not reach the
		// caller; either way the login must be gone, and a fresh one on the same
		// endpoint must be able to start (the pending slot is not leaked).
		descriptor, beginErr := engine.Begin(context.Background(), mcpoauth.BeginRequest{
			OwnerUserID: h.owner, Server: "fixture", Endpoint: fx.endpointAt("expiring"),
		})
		if beginErr == nil {
			deadline := time.Now().Add(10 * time.Second)
			var err error
			for {
				_, err = engine.Complete(context.Background(), h.owner, descriptor.LoginID, "code=x&state=y")
				if errors.Is(err, mcpoauth.ErrMissing) {
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatalf("an expired login stayed usable: %v", err)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		fresh, err := h.newEngineWith(context.Background(), nil).Begin(context.Background(), mcpoauth.BeginRequest{
			OwnerUserID: h.owner, Server: "fixture", Endpoint: fx.endpointAt("expiring"),
		})
		c.NoError(err, "a login after an expiry")
		c.NotEq("", fresh.LoginID, "a fresh login id")
	})
}

// ─── malformed metadata, issuer, state and private redirects ───────────────

// TestMcpOAuthRefusesDamagedDiscoveryAndRedirects covers the hostile-input
// shapes: undecodable authorization-server metadata, an issuer that does not
// match, an authorization endpoint pointing at a private address, a
// protected-resource document that must be fallen back FROM rather than
// trusted, and an authorization server that answers the consent round with an
// OAuth error. Each must fail the login (or bind only the documented fallback)
// and leave no grant behind for a damaged flow.
func TestMcpOAuthRefusesDamagedDiscoveryAndRedirects(t *testing.T) {
	cases := []struct {
		name  string
		shape func(*fakeMCPOAuth)
		// wantHint is a substring the surfaced error must carry.
		wantHint string
		// mustNotContain are strings that must NOT survive into the refusal:
		// addresses chosen by remote metadata are redacted, not echoed.
		mustNotContain []string
		// wantErr is the sentinel the refusal keeps, when it keeps one.
		wantErr error
	}{
		{
			name:     "undecodable authorization-server metadata",
			shape:    func(f *fakeMCPOAuth) { f.malformedASM = true },
			wantHint: "failed",
		},
		{
			name:     "issuer does not match",
			shape:    func(f *fakeMCPOAuth) { f.asmIssuer = "https://issuer.invalid" },
			wantHint: "failed",
		},
		{
			name:           "authorization endpoint on a private address",
			shape:          func(f *fakeMCPOAuth) { f.authorizeURL = "https://10.0.0.1/authorize" },
			wantHint:       "failed",
			mustNotContain: []string{"10.0.0.1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			h := newMcpOAuthHarness(t)
			fx := newFakeMCPOAuth(t)
			fx.set(tc.shape)

			descriptor, err := h.engine.Begin(context.Background(), mcpoauth.BeginRequest{
				OwnerUserID: h.owner, Server: "fixture", Endpoint: fx.endpoint(),
			})
			// Either Begin or the round it publishes fails; both are refusals.
			if err == nil {
				_, err = h.engine.Complete(context.Background(), h.owner, descriptor.LoginID, "code=x&state=y")
			}
			c.Error(err, "a damaged discovery/redirect produced a login")
			c.StrContains(err.Error(), tc.wantHint, "the refusal is authored and names the step: %v", err)
			if tc.wantErr != nil {
				c.True(errors.Is(err, tc.wantErr), "the refusal keeps the sentinel: %v", err)
			}
			for _, forbidden := range tc.mustNotContain {
				c.NotStrContains(err.Error(), forbidden, "remote metadata's choice survives into the refusal")
			}
			_, lookupErr := h.lookup(fx.endpoint())
			c.True(errors.Is(lookupErr, mcpoauth.ErrMissing), "a damaged login left a binding: %v", lookupErr)
			pool2 := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
			defer pool2.Close()
			var grant int
			c.NoError(pool2.QueryRow(context.Background(),
				`SELECT count(*) FROM conversations.mcp_credential WHERE owner_user_id = $1::uuid`, h.owner).Scan(&grant),
				"count grants")
			c.Eq(0, grant, "a damaged login stored a grant")
			c.Eq(0, len(fx.headersOnPath("/authorize")),
				"a browser was sent to an authorization endpoint the daemon refuses")
		})
	}

	// A protected-resource document that cannot be decoded is neither trusted
	// nor fatal: the SDK's documented fallback decides the resource, and the
	// binding is that fallback — never a claim parsed out of the damaged body.
	t.Run("undecodable protected-resource metadata is not trusted", func(t *testing.T) {
		c := assert.NewAborting(t)
		h := newMcpOAuthHarness(t)
		fx := newFakeMCPOAuth(t)
		fx.set(func(f *fakeMCPOAuth) { f.malformedPRM = true; f.servePRMRoot = false })

		res := h.login(fx, "fixture", fx.endpoint())
		c.Eq(fx.endpoint(), res.Resource, "the fallback resource is the MCP endpoint itself")
		c.NotEq(fx.canonicalRoot(t), res.Resource, "the damaged document must not decide the resource")
		c.True(fx.prmHitsFor("/.well-known/oauth-protected-resource/mcp") >= 1, "the damaged document was never even read")
	})

	t.Run("consent answered with an OAuth error", func(t *testing.T) {
		c := assert.NewAborting(t)
		h := newMcpOAuthHarness(t)
		fx := newFakeMCPOAuth(t)
		fx.set(func(f *fakeMCPOAuth) { f.authorizeFail = "access_denied" })
		descriptor := h.begin(fx, "fixture", fx.endpoint())
		_, err := h.engine.Complete(context.Background(), h.owner, descriptor.LoginID, fx.callbackQuery(t, descriptor.AuthorizeURL))
		c.Error(err, "a refused consent completed the login")
		c.StrContains(err.Error(), "access_denied", "the refusal keeps only the protocol error code: %v", err)
		c.NotStrContains(err.Error(), "code=", "the refusal echoed the callback")
		_, lookupErr := h.lookup(fx.endpoint())
		c.True(errors.Is(lookupErr, mcpoauth.ErrMissing), "a refused consent left a binding: %v", lookupErr)
	})
}
