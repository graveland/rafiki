// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end tests for the CHILD surface of the MCP agent-control face: the
// per-child RAFIKI_MCP_TOKEN minted at spawn (Controller.mintMCPToken),
// delivered to the child by environment only, and served by mcp_face.go's
// ProvenanceChildToken arm.
//
// What only this file can prove, because everything below drives a REAL daemon
// subprocess over real HTTP and a REAL child process:
//   - the token a real spawn actually delivered to a real child authenticates
//     a real MCP session, and tools/list is exactly the twelve tools
//     mcpToolNames pins;
//   - that session binds a CONTROLLER spawner scoped to the child's own
//     subtree, not a user spawner with daemon-wide scope (coordinator
//     amendment A1) — the regression the unit tests cannot see;
//   - the bare per-boot proxy secret, the child's billing bearer, is NOT an
//     agent-control credential (amendment A2);
//   - a session id is bound to the (UserID, ChildID) principal that created
//     it, so a sibling presenting it is refused (403);
//   - agent_kill across a sibling edge is refused by position in the tree;
//   - AppendSystemPrompt and ExtraArgs survive the daemon's spawn path into
//     the child's actual argv.
//
// The claude children are the fake-claude.sh fixture, selected at daemon boot
// with CLAUDE_BINARY (resolveClaudeBinary's override chain): the daemon's
// local-subprocess claude path execs it, and it records the env and argv it
// was launched with for the tests to read. The harness has no executor pool,
// so every claude child here takes that local path; the daraja path this
// surface also runs on builds argv through the same claudeargv builder, and
// TestClaudeArgvIdenticalAcrossPaths pins the two byte-identical.
//
// Same rules as mcp_test.go: -count=1 always (go test caching cannot see
// through to a daemon subprocess), and every fatal carries the daemon's
// captured stderr, because a proxy-face failure reads as a dead endpoint.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── harness: daemon + fake claude children ──────────────────────────────────

// bootMCPChildDaemon boots an MCP daemon whose claude children are the
// fake-claude fixture, and returns the daemon plus the directory the fixture
// dumps into. CLAUDE_BINARY selects the fixture for MCP-spawned children (the
// MCP agent_spawn schema deliberately carries no binary override); a test that
// spawns over the UDS can also name it explicitly via piBinary. Everything
// else matches bootMCPDaemon: noRealProviderEnv, stop-then-remove cleanup, and
// both readiness waits.
func bootMCPChildDaemon(t *testing.T) (*daemon, string) {
	t.Helper()
	dumps := t.TempDir()
	// Executors must be off: a DB-backed daemon builds an executor pool by
	// default, and a claude child then REQUIRES an executor that advertises
	// claude launch — refused outright against a pool with zero live
	// executors. RAFIKI_EXECUTORS_ENABLED=0 removes the pool entirely, which
	// is what routes claude spawns down the local-subprocess path the
	// fake-claude fixture observes (claudeRunner's documented lone-developer
	// topology). Nothing these tests assert touches the executor plane.
	extra := append(noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"CLAUDE_BINARY="+fakeClaudeBin(t),
		"FAKE_CLAUDE_DIR="+dumps,
	)
	d := bootDaemonDB(t, nextDaemonID(), extra...)
	t.Cleanup(func() {
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})
	return d, dumps
}

// fakeClaudeBin locates the committed fixture. Absolute, so the daemon
// subprocess — which inherits the test process's cwd — always resolves it.
func fakeClaudeBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot, "test", "integration", "fake-claude.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fake-claude.sh not found at %s: %v", p, err)
	}
	return p
}

// claudeDump is what one fake-claude child recorded about its own launch: the
// argv it was exec'd with (before the marker) and its effective environment
// (after it).
type claudeDump struct {
	argv []string
	env  []string
}

