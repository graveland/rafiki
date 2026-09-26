// SPDX-License-Identifier: Apache-2.0

package integration_test

// Script children, end to end on the real daemon (wave 3): a saved pymodule
// run as a child process with a per-child Connect socket as its only channel.
//
// Two tests live here:
//
//   - TestConnectScriptVerbsOnTheConnectPlane pins the wave-2 verbs
//     (Report/Receive/SetResult) against the REAL daemon wiring with a real
//     script-child credential — the per-child socket, proxy injection and
//     SetScriptHub included (wave 2 had only unit coverage).
//
//   - TestScriptChildEndToEnd drives the whole runtime: a driver script that
//     spawns a fundi child on a fake-LLM seat (a local providers.toml
//     pointing at a canned Anthropic Messages server), reports progress,
//     waits for the worker's settle on its Receive stream, sets a result and
//     exits 0; the parent's history carries the report and the settle
//     fragment carries the result. A second script that tries to kill a
//     sibling tree is denied, and the sibling survives.
//
// Every script child here needs python3; the tests say so when skipping.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"os/exec"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
)

// ─── fixtures: fake LLM + drivers ────────────────────────────────────────────

// fakeAnthropicServer answers the Anthropic Messages API with one canned
// end_turn reply per request. It is the fake-LLM seat a spawned fundi child
// runs on: the daemon routes "fakellm/mini" to it through a providers.toml
// [providers.fakellm] block (kind = "anthropic", base_url = this server).
func fakeAnthropicServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_fake_1","type":"message","role":"assistant",` +
			`"model":"mini","content":[{"type":"text","text":"done"}],` +
			`"stop_reason":"end_turn","stop_sequence":null,` +
			`"usage":{"input_tokens":10,"output_tokens":5,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeFakeProviders writes a providers.toml routing provider "fakellm" at
// the fake server and returns its path — the RAFIKI_PROVIDERS value the
// daemon boots with. The file REPLACES the shipped registry, so the shipped
// anthropic entry is spelled out too; an invalid registry is not a boot
// failure, it silently falls back to the shipped defaults (main.go), which
// would strand this test on a fixture typo without any failing spawn until
// the first turn.
func writeFakeProviders(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.toml")
	// A registry REPLACES the shipped set, and default_provider must name a
	// provider defined in the same file — so the shipped anthropic entry is
	// spelled out here, exactly as a real deployment would.
	body := fmt.Sprintf(`default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.fakellm]
