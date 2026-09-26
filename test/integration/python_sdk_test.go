// SPDX-License-Identifier: Apache-2.0

package integration_test

// The Python SDK (sdk/python), end to end on the real daemon — wave 6.
//
// Three tests live here, all of them driving the REAL SDK package from Go:
//
//   - TestPythonSDK_TransportRetry needs no daemon: a fake Connect server in
//     the driver exercises the wire plumbing (enveloped streams, the
//     end-of-stream error envelope, error mapping, and the bounded-backoff
//     retry that fires on the proxy's 503-unavailable body only).
//
//   - TestPythonSDK_StandaloneLifecycle runs Client.from_profile() against a
//     scratch daemon's connect.sock: spawn (with the fake-LLM seat), send,
//     list with a label filter, wait for the settle, export the transcript,
//     stop. export is the test that pins the Connect-plane spawn's owner
//     attribution: ConversationExport is owner-scoped, so it answers
//     not-found when the child's conversation row lands unattributed —
//     which is exactly what an un-defaulted empty spawn kind used to do
//     (controller.Spawn now resolves the fundi default itself).
//
//   - TestPythonSDK_ScriptChild runs a script child whose pymodule IS the
//     SDK: Client.inside() over the per-child socket, report/receive/result
//     and the work-first/stop-last Receive contract, watched to its settle
//     by the profile-side driver.
//
// Every test needs python3 with httpx — both the drivers run here and the
// daemon's script children (RAFIKI_PYMODULE_PYTHON points them at the same
// interpreter). When either is missing the tests SKIP, and the skip says so:
// the reason names the interpreter check that failed, and `make test` prints
// the same condition before the run, so a compacted wrapper cannot turn "the
// SDK was never exercised" into a green that looks like a pass.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sdkPython returns the interpreter the SDK tests run (RAFIKI_TEST_SDK_PYTHON
// when set — a venv python is the usual reason — else python3), skipping
// LOUDLY when it cannot import httpx. The skip reason names the check and the
// fix, so it survives both -v runs and the Makefile's warning.
func sdkPython(t *testing.T) string {
	t.Helper()
	py := os.Getenv("RAFIKI_TEST_SDK_PYTHON")
	if py == "" {
		py = "python3"
	}
	if _, err := exec.LookPath(py); err != nil {
		t.Skipf("sdk/python tests skipped: no %s on PATH (%v); install python3 with httpx, or set RAFIKI_TEST_SDK_PYTHON", py, err)
	}
	out, err := exec.Command(py, "-c", "import httpx").CombinedOutput()
	if err != nil {
		t.Skipf("sdk/python tests skipped: %s cannot import httpx (%v: %s); pip install httpx, or point RAFIKI_TEST_SDK_PYTHON at an interpreter that has it", py, err, strings.TrimSpace(string(out)))
	}
	return py
}

// bootSDKDaemon boots a script-capable DB-backed daemon (the fake-LLM seat
// and fast event buffer of bootScriptDaemon) whose script children run under
// the SAME interpreter the drivers use, so a pymodule can import the SDK.
func bootSDKDaemon(t *testing.T, python string) *scriptDaemon {
	t.Helper()
	fake := fakeAnthropicServer(t)
	providers := writeFakeProviders(t, fake.URL)
	id := nextDaemonID()
	extra := append(noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"RAFIKI_PROVIDERS="+providers,
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
		// The local script runner resolves its interpreter from the daemon's
		// own environment at spawn time; pinning it here keeps a pymodule's
		// `import rafiki` on the interpreter that has httpx.
		"RAFIKI_PYMODULE_PYTHON="+python,
	)
	d := bootDaemonDB(t, id, extra...)
	t.Cleanup(func() {
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})
	return &scriptDaemon{daemon: d, id: id, providersPath: providers}
}

// sdkBinDir creates a directory holding a `rafiki` symlink to the test-built
// CLI, so a Python driver's shutil.which("rafiki") finds the binary that
// speaks `profile show -o json` — a developer's installed rafiki may predate
// it, and the SDK's from_profile error would then say so instead of running.
func sdkBinDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(cliPath, filepath.Join(dir, "rafiki")); err != nil {
		t.Fatalf("symlink rafiki: %v", err)
	}
	return dir
}