// envValue returns a single-line K=V from the recorded environment, or "".
// Only single-line keys are readable this way; ANTHROPIC_CUSTOM_HEADERS is
// deliberately multi-line (FormatHeaders joins with raw newlines) and is read
// through sessionHeader instead.
func (c claudeDump) envValue(key string) string {
	for _, l := range c.env {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return ""
}

// sessionHeader returns the X-Rafiki-Session value inside the recorded
// ANTHROPIC_CUSTOM_HEADERS — the attribution the proxy face bills /v1/messages
// through, and the second way a dump names the child that wrote it. The
// header's value carries a raw newline, so `env` prints the first header
// glued onto the ANTHROPIC_CUSTOM_HEADERS= line and the rest bare; match the
// prefix anywhere on the line rather than only at line start.
func (c claudeDump) sessionHeader() string {
	const marker = "X-Rafiki-Session: "
	for _, l := range c.env {
		if i := strings.LastIndex(l, marker); i >= 0 {
			return strings.TrimSpace(l[i+len(marker):])
		}
	}
	return ""
}

// parseClaudeDump splits one dump file into its argv and env halves.
func parseClaudeDump(t *testing.T, path string) claudeDump {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		if l == "---ENV---" {
			return claudeDump{argv: lines[:i], env: lines[i+1:]}
		}
	}
	t.Fatalf("dump %s carries no ---ENV--- marker; the fixture changed shape", path)
	return claudeDump{}
}

