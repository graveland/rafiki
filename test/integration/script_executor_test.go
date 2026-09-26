// SPDX-License-Identifier: Apache-2.0

package integration_test

// Wave 4: script children hosted ON EXECUTORS via daraja (`--launch script`).
//
// The full chain, against a real daemon, a real enrolled executor subprocess
// and a real daraja the executor launches:
//
//	daemon (TLS control listener, executor pool)
//	  └─ executor subprocess (--launch script, owner-labelled)
//	       └─ daraja serve (launched by AdminService.Launch on a script spawn)
//	            ├─ resolves the pymodule from ITS synced cache
//	            │    (the daemon pushed the owner's corpus on executor connect
//	            │     and on the pymodule put)
//	            ├─ runs pkg/childsock against the daemon's TLS face with the
//	            │    pinned certificate and the launch payload's child secret
//	            └─ runs the interpreter on the resolved argv
//
// What the assertions cover, by wave-4 item:
//
//   - 4.1 the executor self-reports the launch kind and the child lands on it
//     (rafiki/executor + rafiki/daraja-pgid labels);
//   - 4.2 the script's pymodule resolves from the executor's synced cache and
//     the run's output reaches the daemon;
//   - 4.3 the script talks to its daemon THROUGH the per-child socket the
//     daraja serves — Report and SetResult over it must succeed, which is only
//     possible if the TLS-with-pin proxy and the child secret from the launch
//     payload both work; and the launch payload's credentials never reach the
//     script's environment (the leak check env_leaks() the driver runs first);
//   - 4.4's process-survival semantics are documented, not asserted here: a
//     daemon restart refuses the daraja's reconnect credential (an in-memory
//     registry), and the process keeps running under daraja until then — the
//     reconnect machinery itself is pinned in pkg/darajapool's harness.
//
// The driver writes every intermediate observation to a probe file, because a
// failed script's stderr only reaches the daemon when the script FAILS (the
// settle tail), and the ring carries stdout only.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// scriptExecutorDriverCode is the executor-hosted driver. Order matters: the
// environment leak check runs FIRST (a credential that reached the script
// would poison everything after it), then a Report through the per-child
// socket (the executor-hosted childsock leg), then the result.
const scriptExecutorDriverCode = scriptConnectHelper + `
import traceback

probe = sys.argv[sys.argv.index("--probe-out") + 1]

def note(tag, obj):
    with open(probe, "a") as f:
        f.write("%s: %r\n" % (tag, obj))

def run_driver():
    leaks = env_leaks()
    if leaks:
        note("LEAKS", leaks)
        sys.exit(7)

    # The per-child socket reaches the daemon through the EXECUTOR's proxy —
    # TLS with the pinned cert on the way out, the child secret injected on
    # the way in. A successful Report proves both.
    call("Report", {"kind": "progress",
                    "data_json": json.dumps({"stage": "on-executor",
                                             "cwd": os.getcwd(),
                                             "pid": os.getpid()})})
    note("reported", "ok")
    call("SetResult", {"result_json": json.dumps(
        {"on_executor": True, "cwd": os.getcwd()})})
    print("executor-hosted-script-ok", flush=True)
    sys.exit(0)

try:
    run_driver()
except Exception:
    with open(probe, "a") as f:
        f.write(traceback.format_exc())
    sys.exit(1)
`