// sdkDriverEnv builds the environment one driver runs under: the scratch
// profile dir (XDG_CONFIG_HOME), the sdk bin dir first on PATH, and every
// ambient rafiki variable blanked — the same isolation cliCmd applies, for a
// driver that resolves its daemon through the profile like a real user.
func sdkDriverEnv(t *testing.T, configDir, binDir string) []string {
	t.Helper()
	path := os.Getenv("PATH")
	if binDir != "" {
		path = binDir + string(os.PathListSeparator) + path
	}
	env := append(os.Environ(),
		"RAFIKI_URL=", "RAFIKI_TOKEN=", "RAFIKI_SOCKET=",
		"RAFIKI_DEFAULT_MODEL=", "RAFIKI_DEFAULT_PRESET=", "RAFIKI_DEFAULT_LABELS=",
		"RAFIKI_PROFILE=",
		"RAFIKI_CHILD_CONNECT=",
		"XDG_STATE_HOME="+cliStateDir(),
		"XDG_CONFIG_HOME="+configDir,
		"PATH="+path,
	)
	return env
}

// runSDKScript writes one driver script and runs it, failing with the
// script's probe file (its stdout, and its stderr) on a nonzero exit — the
// script_child_test pattern, because a driver's traceback is the diagnosis.
func runSDKScript(t *testing.T, py, source string, env []string, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "driver.py")
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	probe := filepath.Join(dir, "probe.log")
	argv := append([]string{script, "--probe-out", probe}, args...)
	cmd := exec.Command(py, argv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		probeBody := ""
		if b, readErr := os.ReadFile(probe); readErr == nil {
			probeBody = string(b)
		}
		t.Fatalf("driver failed: %v\nstdout+stderr:\n%s\nprobe:\n%s", err, out, probeBody)
	}
	return string(out)
}

// ─── 1. transport, without a daemon ──────────────────────────────────────────