kind = "anthropic"
base_url = %q
`, baseURL)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write providers fixture: %v", err)
	}
	return path
}

// scriptDaemon is a bootScriptDaemon result: the daemon plus the pieces a
// RESTART test needs to boot its successor against the same identity and the
// same fake-LLM seat.
type scriptDaemon struct {
	*daemon
	// id is the daemon id this daemon booted under; a successor reclaims its
	// rows by claiming the same id.
	id string
	// providersPath is the providers.toml fixture routing "fakellm" at the
	// fake server.
	providersPath string
}

// bootScriptDaemon boots a DB-backed daemon whose LLM traffic goes to the
// fake server, with a fast event buffer (the settle fragments aimed at a
// script child are busy-deferred by the buffer, since a script child is
// never idle while it runs; the fast MaxWait keeps the test honest and quick)
// and no executor pool (fundi children run in-process).
func bootScriptDaemon(t *testing.T) *scriptDaemon {
	t.Helper()
	fake := fakeAnthropicServer(t)
	providers := writeFakeProviders(t, fake.URL)
	id := nextDaemonID()
	extra := append(noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"RAFIKI_PROVIDERS="+providers,
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
	)
	d := bootDaemonDB(t, id, extra...)
	t.Cleanup(func() {
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})
	return &scriptDaemon{daemon: d, id: id, providersPath: providers}
}

// scriptUser mints the test's user through the real CLI and returns the
// token (for direct Connect calls against the face) and the config dir whose
// profile carries it. The pymodule a script child runs must be saved under
// the SAME owner the child is spawned with — the materializer reads the
// owner's own saved modules — so every spawn and every put in these tests
// run as this one identity.
func scriptUser(t *testing.T, d *daemon) (token, configDir string) {
	t.Helper()
	configDir = t.TempDir()
	userCmd := cliCmdIn(t, d, configDir, "--output", "json", "user", "create", operatorName())
	var userStderr strings.Builder
	userCmd.Stderr = &userStderr
	userOut, err := userCmd.Output()
	if err != nil {
		t.Fatalf("user create failed: %v\nstdout: %s\nstderr: %s", err, userOut, userStderr.String())
	}
	tokenPath := filepath.Join(configDir, "rafiki", "profiles", "it", "token")
	b, err := os.ReadFile(tokenPath)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		t.Fatalf("user create did not leave a token at %s: %v", tokenPath, err)
	}
	return strings.TrimSpace(string(b)), configDir
}

// putPymodule saves a driver script through the real CLI, authenticating with
// the token `scriptUser` minted, so the module is owned by the same user that
// spawns the script child (the materializer reads the OWNER's modules).
func putPymodule(t *testing.T, d *daemon, configDir, name, code string) {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), name+".py")
	if err := os.WriteFile(fixture, []byte(code), 0o600); err != nil {
		t.Fatalf("write %s fixture: %v", name, err)
	}
	var stderr strings.Builder
	cmd := cliCmdIn(t, d, configDir, "py", "put", name, "--file", fixture)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("py put %s: %v\nstderr: %s", name, err, stderr.String())
	}
	if !strings.Contains(string(out), "saved") {
		t.Fatalf("py put %s output %q does not announce the save", name, out)
	}
}

// bearerTransport injects the profile's token on every request — the same
// pattern mcp_test.go's mcpBearerTransport uses, because a connect client
// carries headers per request and this plane wants them on all of them.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// faceClient is a Connect client on the daemon's proxy face, authenticated as
// the user token. The face is plain HTTP on loopback, so HTTP/1.1 suffices —
// including for the server-streaming verbs.
func faceClient(t *testing.T, d *daemon, token string) rafikiv1connect.ControlClient {
	t.Helper()
	if d.proxyURL == "" {
		t.Fatal("the daemon never announced its proxy face; nothing here can dial it")
	}
	return rafikiv1connect.NewControlClient(
		&http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}},
		d.proxyURL)
}

// childConnectClient dials an arbitrary unix socket — the per-child Connect
// socket — with a prior-knowledge h2c client, the same shape
// (*daemon).connectClient uses against connect.sock.
func childConnectClient(t *testing.T, sockPath string) rafikiv1connect.ControlClient {
	t.Helper()
	h := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sockPath)
		},
	}}
	return rafikiv1connect.NewControlClient(h, "http://child.rafiki.invalid")
}

// scriptSocketPath is where the daemon hosts a script child's socket. The
// layout is cmd/rafikid's scriptHostDir: paths.StateDir()/script-host/<childID>/,
// with childsock.SocketName inside — reconstructed here because the daemon
// binary cannot be imported. paths.StateDir() is $XDG_STATE_HOME/rafiki/state,
// one "state" leaf below the app dir the socket path names.
func scriptSocketPath(d *daemon, childID string) string {
	return filepath.Join(filepath.Dir(d.socketPath), "state", "script-host", childID, "connect.sock")
}

// scriptChildSpawn spawns one kind=script child over the operator Connect
// route and returns its id.
func (d *daemon) scriptChildSpawn(t *testing.T, client rafikiv1connect.ControlClient, req *rafikiv1.SpawnRequest) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Spawn(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("spawn script child: %v", err)
	}
	return resp.Msg.GetChildId()
}

// waitChildExited polls GetChild until the child reads exited and returns
// its summary.
func waitChildExited(t *testing.T, client rafikiv1connect.ControlClient, childID string, timeout time.Duration) *rafikiv1.ChildSummary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
		cancel()
		if err != nil {
			t.Fatalf("GetChild(%s): %v", childID, err)
		}
		if resp.Msg.GetChild().GetStatus() == "exited" {
			return resp.Msg.GetChild()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("child %s did not exit within %v", childID, timeout)
	return nil
}

// waitHistoryContains polls GetHistory(childID) until b contains substr.
func waitHistoryContains(t *testing.T, client rafikiv1connect.ControlClient, childID string, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := client.GetHistory(ctx, connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childID}))
		cancel()
		if err != nil {
			t.Fatalf("GetHistory(%s): %v", childID, err)
		}
		b, _ := json.Marshal(resp.Msg)
		last = string(b)
		if strings.Contains(last, substr) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("history of %s never contained %q; last:\n%.4000s", childID, substr, last)
}

// ─── drivers ─────────────────────────────────────────────────────────────────

// connectHelper is the stdlib-only Connect-over-unix plumbing every driver
// script starts with. No Authorization header is ever sent: the per-child
// socket is the credential, and the proxy injects it.
const scriptConnectHelper = `
import json, os, socket, sys, time
import http.client