// waitClaudeDump polls the dump directory until a fake-claude child whose
// RAFIKI_CHILD_ID (buildEnv sets it on every child) names childID has recorded
// its launch. A child that died at startup — a broken fixture, a spawn plan
// that refused it — never writes one, so the failure carries the daemon's
// stderr: the only place the real cause is written.
func waitClaudeDump(t *testing.T, d *daemon, dumpDir, childID string) claudeDump {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dumpDir, "claude-*.dump"))
		for _, p := range matches {
			dump := parseClaudeDump(t, p)
			if dump.envValue(paths.ChildID) == childID {
				return dump
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Diagnose before failing: every dump that DID land names the child that
	// wrote it, so list them — a dump naming a different child is a spawn
	// bookkeeping bug, no dump at all is a child that died before recording.
	if entries, err := os.ReadDir(dumpDir); err == nil {
		for _, e := range entries {
			if b, err := os.ReadFile(filepath.Join(dumpDir, e.Name())); err == nil {
				t.Logf("dump %s:\n%s", e.Name(), b)
			}
		}
	} else {
		t.Logf("read dump dir %s: %v", dumpDir, err)
	}
	// The daemon's per-child log tree records what it saw the child do.
	if entries, err := os.ReadDir(filepath.Join(d.logsDir, childID)); err == nil {
		for _, e := range entries {
			if b, err := os.ReadFile(filepath.Join(d.logsDir, childID, e.Name())); err == nil && len(b) > 0 {
				t.Logf("child log %s:\n%.2000s", e.Name(), b)
			}
		}
	} else {
		t.Logf("read child log dir %s: %v", filepath.Join(d.logsDir, childID), err)
	}
	t.Fatalf("no fake-claude dump naming child %s appeared in %s within 20s\nstderr:\n%s",
		childID, dumpDir, d.stderr.tail(4000))
	return claudeDump{}
}

// mcpSpawnClaudeChild spawns one claude child through an MCP session — the
// user-token session for top-level children, a child-token session for
// descendants — and returns its child id.
func mcpSpawnClaudeChild(t *testing.T, sess *mcp.ClientSession, name string) string {
	t.Helper()
	text := mcpOK(t, sess, "agent_spawn", map[string]any{
		"prompt": "Integration-test filler; the fake claude child never answers it.",
		"name":   name,
		"cwd":    "/tmp",
		"model":  "anthropic/sonnet-latest",
		"kind":   "claude",
	})
	id := mcpFirstAgentID(t, text)
	if id == "" {
		t.Fatalf("agent_spawn(%s) returned no agent id; output:\n%s", name, text)
	}
	return id
}

// mcpRawInitialize establishes a raw streamable-HTTP session with token and
// returns its Mcp-Session-Id. The initialize stream is read to EOF before
// returning: the face records the session→principal binding only after its
// handler returns, so a request pipelined ahead of that would read as an
// unknown session.
func mcpRawInitialize(t *testing.T, d *daemon, token string, id int) string {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-child-it","version":"0"}}}`, id)
	resp := mcpRawPost(t, d.proxyURL, token, "", body)
	if resp.StatusCode != http.StatusOK {
		b := mcpDrain(t, resp)
		t.Fatalf("child-token initialize status = %d, want 200 (body: %.200s)", resp.StatusCode, b)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("child-token initialize response carried no Mcp-Session-Id")
	}
	mcpReadSSEData(t, resp, float64(id))
	return sid
}

// mcpRawToolsListNames issues a raw tools/list POST and decodes the tool names
// out of its SSE response. Status and stream-shape failures carry the body.
func mcpRawToolsListNames(t *testing.T, d *daemon, token, sid string, id int) []string {
	t.Helper()
	resp := mcpRawPost(t, d.proxyURL, token, sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list"}`, id))
	if resp.StatusCode != http.StatusOK {
		b := mcpDrain(t, resp)
		t.Fatalf("tools/list status = %d, want 200 (body: %.200s)", resp.StatusCode, b)
	}
	var payload struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mcpReadSSEData(t, resp, float64(id)), &payload); err != nil {
		t.Fatalf("decode tools/list SSE frame: %v", err)
	}
	names := make([]string, 0, len(payload.Result.Tools))
	for _, tl := range payload.Result.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// mcpDrain closes a response whose body is a plain error page, returning its
// text for failure messages.
func mcpDrain(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(raw)
}

// mcpAssertTwelveTools fails the test unless got is exactly the assertion of
// record. Both credential kinds assert against the one list.
func mcpAssertTwelveTools(t *testing.T, where string, got []string) {
	t.Helper()
	want := slices.Clone(mcpToolNames)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: tools/list = %v, want exactly %v", where, got, want)
	}
}

// ─── the tests ───────────────────────────────────────────────────────────────

// TestMCPChildTokenReachesTwelveTools is the test whose failure was the whole
// bug: a child spawned through the daemon holds a per-child MCP secret, and a
// session established with it must reach the twelve-tool agent-control
// surface.
//
// The negative legs carry the two coordinator amendments:
//
//	A1 — the child-token session binds a CONTROLLER spawner: agent_list
//	     through it shows the child's own subtree and omits a top-level
//	     non-descendant, while the owner's user-token session sees that
//	     non-descendant. A regression substituting newUserSpawner into the
//	     child-token arm of getServer passes every unit test and fails here.
//	A2 — the bare per-boot proxy secret (the child's ANTHROPIC_AUTH_TOKEN,
//	     no X-Rafiki-Session header) is NOT an agent-control credential:
//	     tools/list with it alone gets 403, never a tool list or a stream.
func TestMCPChildTokenReachesTwelveTools(t *testing.T) {
	t.Parallel()
	d, dumps := bootMCPChildDaemon(t)
	token := d.createMCPUser(t)
	userSess := mcpConnect(t, d.proxyURL, token)

	// The child must be spawned through a path that RECORDS an owner:
	// ChildForMCPToken refuses an anonymous spawn, so a child created over the
	// unauthenticated UDS could never resolve its own secret. The owner's MCP
	// session is the path a real child credential holder is minted through.
	childA := mcpSpawnClaudeChild(t, userSess, "mcp-child-a")
	dumpA := waitClaudeDump(t, d, dumps, childA)

	// The attribution header the /v1/messages face bills through names this
	// same child — the id the MCP secret resolves onto.
	if got := dumpA.sessionHeader(); got != childA {
		t.Errorf("X-Rafiki-Session in the child's environment = %q, want %q", got, childA)
	}

	mcpToken := dumpA.envValue("RAFIKI_MCP_TOKEN")
	if mcpToken == "" {
		t.Fatal("the spawned claude child's environment carries no RAFIKI_MCP_TOKEN; the spawn path delivered no per-child secret")
	}
	if mcpToken == token {
		t.Fatal("the per-child secret must not be the owner's user token")
	}
	// The per-boot proxy bearer rides the same environment (it is the billing
	// credential on every child path) and must stay a DIFFERENT secret from
	// the per-child one — its presence here is what makes A2's negative leg
	// below a real credential rather than a guess.
	bootToken := dumpA.envValue("ANTHROPIC_AUTH_TOKEN")
	if bootToken == "" || bootToken == mcpToken {
		t.Fatalf("ANTHROPIC_AUTH_TOKEN = %q; the per-boot proxy bearer must be present and distinct from the per-child secret", bootToken)
	}

	// Headline: tools/list through the child-token session is exactly the
	// twelve tools mcpToolNames pins — no more, no fewer.
	sessA := mcpConnect(t, d.proxyURL, mcpToken)
	tools, err := sessA.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("child-token ListTools: %v", err)
	}
	var got []string
	for _, tl := range tools.Tools {
		got = append(got, tl.Name)
	}
	mcpAssertTwelveTools(t, "child-token session", got)

	// A1: seed a child parented OUTSIDE the spawned child — a top-level row.
	outsider := d.spawnChild(t)

	// A1's strong half: the child-token list must SHOW the child's own
	// subtree, so a vacuously-empty list cannot pass the omission below. This
	// also exercises the child-token spawn path: the daemon must parent this
	// child under A, never top-level.
	underText := mcpOK(t, sessA, "agent_spawn", map[string]any{
		"prompt": "Integration-test filler; exists only to appear in its parent's subtree.",
		"name":   "mcp-under-a",
		"cwd":    "/tmp",
		"model":  "anthropic/sonnet-latest",
	})
	underA := mcpFirstAgentID(t, underText)
	if underA == "" {
		t.Fatalf("agent_spawn through the child-token session returned no id; output:\n%s", underText)
	}

	listA := mcpOK(t, sessA, "agent_list", nil)
	if !strings.Contains(listA, underA) {
		t.Fatalf("child-token agent_list omits the child's own descendant %s; output:\n%s", underA, listA)
	}
	if strings.Contains(listA, outsider) {
		t.Fatalf("child-token agent_list leaked the non-descendant %s — the child-token arm bound a user spawner (daemon-wide scope), not a controller spawner; output:\n%s", outsider, listA)
	}
	listUser := mcpOK(t, userSess, "agent_list", nil)
	if !strings.Contains(listUser, outsider) {
		t.Fatalf("user-token agent_list does not see the top-level row %s, so the child-token omission proves nothing; output:\n%s", outsider, listUser)
	}
	if !strings.Contains(listUser, childA) {
		t.Fatalf("user-token agent_list does not see the spawned child %s; output:\n%s", childA, listUser)
	}

	// A2: the bare per-boot proxy secret is a billing credential, not an
	// agent-control one. No X-Rafiki-Session header means nothing attributes
	// it to a child, so it resolves to an identity with no provenance, and
	// the face must answer 403 before any server object is built.
	resp, body := mcpRawBody(t, d.proxyURL, bootToken, "", `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("bare per-boot secret tools/list status = %d, want 403 (body: %.200s)", resp.StatusCode, body)
	}
	if strings.Contains(body, `"result"`) {
		t.Errorf("the bare per-boot secret got a JSON-RPC result; an unentitled credential must never reach the tool surface (body: %.200s)", body)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("the bare per-boot secret was answered with %q; an unentitled credential must never get a stream", ct)
	}
}

// TestMCPChildKillNonDescendantRefused: two sibling children; A's secret
// calling agent_kill on B is refused — the controller spawner authorizes by
// position in the tree, and a sibling is not a descendant. The positive
// control (the same session and verb against A's OWN descendant) keeps the
// refusal from passing for a kill verb that is simply broken.
func TestMCPChildKillNonDescendantRefused(t *testing.T) {
	t.Parallel()
	d, dumps := bootMCPChildDaemon(t)
	token := d.createMCPUser(t)
	userSess := mcpConnect(t, d.proxyURL, token)

	childA := mcpSpawnClaudeChild(t, userSess, "mcp-killer-a")
	childB := mcpSpawnClaudeChild(t, userSess, "mcp-sibling-b")
	tokA := waitClaudeDump(t, d, dumps, childA).envValue("RAFIKI_MCP_TOKEN")
	if tokA == "" {
		t.Fatal("child A's environment carries no RAFIKI_MCP_TOKEN")
	}

	sessA := mcpConnect(t, d.proxyURL, tokA)
	res, text := mcpCallTool(t, sessA, "agent_kill", map[string]any{"agent": childB})
	if !res.IsError {
		t.Fatalf("agent_kill on sibling %s succeeded; a child may only kill its own subtree (output:\n%s)", childB, text)
	}
	if !strings.Contains(text, "not a descendant") {
		t.Fatalf("agent_kill refusal does not state the non-descendant reason; output:\n%s", text)
	}

	// Positive control: A's own descendant must be killable by A's secret.
	underA := mcpSpawnClaudeChild(t, sessA, "mcp-under-killer")
	killText := mcpOK(t, sessA, "agent_kill", map[string]any{"agent": underA})
	if !strings.Contains(killText, underA) {
		t.Fatalf("agent_kill on A's own descendant %s did not name it; output:\n%s", underA, killText)
	}
}

// TestMCPChildSessionNotShared: A's secret establishes a session; B's secret
// presenting A's Mcp-Session-Id is refused, because the face keys sessions on
// the (UserID, ChildID) principal — two children of one owner share a UserID,
// and a UserID-keyed map would let B execute against A's bound spawner.
func TestMCPChildSessionNotShared(t *testing.T) {
	t.Parallel()
	d, dumps := bootMCPChildDaemon(t)
	token := d.createMCPUser(t)
	userSess := mcpConnect(t, d.proxyURL, token)

	childA := mcpSpawnClaudeChild(t, userSess, "mcp-sess-a")
	childB := mcpSpawnClaudeChild(t, userSess, "mcp-sess-b")
	tokA := waitClaudeDump(t, d, dumps, childA).envValue("RAFIKI_MCP_TOKEN")
	tokB := waitClaudeDump(t, d, dumps, childB).envValue("RAFIKI_MCP_TOKEN")
	if tokA == "" || tokB == "" {
		t.Fatalf("child secrets missing (A=%q B=%q)", tokA, tokB)
	}
	if tokA == tokB {
		t.Fatal("two children minted the same MCP secret")
	}

	// A establishes a session; the initialize stream is read to EOF, which is
	// what lets the daemon's handler return and record the binding.
	sid := mcpRawInitialize(t, d, tokA, 1)

	// B's token presenting A's session id: 403, never a dispatch.
	resp, body := mcpRawBody(t, d.proxyURL, tokB, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("sibling presenting another child's Mcp-Session-Id: status = %d, want 403 (body: %.200s)", resp.StatusCode, body)
	}
	if strings.Contains(body, `"result"`) {
		t.Errorf("the hijacked session carried a JSON-RPC result (body: %.200s)", body)
	}

	// The principal check is what refused it, not the session id or the verb:
	// A's own token on its own session still lists the twelve tools...
	mcpAssertTwelveTools(t, "own principal on its own session",
		mcpRawToolsListNames(t, d, tokA, sid, 3))

	// ...and B can establish its OWN session on its own credential — siblings
	// coexist; only the cross-principal reuse is refused.
	sidB := mcpRawInitialize(t, d, tokB, 4)
	mcpAssertTwelveTools(t, "sibling's own session",
		mcpRawToolsListNames(t, d, tokB, sidB, 5))
}

// TestMCPChildArgvFlagsSurviveDaraja: a child spawned with AppendSystemPrompt
// and ExtraArgs set has both in its ACTUAL argv — the wire-to-process hop no
// unit test observes. The harness has no executor pool, so this child takes
// the local-subprocess path; the daraja path builds argv through the same
// claudeargv builder, and TestClaudeArgvIdenticalAcrossPaths pins the two
// byte-identical.
func TestMCPChildArgvFlagsSurviveDaraja(t *testing.T) {
	t.Parallel()
	d, dumps := bootMCPChildDaemon(t)

	const wantPrompt = "integration-test system prompt appendix"
	frame := fmt.Sprintf(
		`{"type":"ctrl_spawn","id":"argv1","cwd":"/tmp","noSession":true,"kind":"claude",`+
			`"model":"anthropic/sonnet-latest","piBinary":%q,`+
			`"appendSystemPrompt":%q,"extraArgs":["--foo","bar"]}`,
		fakeClaudeBin(t), wantPrompt)
	raw := d.request(t, frame)
	var r protocol.Response
	mustUnmarshal(t, raw, &r)
	if !r.Success {
		t.Fatalf("ctrl_spawn (claude, argv flags) failed: %+v", r.Error)
	}
	var data protocol.SpawnResponseData
	mustUnmarshal(t, r.Data, &data)
	if data.ChildID == "" {
		t.Fatal("spawn returned empty childId")
	}

	dump := waitClaudeDump(t, d, dumps, data.ChildID)
	for _, want := range []string{"--append-system-prompt", wantPrompt, "--foo", "bar"} {
		if !slices.Contains(dump.argv, want) {
			t.Errorf("claude argv %q is missing %q; the spawn path dropped it", dump.argv, want)
		}
	}

	// The same dump pins the security invariant on the real launch: the
	// per-child secret travels by ENVIRONMENT only. It must appear in the env
	// half and never in the argv half, whose --mcp-config carries only the
	// ${RAFIKI_MCP_TOKEN} placeholder (ps renders argv world-readable).
	tok := dump.envValue("RAFIKI_MCP_TOKEN")
	if tok == "" {
		t.Fatal("the child's environment carries no RAFIKI_MCP_TOKEN")
	}
	if slices.Contains(dump.argv, tok) {
		t.Errorf("the per-child MCP secret appears in the child's argv %q; it must travel by environment only", dump.argv)
	}
}