// sdkTransportDriver is a fake Connect server plus assertions: unary retry on
// the 503-unavailable body (twice, then success), no retry on
// permission_denied, the enveloped Receive stream (work first, stop last,
// end-of-stream envelope closing cleanly), settled() over a server stream of
// agent_status events, and the generated codec's contract (oneof guards,
// 64-bit ints as strings on the wire, presence fields).
const sdkTransportDriver = `
import json, struct, sys, threading, time, traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, "%SDKDIR%")
from rafiki import Client, ConnectError
from rafiki._gen import control_pb, event_pb

probe = sys.argv[sys.argv.index("--probe-out") + 1]
def note(s):
    with open(probe, "a") as f:
        f.write(s + "\n")

STATE = {"unary_503s": 2, "stream_503s": 0, "requests": []}

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        ct = self.headers.get("Content-Type")
        proto = self.headers.get("Connect-Protocol-Version")
        auth = self.headers.get("Authorization")
        STATE["requests"].append((self.path, ct, proto, auth))
        if self.path.endswith("/Spawn"):
            if ct != "application/json" or proto != "1":
                self._err(400, "invalid_argument", "content-type")
                return
            if STATE["unary_503s"] > 0:
                STATE["unary_503s"] -= 1
                self._err(503, "unavailable", "daemon restarting")
                return
            self._json({"childId": "c_" + json.loads(body)["kind"]})
            return
        if self.path.endswith("/Receive"):
            if ct != "application/connect+json":
                self._err(400, "invalid_argument", "stream content type")
                return
            flags = body[0]
            n = struct.unpack(">I", body[1:5])[0]
            payload = json.loads(body[5:5 + n])
            assert payload == {}, payload
            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.end_headers()
            env(self.wfile, {"text": {"text": "work one", "mode": "SEND_MODE_PROMPT", "messageIds": ["r1"]}})
            if STATE.get("truncate"):
                # Close WITHOUT the end-of-stream envelope: a truncated
                # stream. The client must translate the internal StreamEnded,
                # not leak it.
                return
            env(self.wfile, {"stop": {"reason": "stopping"}})
            env(self.wfile, {"error": None}, end=True)
            return
        if self.path.endswith("/StreamEvents"):
            if STATE["stream_503s"] > 0:
                STATE["stream_503s"] -= 1
                self._err(503, "unavailable", "gap")
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.end_headers()
            env(self.wfile, {"childId": "c_1", "ordinal": 7, "tsUnixMs": "1727", "agentStatus": {"state": "streaming"}})
            env(self.wfile, {"childId": "c_1", "ordinal": 8, "tsUnixMs": "1728", "agentStatus": {"state": "exited"}})
            env(self.wfile, {"error": None}, end=True)
            return
        if self.path.endswith("/GetChild"):
            req = json.loads(body)
            if req.get("childId") == "c_1":
                self._json({"child": {"childId": "c_1", "status": "streaming", "latestOrdinal": 6, "labels": {}}})
            else:
                self._err(403, "permission_denied", "not yours")
            return
        self._err(404, None, None)

    def _json(self, obj):
        raw = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _err(self, status, code, message):
        raw = json.dumps({"code": code, "message": message}).encode() if code else b"<html>404</html>"
        self.send_response(status)
        self.send_header("Content-Type", "application/json" if code else "text/html")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

def env(w, obj, end=False):
    raw = json.dumps(obj).encode()
    w.write(bytes([2 if end else 0]) + struct.pack(">I", len(raw)) + raw)

def run():
    try:
        srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        port = srv.server_address[1]
        threading.Thread(target=srv.serve_forever, daemon=True).start()

        c = Client("http://127.0.0.1:%d" % port, token="tok-test", max_attempts=6)

        # 1. unary with two injected 503s: the bounded-backoff retry lands.
        cid = c.spawn(kind="script")
        assert cid == "c_script", cid
        assert STATE["requests"][-1][3] == "Bearer tok-test", STATE["requests"][-1]
        assert STATE["requests"][-1][1] == "application/json"
        note("1 unary+retry OK")

        # 2. permission_denied must NOT retry: one request, then the raise.
        before = len(STATE["requests"])
        try:
            c.get("c_nobody")
            raise AssertionError("expected ConnectError")
        except ConnectError as e:
            assert e.code == "permission_denied" and not e.unavailable, e
            assert len(STATE["requests"]) == before + 1, "permission_denied retried"
        note("2 error map OK")

        # 3. receive: work first, stop last; the stop ends the generator.
        msgs = list(c.receive())
        assert len(msgs) == 2, msgs
        assert msgs[0].text.text == "work one" and msgs[0].text.message_ids == ["r1"]
        assert msgs[1].stop.reason == "stopping"
        note("3 receive OK")

        # 4. settled over a server stream (one 503 on the open, then events).
        settles = list(c.settled(["c_1"]))
        assert [s.state for s in settles] == ["exited"], settles
        note("4 settled OK")

        # 5. generated codec: presence, oneof guard, int64 as wire strings.
        req = control_pb.SpawnRequest(kind="fundi", max_cost=0.0)
        assert req.to_dict() == {"kind": "fundi", "maxCost": 0.0}, req.to_dict()
        back = control_pb.SpawnRequest.from_dict({"kind": "script", "maxCost": 0})
        assert back.max_cost == 0.0 and back.kind == "script"
        ev = event_pb.Event.from_dict({"childId": "c_1", "ordinal": 3, "tsUnixMs": "1727", "agentStatus": {"state": "idle"}})
        assert ev.ts_unix_ms == 1727 and ev.to_dict()["tsUnixMs"] == "1727"
        try:
            control_pb.ScriptMessage(text=control_pb.ScriptMessage.Text(text="x"),
                                     stop=control_pb.ScriptMessage.Stop()).to_dict()
            raise AssertionError("oneof guard did not fire")
        except ValueError:
            pass
        note("5 codec OK")

        # 6. a stream truncated without its end-of-stream envelope: the
        # internal StreamEnded is translated (the generator ends), not
        # raised raw to callers.
        STATE["truncate"] = True
        msgs2 = list(c.receive())
        STATE["truncate"] = False
        assert len(msgs2) == 1 and msgs2[0].text.text == "work one", msgs2
        note("6 truncated-stream OK")

        # 7. an untimed settled() on a REMOTE (TCP) endpoint must carry the
        # finite idle ceiling (0.0 would mean an infinite read timeout — a
        # silently-dropped connection would hang the watch forever); a unix
        # socket client keeps the unbounded wait.
        import rafiki.client as rc
        from rafiki.connect import ConnectClient
        assert c._conn.is_unix is False, c._conn.is_unix
        assert rc.REMOTE_IDLE_CEILING == 60.0, rc.REMOTE_IDLE_CEILING
        captured = {}
        orig_stream = c._conn.stream
        def spy(method, payload, **kw):
            captured["read_timeout"] = kw.get("read_timeout")
            return orig_stream(method, payload, **kw)
        c._conn.stream = spy
        settles = list(c.settled(["c_1"]))
        assert [s.state for s in settles] == ["exited"], settles
        assert captured["read_timeout"] == 60.0, captured
        uds = ConnectClient("/tmp/rafiki-sdk-test-does-not-exist.sock")
        assert uds.is_unix is True, uds.is_unix
        uds.close()
        note("7 remote-idle-ceiling OK")
        srv.shutdown()
        note("TRANSPORT PASS")
    except Exception:
        note(traceback.format_exc())
        sys.exit(1)

run()
`