SOCK = os.environ["RAFIKI_CHILD_CONNECT"]

class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path, timeout=30):
        super().__init__("localhost", timeout=timeout)
        self._path = path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self._path)
        self.sock = s

def call(method, body):
    conn = UnixHTTP(SOCK)
    try:
        conn.request("POST", "/rafiki.v1.Control/" + method,
                     body=json.dumps(body).encode(),
                     headers={"Content-Type": "application/json",
                              "Connect-Protocol-Version": "1"})
        resp = conn.getresponse()
        data = resp.read()
        if resp.status != 200:
            raise RuntimeError("HTTP %d %s" % (resp.status, data.decode()))
        return json.loads(data) if data else {}
    finally:
        conn.close()

def read_envelope(resp):
    hdr = b""
    while len(hdr) < 5:
        chunk = resp.read(5 - len(hdr))
        if not chunk:
            raise RuntimeError("stream ended early")
        hdr += chunk
    n = int.from_bytes(hdr[1:5], "big")
    payload = b""
    while len(payload) < n:
        chunk = resp.read(n - len(payload))
        if not chunk:
            raise RuntimeError("stream ended mid-message")
        payload += chunk
    return json.loads(payload)

def env_leaks():
    return sorted(k for k in os.environ
                  if k.startswith(("RAFIKI_", "ANTHROPIC_", "OPENROUTER_"))
                  and k != "RAFIKI_CHILD_CONNECT")
`

// scriptDriverCode is the E2E driver: spawn a fundi child, report progress,
// wait for the worker's settle on the Receive stream, set a result, exit 0.
// Every step's failure is recorded on the probe file the test passes as
// --probe-out, because stderr is only dumped to disk on exit and the ring
// carries stdout only.
const scriptDriverCode = scriptConnectHelper + `
import traceback

probe = sys.argv[sys.argv.index("--probe-out") + 1] if "--probe-out" in sys.argv else "/dev/null"


def run_driver():
    leaks = env_leaks()
    if leaks:
        with open(probe, "a") as f:
            f.write("LEAKS: %r\n" % (leaks,))
        sys.exit(7)

    cwd = os.getcwd()
    model = sys.argv[sys.argv.index("--model") + 1]
    spawned = call("Spawn", {"cwd": cwd, "kind": "fundi", "model": model})
    child = spawned["childId"]
    call("Report", {"kind": "progress",
                    "data_json": json.dumps({"stage": "spawned", "child": child})})
    call("Send", {"childId": child, "mode": "SEND_MODE_PROMPT",
                  "blocks": [{"text": {"text": "one turn, please"}}]})

    # The worker's settle fragment reaches this inbox as a Text message on
    # the Receive stream (the daemon's event buffer flushes it there, and
    # the inbox delivery defers to this stream).
    # Server-streaming over the Connect protocol is enveloped AND carries the
    # streaming content type: application/connect+json (application/json is
    # the unary codec). The request is one envelope: flags byte, 4-byte
    # big-endian length, payload.
    req_payload = json.dumps({}).encode()
    conn = UnixHTTP(SOCK, timeout=120)
    conn.request("POST", "/rafiki.v1.Control/Receive",
                 body=b"\x00" + len(req_payload).to_bytes(4, "big") + req_payload,
                 headers={"Content-Type": "application/connect+json",
                          "Connect-Protocol-Version": "1"})
    resp = conn.getresponse()
    if resp.status != 200:
        with open(probe, "a") as f:
            f.write("RECEIVE-STATUS: %d\n" % resp.status)
        sys.exit(9)
    settle = None
    deadline = time.time() + 90
    while time.time() < deadline:
        try:
            msg = read_envelope(resp)
        except Exception as exc:
            with open(probe, "a") as f:
                f.write("STREAM-READ-FAIL: %r\n" % (exc,))
            sys.exit(10)
        if "text" in msg and "settled" in msg["text"].get("text", ""):
            settle = msg["text"]["text"]
            break
        if "stop" in msg:
            with open(probe, "a") as f:
                f.write("STREAM-STOPPED: %r\n" % (msg,))
            sys.exit(11)
    conn.close()
    if settle is None:
        with open(probe, "a") as f:
            f.write("NO-SETTLE\n")
        sys.exit(12)

    call("Report", {"kind": "progress",
                    "data_json": json.dumps({"stage": "settled", "child": child})})
    call("SetResult", {"result_json": json.dumps({"child": child, "settle_seen": True})})
    sys.exit(0)


