package integration_test

// sandbox_test.go is the end-to-end sandbox lifecycle suite: a real daemon, a
// real `rafiki executor serve` launcher with a real relay, a real sandbox
// executor subprocess, and an in-process fake Docker Engine (see
// sandbox_fakedocker_test.go) standing in for the container runtime.
//
// Every scenario would fail if its guard were deleted: the create-body shape is
// asserted on the recorded body, the sandbox binding on the child's
// rafiki/executor label, removal on whether the fake saw the DELETE, and the
// reaper/TTL on the fake's container set.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/executorsdb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/sandbox"

	"github.com/multigres/testkit/assert"
)

// sandboxTestImage is the image the daemon is told to use; the fake Docker
// ignores it (it reports every image present).
const sandboxTestImage = "rafiki/sandbox:test"

// sandboxHarness owns one sandbox test's daemon, launcher and fake engine.
type sandboxHarness struct {
	t              *testing.T
	d              *daemon
	dsn            string
	daemonID       string
	store          executors.Store
	docker         *fakeDocker
	mountRoot      string
	relayDir       string
	launcher       *exec.Cmd
	launcherErr    *stderrBuf
	launcherID     string
	launcherMarker string
	extraEnv       []string
}

// bootSandboxDaemon boots a DB-backed daemon with the executor pool on, the
// sandbox image set and a fast sweep interval, then starts the launcher.
func bootSandboxDaemon(t *testing.T, extraEnv ...string) *sandboxHarness {
	t.Helper()
	dsn := requireExecutorDB(t)

	docker := newFakeDocker(t)
	mountRoot := shortTempDir(t, "rafiki-sbx-root-")
	_ = os.MkdirAll(filepath.Join(mountRoot, "app"), 0o755)
	relayDir := filepath.Join(shortTempDir(t, "rafiki-sbx-relay-"), "relay")

	daemonID := nextDaemonID()
	env := append([]string{
		"RAFIKI_EXECUTORS_ENABLED=1",
		"RAFIKI_SANDBOX_IMAGE=" + sandboxTestImage,
		"RAFIKI_SANDBOX_SWEEP_INTERVAL=500ms",
		// The suite shares one database and one anonymous owner (""), so named
		// sandboxes from sibling tests and earlier runs accumulate. 0 disables
		// the per-owner cap (config.go: MaxPerOwner <= 0 is unlimited), which is
		// what keeps one test's leftovers from failing another's create.
		"RAFIKI_SANDBOX_MAX_PER_OWNER=0",
	}, extraEnv...)
	d := bootDaemonDB(t, daemonID, env...)

	pool, err := pgxpool.New(context.Background(), dsn)
	assert.NewAborting(t).NoError(err, "connect pool")
	t.Cleanup(pool.Close)

	h := &sandboxHarness{
		t:         t,
		d:         d,
		dsn:       dsn,
		daemonID:  daemonID,
		store:     executorsdb.NewPostgresStore(pool),
		docker:    docker,
		mountRoot: mountRoot,
		relayDir:  relayDir,
		extraEnv:  env,
	}
	h.startLauncher(t)
	h.waitLiveExecutors(t, 1)
	h.launcherID = h.resolveLauncherID(t)
	return h
}

// startLauncher mints a token and runs a real `rafiki executor serve` whose
// docker proxy is the fake engine, with a mount root and a relay dir.
func (h *sandboxHarness) startLauncher(t *testing.T) {
	t.Helper()
	c := assert.NewAborting(t)

	h.launcherMarker = fmt.Sprintf("launcher-%d", time.Now().UnixNano())
	token, err := h.store.MintToken(context.Background(), executors.NewToken{
		Labels:        map[string]string{"machine": "launcher", "test-run": h.launcherMarker},
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint launcher token")

	execSock := filepath.Join(h.d.homeDir, "rafiki", "executor.sock")
	launcherHome := t.TempDir()
	cred := filepath.Join(t.TempDir(), "cred")
	cmd := exec.Command(cliBinary(), "executor", "serve",
		"--connect-socket", execSock,
		"--enroll-token", token,
		"--credential-file", cred,
		"--proxy", "docker=unix://"+h.docker.socketPathOnHost(),
		"--sandbox-mount-root", h.mountRoot,
		"--relay-dir", h.relayDir,
	)
	cmd.Env = append(os.Environ(),
		"HOME="+launcherHome,
		"XDG_RUNTIME_DIR="+launcherHome,
		"XDG_STATE_HOME="+launcherHome,
		"XDG_DATA_HOME="+launcherHome,
		"XDG_CONFIG_HOME="+launcherHome,
	)
	h.launcherErr = &stderrBuf{}
	cmd.Stderr = h.launcherErr
	c.NoError(cmd.Start(), "start launcher")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_, _ = cmd.Process.Wait()
	})
	h.launcher = cmd
}