func TestPythonSDK_TransportRetry(t *testing.T) {
	t.Parallel()
	py := sdkPython(t)
	runSDKScript(t, py, strings.ReplaceAll(sdkTransportDriver, "%SDKDIR%", filepath.Join(repoRoot, "sdk", "python")), sdkDriverEnv(t, "", ""))
}

// ─── 2. the standalone profile lifecycle, on the real daemon ─────────────────

// sdkProfileDriver shells out to from_profile against the scratch daemon and
// walks the lifecycle: spawn a fundi child on the fake-LLM seat with a
// prompt, wait for its settle, export its transcript, exercise the label
// filter, stop a second child. export is the owner-attribution pin: a
// conversation whose row landed unattributed answers not-found here.
const sdkProfileDriver = `
import sys, time, traceback

sys.path.insert(0, "%SDKDIR%")
from rafiki import Client, ConnectError

probe = sys.argv[sys.argv.index("--probe-out") + 1]
def note(s):
    with open(probe, "a") as f:
        f.write(s + "\n")

def run():
    try:
        c = Client.from_profile()
        note("from_profile OK")

        model = sys.argv[sys.argv.index("--model") + 1]

        # spawn with a prompt: the child turns on the fake seat and settles.
        cid = c.spawn("one turn, please", name="sdk-life", model=model, labels={"team": "sdk", "wave": "6"})
        assert cid.startswith("c_"), cid
        states = c.wait([cid], timeout=60)
        assert states[cid] in ("idle", "exited"), states
        child = c.get(cid)
        note("waited: %s kind=%s" % (states, child.kind))
        assert child.kind == "fundi", child.kind

        # the transcript: owner-scoped, so this is the attribution pin.
        tr = c.export(cid)
        note("export OK: %d turn(s), driven_by=%s, owner=%s" % (len(tr.turns), tr.driven_by, tr.owner))
        assert tr.owner != "", "export returned an unattributed transcript"

        # label filter: the spawned child matches its own two labels, and a
        # sibling with other labels does not leak into the answer.
        other = c.spawn(name="sdk-life-other", model=model, labels={"team": "other"})
        found = [x.child_id for x in c.list(labels={"team": "sdk", "wave": "6"})]
        assert cid in found, found
        assert other not in found, found
        note("list+labels OK")

        # send lands an inbox row with a durable id.
        mid = c.send(cid, "a steer, unsteerable but durable")
        assert mid != "", mid
        note("send OK")

        # stop a child; the KillResponse carries its exit.
        kid = c.stop(other)
        assert kid.child_id == other
        note("stop OK")

        # settled() on an already-settled child answers at once.
        c.stop(cid)
        settle = list(c.settled([cid]))
        assert settle and settle[0].state in ("idle", "exited"), settle
        note("PROFILE PASS")
    except Exception:
        note(traceback.format_exc())
        sys.exit(1)

run()
`

func TestPythonSDK_StandaloneLifecycle(t *testing.T) {
	t.Parallel()
	py := sdkPython(t)
	d := bootSDKDaemon(t, py)
	_, configDir := scriptUser(t, d.daemon)
	binDir := sdkBinDir(t)
	runSDKScript(t, py,
		strings.ReplaceAll(sdkProfileDriver, "%SDKDIR%", filepath.Join(repoRoot, "sdk", "python")),
		sdkDriverEnv(t, configDir, binDir),
		"--model", "fakellm/mini")
}

// ─── 3. the script child, SDK both sides ─────────────────────────────────────