try:
    run_driver()
except Exception:
    with open(probe, "a") as f:
        f.write(traceback.format_exc())
    sys.exit(1)
`

// scriptDenierCode tries to kill the sibling tree named in argv[1]: denied
// by the childScoped gate, which the script reports as its result.
const scriptDenierCode = scriptConnectHelper + `
import traceback

sibling = sys.argv[1]
probe = sys.argv[sys.argv.index("--probe-out") + 1] if "--probe-out" in sys.argv else "/dev/null"
with open(probe, "a") as f:
    f.write("SOCK=%s argv=%r\n" % (SOCK, sys.argv))
try:
    call("Kill", {"childId": sibling})
    result = {"denied": False, "error": "kill unexpectedly succeeded"}
except Exception as exc:
    # The verdict is the finding; the exception rides the probe file the
    # test reads on failure (stderr is only dumped to disk on exit, and the
    # ring carries stdout only).
    with open(probe, "a") as f:
        f.write("DENIER-EXC: %r\n" % (exc,))
    result = {"denied": "permission_denied" in str(exc), "error": str(exc)[:200]}
try:
    call("SetResult", {"result_json": json.dumps(result)})
except Exception as exc:
    with open(probe, "a") as f:
        f.write("DENIER-SETRESULT-EXC: %r\n" % (exc,))