// waitLiveExecutors polls until the pool reports want live executors, observed
// through a refused spawn's "N live executor(s)" message (the pool's live set
// is only observable through selection).
func (h *sandboxHarness) waitLiveExecutors(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.spawn(t, spawnOpts{selector: "env=definitely-not-a-match"})
		if err != nil && strings.Contains(err.Error(), fmt.Sprintf("%d live executor(s)", want)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("executors never became live (%d expected)\nlauncher stderr:\n%s\ndaemon stderr:\n%s",
		want, h.launcherErr.tail(4000), h.d.stderr.tail(4000))
}

type spawnOpts struct {
	parent   string
	selector string
	ref      string
	sandbox  *rafikiv1.SandboxSpec
	maxDepth *int32
	children *int32
	cwd      string
}

// spawn spawns a fundi child over Connect and returns its id plus the error.
func (h *sandboxHarness) spawn(t *testing.T, o spawnOpts) (string, error) {
	t.Helper()
	cwd := o.cwd
	if cwd == "" {
		cwd = t.TempDir()
	}
	req := &rafikiv1.SpawnRequest{
		Cwd:              cwd,
		Kind:             "fundi",
		Model:            "anthropic/sonnet-latest",
		NoSession:        true,
		ParentChildId:    o.parent,
		ExecutorSelector: o.selector,
		ExecutorRef:      o.ref,
		Sandbox:          o.sandbox,
		MaxDepth:         o.maxDepth,
		MaxChildren:      o.children,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := h.d.control(t).Spawn(ctx, connect.NewRequest(req))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetChildId(), nil
}

// mustSpawn spawns and fails the test on error.
func (h *sandboxHarness) mustSpawn(t *testing.T, o spawnOpts) string {
	t.Helper()
	id, err := h.spawn(t, o)
	assert.NewAborting(t).NoError(err, "spawn %+v", o)
	t.Cleanup(func() {
		h.cleanupChild(id)
	})
	return id
}

// cleanupChild kills and forgets a child, best-effort.
func (h *sandboxHarness) cleanupChild(id string) {
	h.killChild(id)
	h.closeChild(id)
}

// killChild stops a child without closing it.
func (h *sandboxHarness) killChild(id string) {
	client := h.d.control(h.t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = client.Kill(ctx, connect.NewRequest(&rafikiv1.KillRequest{ChildId: id}))
}

// closeChild finalizes a child.
func (h *sandboxHarness) closeChild(id string) {
	client := h.d.control(h.t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = client.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{ChildId: id}))
}

// resolveLauncherID finds the launcher this harness started, by its unique
// test-run marker label.
func (h *sandboxHarness) resolveLauncherID(t *testing.T) string {
	t.Helper()
	for _, e := range h.listStoreExecutors(t) {
		if e.Labels["test-run"] == h.launcherMarker {
			return e.ID
		}
	}
	return ""
}

// listStoreExecutors returns every executor row the store holds.
func (h *sandboxHarness) listStoreExecutors(t *testing.T) []executors.Executor {
	t.Helper()
	execs, err := h.store.List(context.Background())
	assert.NewAborting(t).NoError(err, "list executors")
	return execs
}

// shortTempDir makes a directory under a SHORT base, so the unix socket paths a
// launcher's relay and the fake docker bind stay under the 104-byte sun_path
// limit (t.TempDir() on macOS resolves through /private/var/folders/…).
func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	base := "/tmp"
	if _, err := os.Stat(base); err != nil {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, prefix)
	assert.NewAborting(t).NoError(err, "mkdirtemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// sandboxOf returns a child's labels.
func (h *sandboxHarness) labelsOf(t *testing.T, childID string) map[string]string {
	t.Helper()
	return getChild(t, h.d.control(t), childID).GetLabels()
}

// createSandboxCLI runs `rafiki sandbox create` and returns the created info.
func (h *sandboxHarness) createSandboxCLI(t *testing.T, args ...string) (*rafikiv1.SandboxInfo, error) {
	t.Helper()
	cmd := cliCmd(t, h.d, append([]string{"--output", "json", "sandbox", "create"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w\nstderr: %s", err, stderr.String())
	}
	var resp rafikiv1.CreateSandboxResponse
	if err := protojson.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("decode sandbox create: %w\n%s", err, out)
	}
	info := resp.GetSandbox()
	if name := info.GetName(); name != "" {
		t.Cleanup(func() { _ = cliCmd(t, h.d, "sandbox", "rm", name).Run() })
	}
	return info, nil
}

// listSandboxCLI runs `rafiki sandbox ls` and returns the rows. The CLI's -o
// json face wraps list rows in a {"rows":[…] } envelope (cmd/rafiki/protoout.go),
// so the envelope is unwrapped and each row decoded as a SandboxInfo.
func (h *sandboxHarness) listSandboxCLI(t *testing.T) []*rafikiv1.SandboxInfo {
	t.Helper()
	out, err := cliCmd(t, h.d, "--output", "json", "sandbox", "ls").Output()
	assert.NewAborting(t).NoError(err, "sandbox ls")
	var env struct {
		Rows []json.RawMessage `json:"rows"`
	}
	assert.NewAborting(t).NoError(json.Unmarshal(out, &env), "decode sandbox ls envelope: %s", out)
	rows := make([]*rafikiv1.SandboxInfo, 0, len(env.Rows))
	for _, raw := range env.Rows {
		var info rafikiv1.SandboxInfo
		assert.NewAborting(t).NoError(protojson.Unmarshal(raw, &info), "decode sandbox row: %s", raw)
		rows = append(rows, &info)
	}
	return rows
}

// sandboxByName returns the named sandbox row from `sandbox ls`, or nil.
func (h *sandboxHarness) sandboxByName(t *testing.T, name string) *rafikiv1.SandboxInfo {
	t.Helper()
	for _, s := range h.listSandboxCLI(t) {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

// ─── scenarios ────────────────────────────────────────────────────────────────

// TestSandboxCreateRunsToolsInsideTheContainer: a named sandbox is created
// through the CLI against the real daemon; the create body the launcher
// forwarded has network bridge, a read-only bind of the host path and the
// read-only relay mount, and NO Binds array; a child spawned with executor:<name>
// lands on the sandbox's own executor.
func TestSandboxCreateRunsToolsInsideTheContainer(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	app := filepath.Join(h.mountRoot, "app")
	info, err := h.createSandboxCLI(t, "--name", "box-"+runTag(),
		"--mount", "ro:/work=host:"+app)
	c.NoError(err, "sandbox create")
	c.Eq(sandboxStateReady(), info.GetState(), "state")
	c.True(info.GetConnected(), "the sandbox executor is connected")

	creates := h.docker.recordedCreates()
	c.Require().Len(creates, 1, "one container create")
	body := creates[0].Body
	c.Eq("bridge", body.HostConfig.NetworkMode, "egress network is bridge")

	var sawWork bool
	for _, m := range body.HostConfig.Mounts {
		if m.Target == "/work" {
			sawWork = true
			c.Eq("bind", m.Type, "host mount type")
			c.Eq(app, m.Source, "host mount source")
			c.True(m.ReadOnly, "ro mount is read-only")
		}
		if m.Target == sandbox.ContainerRelayDir {
			c.Eq("bind", m.Type, "relay mount is a bind")
			c.Eq(h.relayDir, m.Source, "relay mount source is the launcher's relay dir")
			c.True(m.ReadOnly, "relay mount is read-only")
		}
	}
	c.True(sawWork, "the host-path mount reached docker")
	// No Binds array is expressible at all: the create-body struct never emits
	// one, and the raw body proves it.
	c.NotStrContains(string(creates[0].Raw), `"Binds"`, "the create body must carry no Binds")

	// A child spawned with executor:<name> lands on the sandbox's own executor.
	childID := h.mustSpawn(t, spawnOpts{ref: info.GetName()})
	c.Eq(info.GetExecutorId(), h.labelsOf(t, childID)["rafiki/executor"],
		"the child bound to the sandbox's executor")
}

// sandboxStateReady is the persisted ready state literal.
func sandboxStateReady() string { return "ready" }

// runTag returns a per-run-unique tag so sandbox names never collide with an
// earlier run's rows in the shared test database.
func runTag() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

// TestSandboxNetworkNoneAndReadOnlyMount: --network none reaches docker as
// NetworkMode none and the mount is still read-only; the sandbox still connects
// (the relay is the only link).
func TestSandboxNetworkNoneAndReadOnlyMount(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	app := filepath.Join(h.mountRoot, "app")
	info, err := h.createSandboxCLI(t, "--name", "none-"+runTag(),
		"--network", "none", "--mount", "ro:/work=host:"+app)
	c.NoError(err, "sandbox create with --network none")
	c.Eq(sandboxStateReady(), info.GetState(), "state")
	c.True(info.GetConnected(), "the sandbox still connects under network none")

	creates := h.docker.recordedCreates()
	c.Require().Len(creates, 1, "one create")
	c.Eq("none", creates[0].Body.HostConfig.NetworkMode, "network none reaches docker")
	for _, m := range creates[0].Body.HostConfig.Mounts {
		if m.Target == "/work" {
			c.True(m.ReadOnly, "the mount stays read-only")
		}
	}
}

// TestSandboxHostPathOutsideRootRefusedByTheDaemon: the daemon pre-check refuses
// a host path outside every launcher mount root, naming the field.
func TestSandboxHostPathOutsideRootRefusedByTheDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	_, err := h.createSandboxCLI(t, "--name", "escape-"+runTag(),
		"--mount", "ro:/etc=host:/etc")
	c.Error(err, "a host path outside every root must be refused")
	c.StrContains(err.Error(), "mount root", "the refusal names the mount root")
	c.Len(h.docker.recordedCreates(), 0, "no container was created")
}

// TestSandboxEmptySelectorNeverLandsInASandbox: with a named sandbox and an
// ordinary executor both live, an empty-selector spawn binds to the ordinary
// one — a sandbox is never an implicit choice.
func TestSandboxEmptySelectorNeverLandsInASandbox(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)
	h.enrollOrdinaryExecutor(t)
	h.waitLiveExecutors(t, 2)

	info, err := h.createSandboxCLI(t, "--name", "implicit-"+runTag())
	c.NoError(err, "sandbox create")

	childID := h.mustSpawn(t, spawnOpts{})
	got := h.labelsOf(t, childID)["rafiki/executor"]
	c.NotEq("", got, "child bound to an executor")
	c.NotEq(info.GetExecutorId(), got, "an empty-selector spawn must never land in the sandbox")
}

// enrollOrdinaryExecutor mints a token and starts a native executor (no docker
// proxy) so ordinary selection has a non-sandbox candidate.
func (h *sandboxHarness) enrollOrdinaryExecutor(t *testing.T) {
	t.Helper()
	c := assert.NewAborting(t)
	token, err := h.store.MintToken(context.Background(), executors.NewToken{
		Labels:        map[string]string{"machine": "ordinary"},
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint ordinary token")
	execSock := filepath.Join(h.d.homeDir, "rafiki", "executor.sock")
	home := t.TempDir()
	cmd := exec.Command(cliBinary(), "executor", "serve",
		"--connect-socket", execSock,
		"--enroll-token", token,
		"--credential-file", filepath.Join(t.TempDir(), "cred"),
		"--root", t.TempDir(),
	)
	cmd.Env = append(os.Environ(),
		"HOME="+home, "XDG_RUNTIME_DIR="+home, "XDG_STATE_HOME="+home,
		"XDG_DATA_HOME="+home, "XDG_CONFIG_HOME="+home,
	)
	c.NoError(cmd.Start(), "start ordinary executor")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_, _ = cmd.Process.Wait()
	})
}

// TestSandboxSpawnBlockScopes: a spawn-block sandbox is owned by its child and
// the owner binds to it; a self-scoped block never covers the owner's own
// child, which is refused with no ordinary executor to fall back to; an
// unrelated child cannot reach a sandbox by naming its machine; and Kill leaves
// the container alone while Close removes it (the fake engine sees the DELETE).
func TestSandboxSpawnBlockScopes(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	depth := int32(3)
	kids := int32(8)

	// A owns a subtree block; its owning child binds to the sandbox.
	a, err := h.spawn(t, spawnOpts{sandbox: &rafikiv1.SandboxSpec{Scope: "subtree"}, maxDepth: &depth, children: &kids})
	c.NoError(err, "spawn A with a subtree block")
	aExec := h.labelsOf(t, a)["rafiki/executor"]
	c.NotEq("", aExec, "A bound to its sandbox")

	var row *rafikiv1.SandboxInfo
	for _, s := range h.listSandboxCLI(t) {
		if s.GetOwnerChild() == a {
			row = s
		}
	}
	c.Require().NotNil(row, "A's spawn block is a live sandbox row")
	c.Eq("subtree", row.GetScope(), "the block records its scope")
	c.Eq(aExec, row.GetExecutorId(), "A bound to the sandbox the row owns")

	// A descendant of A inherits A's selector; a child-owned sandbox is never an
	// ordinary candidate, so the descendant stays inside the block and never
	// escapes to the launcher.
	b, err := h.spawn(t, spawnOpts{parent: a})
	c.NoError(err, "spawn B under A")
	c.NotEq(h.launcherID, h.labelsOf(t, b)["rafiki/executor"], "the descendant must not escape to the launcher")
	h.cleanupChild(b)

	// self: C owns a self block; D (C's child) must NOT share it. With no
	// ordinary executor live, an empty-selector spawn for D is refused.
	cid, err := h.spawn(t, spawnOpts{sandbox: &rafikiv1.SandboxSpec{Scope: "self"}, maxDepth: &depth, children: &kids})
	c.NoError(err, "spawn C with a self block")
	d, err := h.spawn(t, spawnOpts{parent: cid})
	c.NoError(err, "a parented spawn with no matching executor starts unbound")
	dLabels := h.labelsOf(t, d)
	c.Eq("", dLabels["rafiki/executor"], "a self-scoped block does not cover the owner's own child")
	c.Eq("unbound", dLabels["rafiki/executor-state"], "the owner's child is unbound, not on a machine the block did not offer")
	h.cleanupChild(d)
	h.cleanupChild(cid)

	// An unrelated top-level child cannot reach A's sandbox by naming its machine.
	_, err = h.spawn(t, spawnOpts{ref: sandboxMachineName(row.GetId())})
	c.Error(err, "an unrelated child must not reach a child-owned sandbox")

	// Kill leaves the container; Close removes it.
	h.killChild(a)
	c.False(waitFor(t, 2*time.Second, func() bool { return h.docker.hasRemoved(row.GetContainerId()) }),
		"Kill must not remove the spawn block's container")
	h.closeChild(a)
	c.True(waitFor(t, 10*time.Second, func() bool { return h.docker.hasRemoved(row.GetContainerId()) }),
		"Close removes the spawn block's container")
}

// sandboxMachineName mirrors cmd/rafikid's sandboxSpawnMachineName: "sbx-" plus
// the first 12 characters of the row id after its "sbx_" prefix, lowercased.
func sandboxMachineName(rowID string) string {
	rest := strings.TrimPrefix(rowID, "sbx_")
	if len(rest) > 12 {
		rest = rest[:12]
	}
	return "sbx-" + strings.ToLower(rest)
}

// TestSandboxExecutorRmRefusesASandboxRow: `rafiki executor delete` refuses a
// sandbox's executor row.
func TestSandboxExecutorRmRefusesASandboxRow(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	info, err := h.createSandboxCLI(t, "--name", "rm-"+runTag())
	c.NoError(err, "sandbox create")

	out, err := cliCmd(t, h.d, "executor", "delete", info.GetExecutorId()).CombinedOutput()
	c.Error(err, "deleting a sandbox executor must be refused")
	c.StrContains(string(out), "sandbox", "the refusal mentions the sandbox; got: %s", out)
}

// TestSandboxTTLExpiryRemoves: a short-TTL sandbox is removed by the sweeper —
// the container is deleted and the row disappears from `sandbox ls`.
func TestSandboxTTLExpiryRemoves(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	name := "ttl-" + runTag()
	info, err := h.createSandboxCLI(t, "--name", name, "--ttl", "2s")
	c.NoError(err, "sandbox create with a 2s TTL")

	ok := waitFor(t, 15*time.Second, func() bool {
		return h.docker.hasRemoved(info.GetContainerId())
	})
	c.True(ok, "the sweeper removed the TTL-expired container")
	c.True(waitFor(t, 5*time.Second, func() bool { return h.sandboxByName(t, name) == nil }),
		"the expired row is gone from sandbox ls")
}

// TestSandboxReaperRemovesAnOrphan keeps a sandbox with a live row while
// removing a container the daemon has no row for, and reports a row whose
// container the fake dropped as lost without recreating it.
func TestSandboxReaperRemovesAnOrphan(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	info, err := h.createSandboxCLI(t, "--name", "keep-"+runTag())
	c.NoError(err, "sandbox create")

	// An orphan container with a sandbox label no row owns.
	h.docker.addOrphan("orphan-1", "sbx_nonexistent")

	ok := waitFor(t, 15*time.Second, func() bool { return h.docker.hasRemoved("orphan-1") })
	c.True(ok, "the reaper removed the orphan container")

	// The live sandbox is kept: its container was never deleted.
	c.False(h.docker.hasRemoved(info.GetContainerId()), "the live sandbox's container was kept")

	// Drop the live sandbox's container: the row must read lost, not be recreated.
	beforeCreates := h.docker.createCount()
	h.docker.dropContainer(info.GetContainerId())
	ok = waitFor(t, 40*time.Second, func() bool {
		s := h.sandboxByName(t, info.GetName())
		return s != nil && s.GetState() == "lost"
	})
	if !ok {
		s := h.sandboxByName(t, info.GetName())
		t.Logf("row=%v\ndaemon stderr tail:\n%s", s, h.d.stderr.tail(4000))
	}
	c.True(ok, "a row whose container is gone reads lost")
	c.Eq(beforeCreates, h.docker.createCount(), "a lost sandbox is not recreated")
}

// TestSandboxSurvivesDaemonRestart: with a live sandbox and a bound child, the
// daemon is stopped and restarted against the same database and home; the
// sandbox executor reconnects without a second container create and the child
// stays bound to the same executor id.
func TestSandboxSurvivesDaemonRestart(t *testing.T) {
	c := assert.NewAborting(t)
	h := bootSandboxDaemon(t)

	info, err := h.createSandboxCLI(t, "--name", "restart-"+runTag())
	c.NoError(err, "sandbox create")
	childID := h.mustSpawn(t, spawnOpts{ref: info.GetName()})
	c.Eq(info.GetExecutorId(), h.labelsOf(t, childID)["rafiki/executor"], "child bound pre-restart")

	createsBefore := h.docker.createCount()

	// Stop the daemon (the launcher keeps running and redials).
	c.NoError(h.d.proc.Process.Signal(os.Interrupt), "signal daemon")
	_, _ = h.d.proc.Process.Wait()

	// Restart against the same home dir and daemon id.
	d2 := restartDaemon(t, h.d, h.daemonID, h.extraEnv...)
	h.d = d2

	// The sandbox row is recovered and its executor reconnects.
	ok := waitFor(t, 30*time.Second, func() bool {
		s := h.sandboxByName(t, info.GetName())
		return s != nil && s.GetConnected()
	})
	c.True(ok, "the sandbox executor reconnected after restart")
	c.Eq(createsBefore, h.docker.createCount(), "the sandbox was reconnected, never re-created")

	// The child is still bound to the same executor id.
	got := h.labelsOf(t, childID)["rafiki/executor"]
	c.Eq(info.GetExecutorId(), got, "the child remains bound to the same executor after restart")
}

// restartDaemon boots a fresh rafikid against an existing daemon's home dir and
// database, mirroring TestDBChildState_RestartSurvivesWipedStateDir.
func restartDaemon(t *testing.T, old *daemon, daemonID string, extraEnv ...string) *daemon {
	t.Helper()
	c := assert.NewAborting(t)
	cmd := exec.Command(daemonBinary())
	cmd.Env = append(os.Environ(),
		"HOME="+old.homeDir,
		"XDG_RUNTIME_DIR="+old.homeDir,
		"XDG_STATE_HOME="+old.homeDir,
		"XDG_DATA_HOME="+old.homeDir,
		"RAFIKI_DB="+os.Getenv("RAFIKI_TEST_DSN"),
		"RAFIKI_DAEMON_ID="+daemonID,
		"RAFIKI_PROXY_LISTEN=127.0.0.1:0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	stderr := &stderrBuf{}
	cmd.Stderr = stderr
	c.NoError(cmd.Start(), "restart daemon")
	d2 := &daemon{
		socketPath: old.socketPath,
		proc:       cmd,
		homeDir:    old.homeDir,
		logsDir:    old.logsDir,
		stderr:     stderr,
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_, _ = cmd.Process.Wait()
		os.RemoveAll(old.homeDir)
	})
	waitDaemonSocket(t, d2.socketPath)
	d2.proxyURL = waitProxyListen(t, stderr)
	return d2
}

// waitDaemonSocket polls until the daemon accepts on its control socket.
func waitDaemonSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon never accepted on %s", socketPath)
}