// sdkScriptWorkerPymodule is the CHILD side: a saved pymodule that is the
// script child's whole process. It talks through Client.inside() — the
// per-child socket in RAFIKI_CHILD_CONNECT — reporting progress, receiving
// its inbox (work first), storing a result with the delivered row ids, and
// stopping cleanly when the stream hands it a Stop. %SDKDIR% is substituted
// before `py put` so the run finds the SDK package.
// The worker's mode rides its argv (ScriptSpec.args): "once" exits after the
// first text — its exit IS its result, the settle path — and "stop" keeps
// waiting until the daemon's stop arrives, which is what the stop path needs
// a worker to still be alive for.
const sdkScriptWorkerPymodule = `
import sys, traceback

sys.path.insert(0, "%SDKDIR%")

argv = sys.argv[1:]
probe = argv[argv.index("--probe-out") + 1] if "--probe-out" in argv else "/dev/null"
mode = argv[argv.index("--mode") + 1] if "--mode" in argv else "once"
def note(s):
    with open(probe, "a") as f:
        f.write(s + "\n")

def run():
    try:
        from rafiki import Client
        c = Client.inside()
        c.report("progress", {"stage": "started", "mode": mode})
        for msg in c.receive():
            if msg.stop is not None:
                note("STOP: " + msg.stop.reason)
                c.result({"stopped_by": msg.stop.reason})
                break
            if msg.text is not None:
                note("TEXT: " + msg.text.text)
                c.report("progress", {"stage": "worked", "got": msg.text.text})
                c.result({"worked": msg.text.text, "ids": list(msg.text.message_ids)})
                if mode == "once":
                    break
        sys.exit(0)
    except Exception:
        note(traceback.format_exc())
        sys.exit(1)

run()
`

// sdkSupervisorDriver is the PARENT side: from_profile, spawn the script
// child, watch it settle (a script's settle is its child_exited event), read
// its stored result back, and then drive the stop path against a second
// worker: work first, stop last.
const sdkSupervisorDriver = `
import sys, time, traceback

sys.path.insert(0, "%SDKDIR%")
from rafiki import Client, ConnectError

probe = sys.argv[sys.argv.index("--probe-out") + 1]
def note(s):
    with open(probe, "a") as f:
        f.write(s + "\n")

def run():
    try:
        c = Client.from_profile()
        note("supervisor up")

        # 1. live settle: spawn the worker, hand it work, watch child_exited.
        wid = c.spawn(kind="script", name="sdk-worker",
                      script={"repo": "local", "script": "sdkworker",
                              "args": ["--probe-out", probe, "--mode", "once"]})
        assert wid.startswith("c_"), wid
        time.sleep(1.5)                       # the worker opens its stream
        c.send(wid, "hello from the SDK")
        t0 = time.time()
        settle = None
        for s in c.settled([wid]):
            settle = s
            break
        assert settle is not None, "worker never settled"
        assert time.time() - t0 < 60, "settle took too long"
        child = c.get(wid)
        note("worker settled: %s exit=%s result=%s" % (settle.state, settle.exit_code, child.result))
        assert child.result, "worker stored no result"

        # 2. stop path: a second worker gets its work, then the stop. This
        # one runs in "stop" mode so it is still alive to receive it.
        wid2 = c.spawn(kind="script", name="sdk-stopper",
                       script={"repo": "local", "script": "sdkworker",
                               "args": ["--probe-out", probe, "--mode", "stop"]})
        time.sleep(1.5)
        c.send(wid2, "any work arrives first")
        time.sleep(0.5)                       # work-first: deliver the text
        kid = c.stop(wid2)
        assert kid.child_id == wid2, kid
        states = c.wait([wid2], timeout=30)
        note("stopped: %s" % (states,))
        child2 = c.get(wid2)
        assert child2.exit_code == 0, child2.exit_code
        assert "stopped_by" in (child2.result or ""), child2.result
        note("SCRIPT PASS")
    except Exception:
        note(traceback.format_exc())
        sys.exit(1)

run()
`

func TestPythonSDK_ScriptChild(t *testing.T) {
	t.Parallel()
	py := sdkPython(t)
	d := bootSDKDaemon(t, py)
	_, configDir := scriptUser(t, d.daemon)
	binDir := sdkBinDir(t)

	// The pymodule the child runs: the SDK itself, with the repo's sdk/python
	// path baked in — a script child's PYTHONPATH is the daemon's own
	// environment, not the test's.
	worker := strings.ReplaceAll(sdkScriptWorkerPymodule, "%SDKDIR%", filepath.Join(repoRoot, "sdk", "python"))
	putPymodule(t, d.daemon, configDir, "sdkworker", worker)

	runSDKScript(t, py,
		strings.ReplaceAll(sdkSupervisorDriver, "%SDKDIR%", filepath.Join(repoRoot, "sdk", "python")),
		sdkDriverEnv(t, configDir, binDir))
}