sys.exit(0 if result["denied"] else 9)
`

// scriptSleeperCode stays alive so the test can drive its socket: prints one
// line (the provider's first-response trigger), then waits to be killed.
const scriptSleeperCode = `
import time
print("awake", flush=True)
time.sleep(120)
`

// ─── wave-2 handoff: the verbs pinned against the real wiring ────────────────

// TestConnectScriptVerbsOnTheConnectPlane is the end-to-end pin of
// Report/Receive/SetResult that wave 2 could not write: a real script-child
// credential — the per-child socket — against the real daemon, real hub
// wiring (SetScriptHub), real inbox and real event log.
//
// The script child here is TOP-LEVEL, so its Report lands in its OWN durable
// event log (script_report), where StreamEvents can observe it.
func TestConnectScriptVerbsOnTheConnectPlane(t *testing.T) {
	t.Parallel()
	if _, err := lookPython3(); err != nil {
		t.Skip("python3 not available: script children need an interpreter")
	}
	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	token, configDir := scriptUser(t, d)
	opClient := faceClient(t, d, token)

	putPymodule(t, d, configDir, "verb_probe_it", scriptSleeperCode)
	scriptID := d.scriptChildSpawn(t, opClient, &rafikiv1.SpawnRequest{
		Cwd:    t.TempDir(),
		Kind:   "script",
		Script: &rafikiv1.SpawnRequest_ScriptSpec{Repo: "local", Script: "verb_probe_it"},
	})
	sockPath := scriptSocketPath(d, scriptID)

	// The socket must exist, and briefly: it appears when the child starts.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		gctx, gcancel := context.WithTimeout(context.Background(), 10*time.Second)
		sum, gerr := opClient.GetChild(gctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: scriptID}))
		gcancel()
		diag := fmt.Sprintf("GetChild: %v", gerr)
		if gerr == nil {
			diag = fmt.Sprintf("status=%s exitCode=%v labels=%v", sum.Msg.GetChild().GetStatus(), sum.Msg.GetChild().GetExitCode(), sum.Msg.GetChild().GetLabels())
		}
		t.Fatalf("per-child socket never appeared at %s: %v; %s\n\n-- daemon stderr tail --\n%s",
			sockPath, err, diag, d.stderr.tail(200000))
	}
	childClient := childConnectClient(t, sockPath)

	// SetResult through the socket: stored, then visible to the operator.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := childClient.SetResult(ctx, connect.NewRequest(&rafikiv1.SetResultRequest{
		ResultJson: `{"ok": true, "verbs": 3}`,
	})); err != nil {
		t.Fatalf("SetResult through the per-child socket: %v", err)
	}
	gctx, gcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer gcancel()
	got, err := opClient.GetChild(gctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: scriptID}))
	if err != nil {
		t.Fatalf("GetChild: %v", err)
	}
	if got.Msg.GetChild().GetResult() != `{"ok": true, "verbs": 3}` {
		t.Fatalf("GetChild().result = %q, want the stored JSON", got.Msg.GetChild().GetResult())
	}

	// Report through the socket: a top-level script's report is appended to
	// its OWN durable event log as a script_report event — observable on
	// StreamEvents, which is the only window onto a script child's log
	// (script children have no conversation for GetHistory). The stream is
	// opened WITH a cursor so the durable event replays; an uncensored
	// "from now" stream would block its headers on the first live event.
	rctx, rcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer rcancel()
	if _, err := childClient.Report(rctx, connect.NewRequest(&rafikiv1.ReportRequest{
		Kind:     "progress",
		DataJson: `{"stage":"verb-pin"}`,
	})); err != nil {
		t.Fatalf("Report through the per-child socket: %v", err)
	}
	evctx, evcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer evcancel()
	evStream, err := opClient.StreamEvents(evctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: scriptID}},
		Cursor:  &rafikiv1.EventCursor{Ordinals: map[string]int32{scriptID: 0}},
	}))
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	defer func() { _ = evStream.Close() }()
	deadline2 := time.After(15 * time.Second)
	sawReport := false
	for !sawReport {
		select {
		case <-deadline2:
			t.Fatal("the top-level script's report never appeared as a script_report event")
		default:
		}
		if !evStream.Receive() {
			t.Fatalf("StreamEvents ended early: %v", evStream.Err())
		}
		if ev := evStream.Msg().GetScriptReport(); ev != nil && strings.Contains(ev.GetDataJson(), "verb-pin") {
			sawReport = true
		}
	}

	// Send FIRST, then open Receive: the hub's handler sends no headers
	// until its first message, so a stream opened on an empty inbox blocks
	// its open on the first poll. The message sent first is delivered on the
	// first poll after the open.
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer sendCancel()
	if _, err := opClient.Send(sendCtx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: scriptID,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "hello script"}},
		}},
	})); err != nil {
		t.Fatalf("Send to the script child: %v", err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer scancel()
	recvStream, err := childClient.Receive(sctx, connect.NewRequest(&rafikiv1.ReceiveRequest{}))
	if err != nil {
		t.Fatalf("Receive through the per-child socket: %v", err)
	}
	msgDeadline := time.After(15 * time.Second)
	gotMsg := false
	for !gotMsg {
		select {
		case <-msgDeadline:
			t.Fatal("Receive stream never delivered the sent message")
		default:
		}
		if !recvStream.Receive() {
			t.Fatalf("Receive stream ended early: %v", recvStream.Err())
		}
		if txt := recvStream.Msg().GetText(); txt != nil && strings.Contains(txt.GetText(), "hello script") {
			gotMsg = true
		}
	}
	_ = gotMsg

	// The socket speaks with the child's voice and nothing more: an operator
	// verb through it is refused by the policy gate (a per-child credential
	// on a userOnly procedure), proving the socket carries a child identity,
	// not anonymous local trust.
	kctx, kcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer kcancel()
	if _, err := childClient.ListExecutors(kctx, connect.NewRequest(&rafikiv1.ListExecutorsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("ListExecutors through the per-child socket = %v, want %v", err, connect.CodePermissionDenied)
	}

	// Cleanup: kill the sleeper so the daemon's shutdown does not wait it
	// out. Explicit timeouts: a script that holds no Receive stream is told
	// nothing before the signal rungs, so the default 180s graceful window
	// would be spent waiting for an exit that never comes — the same ladder
	// any stdin-ignoring child gets, exercised here with short rungs.
	kctx2, kcancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer kcancel2()
	if _, err := opClient.Kill(kctx2, connect.NewRequest(&rafikiv1.KillRequest{
		ChildId:           scriptID,
		ShutdownTimeoutMs: 2000,
		KillTimeoutMs:     2000,
	})); err != nil {
		t.Fatalf("kill the sleeper: %v", err)
	}
}

// safeCode dereferences an optional exit code for logging.
func safeCode(code *int32) int32 {
	if code == nil {
		return -1
	}
	return *code
}

// lookPython3 reports whether an interpreter is available for the drivers.
func lookPython3() (string, error) { return exec.LookPath("python3") }

// ─── restart: a locally hosted script dies with its daemon ───────────────────

// TestScriptChildRestartSettlesFailed pins 3.5: a locally hosted script
// child dies with its daemon; the restarted daemon loads its still-live row,
// settles it failed with the reason "daemon restarted", and the PARENT hears
// it — the settle fragment lands in the parent's conversation on its next
// turn, and the script child reads exited (never running-with-no-process).
func TestScriptChildRestartSettlesFailed(t *testing.T) {
	t.Parallel()
	if _, err := lookPython3(); err != nil {
		t.Skip("python3 not available: script children need an interpreter")
	}
	d1 := bootScriptDaemon(t)
	token, configDir := scriptUser(t, d1.daemon)
	client1 := faceClient(t, d1.daemon, token)

	// The parent seat, resumed by daemon 2 after the crash; its turns run on
	// the fake-LLM seat.
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	presp, err := client1.Spawn(pctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/tmp", Kind: "fundi", Model: "fakellm/mini", Name: "restart-parent",
	}))
	pcancel()
	if err != nil {
		t.Fatalf("spawn parent: %v", err)
	}
	parent := presp.Msg.GetChildId()

	putPymodule(t, d1.daemon, configDir, "restart_sleeper_it", scriptSleeperCode)
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	sresp, err := client1.Spawn(sctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:           "/tmp",
		Kind:          "script",
		ParentChildId: parent,
		Name:          "restart-victim",
		Script:        &rafikiv1.SpawnRequest_ScriptSpec{Repo: "local", Script: "restart_sleeper_it"},
	}))
	scancel()
	if err != nil {
		t.Fatalf("spawn script child: %v", err)
	}
	victim := sresp.Msg.GetChildId()

	// The socket's existence is the proof the process started; the crash
	// must find it LIVE (a row still reading idle/streaming/spawning).
	sockPath := scriptSocketPath(d1.daemon, victim)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("victim's per-child socket never appeared: %v", err)
	}

	// Crash, not shutdown: SIGKILL writes nothing, so the row keeps its live
	// status — the shape recovery classifies as "died with the daemon".
	if err := d1.proc.Process.Kill(); err != nil {
		t.Fatalf("kill daemon: %v", err)
	}
	_ = d1.proc.Wait()

	// Daemon 2 takes the same daemon id (reclaiming its own rows) and the
	// same providers fixture. Its fresh home is fine: recovery only reads
	// rows and settles.
	d2 := bootDaemonDB(t, d1.id, append([]string{}, append(
		noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"RAFIKI_PROVIDERS="+d1.providersPath,
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
	)...)...)
	client2 := faceClient(t, d2, token)

	// The victim reads exited, and its exit carries the settle reason: the
	// parent's conversation names the daemon restart.
	vsum := waitChildExited(t, client2, victim, 60*time.Second)
	if vsum.ExitCode != nil {
		t.Fatalf("recovered victim reports exit code %d; recovery settles without one", *vsum.ExitCode)
	}
	waitHistoryContains(t, client2, parent, "failed (daemon restarted)", 120*time.Second)
}

// ─── the wave-3 runtime, end to end ──────────────────────────────────────────

// TestScriptChildEndToEnd drives the full wave-3 runtime through the real
// daemon and the real CLI: a saved pymodule run as a script child spawns a
// fundi child on a fake-LLM seat, reports progress, waits for the worker's
// settle on its Receive stream, sets a result and exits 0. The parent's
// history carries the report; the settle fragment carries the result. A
// second script that tries to kill a sibling tree is denied, and the sibling
// survives.
func TestScriptChildEndToEnd(t *testing.T) {
	t.Parallel()
	if _, err := lookPython3(); err != nil {
		t.Skip("python3 not available: script children need an interpreter")
	}
	sd := bootScriptDaemon(t)
	d := sd.daemon
	token, configDir := scriptUser(t, d)
	opClient := faceClient(t, d, token)

	putPymodule(t, d, configDir, "script_driver_it", scriptDriverCode)
	putPymodule(t, d, configDir, "script_denier_it", scriptDenierCode)

	// The parent seat: a fundi child the report and the settle fragment are
	// pushed into. Its turns run on the fake-LLM seat.
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	presp, err := opClient.Spawn(pctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  "fundi",
		Model: "fakellm/mini",
		Name:  "e2e-parent",
	}))
	pcancel()
	if err != nil {
		t.Fatalf("spawn parent: %v", err)
	}
	parent := presp.Msg.GetChildId()

	// An outsider tree the second script must not reach.
	octx, ocancel := context.WithTimeout(context.Background(), 30*time.Second)
	oresp, err := opClient.Spawn(octx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  "fundi",
		Model: "fakellm/mini",
		Name:  "e2e-outsider",
	}))
	ocancel()
	if err != nil {
		t.Fatalf("spawn outsider: %v", err)
	}
	outsider := oresp.Msg.GetChildId()

	cwd := t.TempDir()
	driverProbe := filepath.Join(t.TempDir(), "driver.log")
	// 1. The driver: spawns its worker on the fake seat, reports, waits for
	//    the settle, sets its result, exits 0.
	driverID := d.scriptChildSpawn(t, opClient, &rafikiv1.SpawnRequest{
		Cwd:           cwd,
		Kind:          "script",
		ParentChildId: parent,
		Name:          "e2e-driver",
		Script: &rafikiv1.SpawnRequest_ScriptSpec{
			Repo:   "local",
			Script: "script_driver_it",
			Args:   []string{"--model", "fakellm/mini", "--probe-out", driverProbe},
		},
	})

	// 2. The denier: tries to kill the sibling tree through its own socket.
	probeOut := filepath.Join(t.TempDir(), "denier.log")
	dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
	dresp, err := opClient.Spawn(dctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:  cwd,
		Kind: "script",
		Script: &rafikiv1.SpawnRequest_ScriptSpec{
			Repo:   "local",
			Script: "script_denier_it",
			Args:   []string{outsider, "--probe-out", probeOut},
		},
	}))
	dcancel()
	if err != nil {
		t.Fatalf("spawn denier: %v", err)
	}
	denier := dresp.Msg.GetChildId()

	// 3. The denier settles fast: the kill is refused by the childScoped
	//    gate, and its result says so.
	denierSum := waitChildExited(t, opClient, denier, 60*time.Second)
	if code := denierSum.ExitCode; code == nil || *code != 0 {
		probe, perr := os.ReadFile(probeOut)
		if perr != nil {
			probe = []byte(fmt.Sprintf("probe file unread: %v", perr))
		}
		t.Fatalf("denier exited with %d and result %q; its log:\n%.4000s", *code, denierSum.GetResult(), probe)
	}
	var verdict map[string]any
	if err := json.Unmarshal([]byte(denierSum.GetResult()), &verdict); err != nil || verdict["denied"] != true {
		t.Fatalf("denier result = %q, want a parsed {denied: true} record (err=%v)", denierSum.GetResult(), err)
	}
	// The sibling tree is untouched.
	octx2, ocancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	osum, err := opClient.GetChild(octx2, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: outsider}))
	ocancel2()
	if err != nil {
		t.Fatalf("GetChild(outsider): %v", err)
	}
	if osum.Msg.GetChild().GetStatus() == "exited" {
		t.Fatal("the sibling tree was killed by the denied call; it must survive")
	}

	// 4. The driver settles done, with its result stored.
	drvSum := waitChildExited(t, opClient, driverID, 120*time.Second)
	if drvSum.ExitCode == nil || *drvSum.ExitCode != 0 {
		probe, perr := os.ReadFile(driverProbe)
		if perr != nil {
			probe = []byte(fmt.Sprintf("probe file unread: %v", perr))
		}
		t.Fatalf("driver exit code = %d (nil=%v), want 0; its log:\n%.6000s", safeCode(drvSum.ExitCode), drvSum.ExitCode == nil, probe)
	}
	var outcome map[string]any
	if err := json.Unmarshal([]byte(drvSum.GetResult()), &outcome); err != nil || outcome["settle_seen"] != true {
		t.Fatalf("driver result = %q, want a parsed {settle_seen: true} record (err=%v)", drvSum.GetResult(), err)
	}

	// 5. The parent heard the driver: the report fragment and the settle
	//    fragment, the latter carrying the driver's stored result. The
	//    parent's turns (fake-LLM) drain its event buffer into its
	//    conversation, so GetHistory sees both fragments verbatim.
	waitHistoryContains(t, opClient, parent, "reported progress", 60*time.Second)
	// The result rides the settle fragment verbatim. The substring stays
	// quote-free deliberately: the history is asserted through its own JSON
	// rendering, where embedded quotes are escaped.
	waitHistoryContains(t, opClient, parent, "final result of "+driverID+": ", 60*time.Second)
	waitHistoryContains(t, opClient, parent, ") done. Read what it did", 60*time.Second)
}

// scriptCliDriverCode is the CLI test's driver: it records the argv it was
// handed (the after-`--` tail) and exits 0. No Connect calls, so the test
// pins the CLI → spec → runtime handoff without the per-child socket.
const scriptCliDriverCode = `
import json, sys