// enrollScriptExecutor is enrollExecutor with --launch script and an isolated
// pymodule cache the test can watch. Returns the enrolled executor's id and
// the cache root its subprocess resolves pymodules from.
func enrollScriptExecutor(t *testing.T, g *grantDaemon, ownerName string) (executorID, cacheRoot string) {
	t.Helper()

	marker := fmt.Sprintf("t%d", time.Now().UnixNano())
	labels := map[string]string{
		"env":      "scripthost",
		"owner":    ownerName,
		"test-run": marker,
	}
	token, err := g.store.MintToken(context.Background(), executors.NewToken{
		Labels:        labels,
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	cache, err := os.MkdirTemp(base, "rafiki-excache-")
	if err != nil {
		t.Fatalf("mkdirtemp cache: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(cache) })

	root := t.TempDir()
	credFile := filepath.Join(t.TempDir(), "cred")
	cmd := exec.Command(cliPath, "executor", "serve",
		"--connect", g.listenAddr,
		"--enroll-token", token,
		"--credential-file", credFile,
		"--pin-cert", g.fingerprint,
		"--launch", "script",
		"--root", root,
	)
	// The executor's own XDG dirs are isolated so the test can watch its
	// pymodule cache fill — and so nothing this subprocess resolves leaks into
	// the developer's machine.
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+cache,
		"XDG_CONFIG_HOME="+filepath.Join(cache, "config"),
		"XDG_STATE_HOME="+filepath.Join(cache, "state"),
		"XDG_DATA_HOME="+filepath.Join(cache, "data"),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start script executor: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		execs, _ := g.store.List(context.Background())
		for _, e := range execs {
			if e.Labels["test-run"] == marker && !e.LastSeenAt.IsZero() {
				return e.ID, cache
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("script executor %v never enrolled and became live", labels)
	return "", ""
}

// waitExecutorCache polls until the executor's synced cache holds the named
// module — the daemon's pusher delivered the owner's corpus (the put fires a
// push to every eligible executor).
func waitExecutorCache(t *testing.T, cacheRoot, name string) {
	t.Helper()
	path := filepath.Join(cacheRoot, "rafiki", "pymodules", name, name+".py")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	entries, _ := os.ReadDir(filepath.Join(cacheRoot, "rafiki"))
	t.Fatalf("the executor's synced cache never received %q (rafiki/ = %v)", name, entries)
}

// TestScriptChildOnExecutor is wave 4's end to end.
func TestScriptChildOnExecutor(t *testing.T) {
	t.Parallel()
	if _, err := lookPython3(); err != nil {
		t.Skip("python3 not available: script children need an interpreter")
	}
	dsn := requireExecutorDB(t)
	g := bootGrantDaemon(t, dsn)
	d := g.daemon

	// The owner of everything here: the executor's corpus is pushed per
	// owner label, and the spawn must carry that owner (ChildForMCPToken
	// refuses an ownerless child, so an anonymous spawn would get a socket
	// that 401s).
	username := fmt.Sprintf("operator-%d", time.Now().UnixNano())
	configDir := t.TempDir()
	userCmd := cliCmdIn(t, d, configDir, "--output", "json", "user", "create", username)
	var userStderr strings.Builder
	userCmd.Stderr = &userStderr
	userOut, err := userCmd.Output()
	if err != nil {
		t.Fatalf("user create failed: %v\nstdout: %s\nstderr: %s", err, userOut, userStderr.String())
	}
	tokenPath := filepath.Join(configDir, "rafiki", "profiles", "it", "token")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil || len(strings.TrimSpace(string(tokenBytes))) == 0 {
		t.Fatalf("user create did not leave a token at %s: %v", tokenPath, err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	opClient := faceClient(t, d, token)

	// Enroll FIRST (its connect fires the pusher), then put, then wait for
	// the corpus to land on the executor's cache.
	executorID, execCache := enrollScriptExecutor(t, g, username)
	putPymodule(t, d, configDir, "script_executor_driver_it", scriptExecutorDriverCode)
	waitExecutorCache(t, execCache, "script_executor_driver_it")

	probe := filepath.Join(t.TempDir(), "driver.log")
	cwd := t.TempDir()
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	sresp, err := opClient.Spawn(sctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:  cwd,
		Kind: "script",
		Name: "on-executor",
		Script: &rafikiv1.SpawnRequest_ScriptSpec{
			Repo:   "local",
			Script: "script_executor_driver_it",
			Args:   []string{"--probe-out", probe},
		},
	}))
	scancel()
	if err != nil {
		t.Fatalf("spawn script child on executor: %v", err)
	}
	childID := sresp.Msg.GetChildId()

	// The child is hosted ON the enrolled executor: the launch stamped the
	// binding and the daraja pgid (4.1). On a timeout the summary and the
	// driver's probe file are dumped first — a hang is a diagnosis, not just
	// a deadline.
	sum := func() *rafikiv1.ChildSummary {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			resp, err := opClient.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
			cancel()
			if err != nil {
				t.Fatalf("GetChild(%s): %v", childID, err)
			}
			if resp.Msg.GetChild().GetStatus() == "exited" {
				return resp.Msg.GetChild()
			}
			time.Sleep(200 * time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := opClient.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
		cancel()
		if err == nil {
			b, _ := json.Marshal(resp.Msg.GetChild())
			t.Logf("stuck child summary: %s", b)
		}
		if probeData, perr := os.ReadFile(probe); perr == nil && len(probeData) > 0 {
			t.Logf("driver probe so far:\n%s", probeData)
		}
		t.Logf("daemon stderr tail:\n%s", d.stderr.tail(8000))
		t.Fatalf("script child did not exit within 90s")
		return nil
	}()
	if sum.ExitCode == nil || *sum.ExitCode != 0 {
		probeData, perr := os.ReadFile(probe)
		if perr != nil {
			probeData = []byte(fmt.Sprintf("probe file unread: %v", perr))
		}
		t.Fatalf("script child exited with %d (result %q); its log:\n%.4000s",
			safeCode(sum.ExitCode), sum.GetResult(), probeData)
	}
	if placed := g.executorOf(t, childID); placed != executorID {
		t.Fatalf("script child landed on executor %q, want the enrolled script host %q", placed, executorID)
	}

	// 4.3's leg, asserted from the INSIDE: the driver's Report and SetResult
	// both travelled the executor's per-child socket — TLS with the pinned
	// cert to the daemon's face, the child secret injected by the proxy. A
	// parseable stored result proves the SetResult leg; the probe file proves
	// the Report leg ran after the env-leak check passed.
	var outcome map[string]any
	if err := json.Unmarshal([]byte(sum.GetResult()), &outcome); err != nil || outcome["on_executor"] != true {
		probeData, perr := os.ReadFile(probe)
		if perr != nil {
			probeData = []byte(fmt.Sprintf("probe file unread: %v", perr))
		}
		t.Fatalf("result = %q, want {on_executor: true} (err=%v); log:\n%.4000s",
			sum.GetResult(), err, probeData)
	}
	// The stdout relay leg: the driver's stdout line reached the daemon and
	// is served back by the same framed verb the logs CLI drives (an exited
	// child's out stream is the log dump, via ctrl_get_recent).
	raw := d.request(t, mustMarshal(t, map[string]any{
		"type": "ctrl_get_recent", "id": "recent", "childId": childID,
	}))
	var recent protocol.Response
	mustUnmarshal(t, raw, &recent)
	if !recent.Success {
		t.Fatalf("ctrl_get_recent failed: %+v", recent.Error)
	}
	var recentData protocol.GetRecentResponseData
	if err := json.Unmarshal(recent.Data, &recentData); err != nil {
		t.Fatalf("decode get_recent: %v (raw %.400s)", err, recent.Data)
	}
	var outAll []byte
	for _, e := range recentData.Events {
		outAll = append(outAll, []byte(e)...)
	}
	if !strings.Contains(string(outAll), "executor-hosted-script-ok") {
		t.Fatalf("the script's stdout never reached the daemon:\n%.2000s", outAll)
	}
}