args = sys.argv[1:]
probe = args[args.index("--probe-out") + 1]
with open(probe, "w") as f:
    f.write(json.dumps({"argv": args}))
`

// TestScriptChildFromTheCLI exercises the wave-5 entry point end to end:
// `rafiki create -d --kind script --pymodule <repo>:<script> [-- args]`
// through the real client binary. The daemon has NO executor pool, so the
// test also pins the pool-less pre-flight tolerance: before wave 5 the CLI's
// ListExecutors auto-resolve hard-failed the create on a daemon that would
// happily have hosted the child locally, and a profile default model (a
// declaration about LLM children) must not ride the spawn — with the strip
// gone the daemon refuses the whole create on `field "model" does not apply`.
func TestScriptChildFromTheCLI(t *testing.T) {
	t.Parallel()
	if _, err := lookPython3(); err != nil {
		t.Skip("python3 not available: script children need an interpreter")
	}
	sd := bootScriptDaemon(t)
	d := sd.daemon
	token, configDir := scriptUser(t, d)
	opClient := faceClient(t, d, token)

	putPymodule(t, d, configDir, "script_cli_it", scriptCliDriverCode)

	probePath := filepath.Join(t.TempDir(), "cli-probe.json")
	cwd := t.TempDir()
	cmd := cliCmdIn(t, d, configDir, "--output", "json", "create", "-d",
		"--kind", "script", "--cwd", cwd,
		"--pymodule", "local:script_cli_it",
		"--", "--probe-out", probePath)
	// The create call's profile carries a default model — a declaration about
	// LLM children that must not reach a script spawn. Rewritten AFTER
	// cliCmdIn (which rewrites the manifest itself) and BEFORE the subprocess
	// starts, so the env is in place but the manifest is the model-carrying
	// one.
	manifest := fmt.Sprintf("[profile.it]\nsocket = %q\nmodel = %q\n",
		d.socketPath, "anthropic/claude-sonnet-4")
	if err := os.WriteFile(filepath.Join(configDir, "rafiki", "profiles.toml"),
		[]byte(manifest), 0o600); err != nil {
		t.Fatalf("rewrite profiles.toml with a default model: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("create --kind script: %v\nstderr: %s", err, stderr.String())
	}
	var created struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.ChildID == "" {
		t.Fatalf("create output %q does not name a childId (err=%v)", out, err)
	}
	childID := created.ChildID

	sum := waitChildExited(t, opClient, childID, 60*time.Second)
	if sum.ExitCode == nil || *sum.ExitCode != 0 {
		probe, perr := os.ReadFile(probePath)
		if perr != nil {
			probe = []byte(fmt.Sprintf("probe file unread: %v", perr))
		}
		t.Fatalf("script child exited %v; its log/probe:\n%.4000s", sum.ExitCode, probe)
	}
	if sum.GetKind() != "script" {
		t.Errorf("GetChild kind = %q, want script", sum.GetKind())
	}
	// The name defaults to the script's name — the same default the
	// pymodule_start tool applies, so both entry points read the same way.
	if sum.GetName() != "script_cli_it" {
		t.Errorf("GetChild name = %q, want the script's name", sum.GetName())
	}
	probeRaw, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatalf("probe file never written: %v", err)
	}
	var probe struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(probeRaw, &probe); err != nil {
		t.Fatalf("probe %s does not parse: %v", probeRaw, err)
	}
	want := []string{"--probe-out", probePath}
	if len(probe.Argv) != len(want) || probe.Argv[0] != want[0] || probe.Argv[1] != want[1] {
		t.Errorf("script argv = %v, want %v (the after-`--` tail must reach the process verbatim)", probe.Argv, want)
	}
}
