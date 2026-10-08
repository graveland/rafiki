// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// newTreeSyncExecutor starts a REAL executor server over h2c and returns a
// client for it plus the server's root. The daemon-side syncer is tested
// against the real ReadTree/WriteTree handlers, so the relay is pinned end to
// end rather than against a hand-written stub.
func newTreeSyncExecutor(t *testing.T) (executorpbconnect.ExecutorServiceClient, string) {
	t.Helper()
	ck := assert.NewAborting(t)
	root := t.TempDir()
	srv := executor.NewServer(executor.Options{Root: root, Concurrency: 8, Version: "test"})
	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	ts := httptest.NewUnstartedServer(mux)
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	ts.Config.Protocols = protos
	ts.Start()
	t.Cleanup(ts.Close)

	client := executorpbconnect.NewExecutorServiceClient(
		&http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", ts.Listener.Addr().String())
			},
		}},
		ts.URL,
	)
	ck.True(ts.Listener.Addr().String() != "", "the executor server must be listening")
	return client, root
}

// observingClient wraps a real executor client to count the tree-sync RPCs it
// is asked for and to record the context each stream was opened with. Both
// streams share one derived context, so a recorded context being cancelled is
// the assertion that both streams were unwound.
type observingClient struct {
	executorpbconnect.ExecutorServiceClient
	mu       sync.Mutex
	reads    int
	writes   int
	readCtx  context.Context
	writeCtx context.Context
}

func (c *observingClient) ReadTree(ctx context.Context, req *connect.Request[executorpb.ReadTreeRequest]) (*connect.ServerStreamForClient[executorpb.ReadTreeResponse], error) {
	c.mu.Lock()
	c.reads++
	c.readCtx = ctx
	c.mu.Unlock()
	return c.ExecutorServiceClient.ReadTree(ctx, req)
}

func (c *observingClient) WriteTree(ctx context.Context) *connect.ClientStreamForClient[executorpb.WriteTreeRequest, executorpb.WriteTreeResponse] {
	c.mu.Lock()
	c.writes++
	c.writeCtx = ctx
	c.mu.Unlock()
	return c.ExecutorServiceClient.WriteTree(ctx)
}

func (c *observingClient) counts() (reads, writes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, c.writes
}

func (c *observingClient) streamContexts() (read, write context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readCtx, c.writeCtx
}

// treeSyncFakePool is the syncer's treeSyncExecutors AND the Controller's
// executorPool: the live set, the dialer, and the handful of methods selection
// needs. It embeds fakePool so the two views can never disagree in a test.
type treeSyncFakePool struct {
	*fakePool
	clients map[string]executorpbconnect.ExecutorServiceClient
}

func newTreeSyncPool(live []execpool.LiveExecutor, clients map[string]executorpbconnect.ExecutorServiceClient) *treeSyncFakePool {
	return &treeSyncFakePool{fakePool: &fakePool{live: live}, clients: clients}
}

func (f *treeSyncFakePool) ConnectClientFor(id string) (executorpbconnect.ExecutorServiceClient, error) {
	c, ok := f.clients[id]
	if !ok {
		return nil, fmt.Errorf("no client for executor %q", id)
	}
	return c, nil
}

// treeSyncExecutor builds one live executor with a real ROW (owner, isolation)
// and a Describe whose only interesting bit is the tree-sync capability.
func treeSyncExecutor(id, ownerUserID string, labels map[string]string, isolation string, capable bool) execpool.LiveExecutor {
	return execpool.LiveExecutor{
		Executor: executors.Executor{
			ID:          id,
			Labels:      labels,
			OwnerUserID: ownerUserID,
			Isolation:   isolation,
			Enabled:     true,
		},
		Describe: &executorpb.DescribeResponse{TreeSync: capable},
	}
}

// treeSyncSandboxExecutor is treeSyncExecutor for a sandbox's executor: its row
// carries the sandbox label isSandboxRow (and dropSandboxCandidates) key on.
func treeSyncSandboxExecutor(id, ownerUserID, machine, isolation string) execpool.LiveExecutor {
	return treeSyncExecutor(id, ownerUserID, map[string]string{
		"machine":               machine,
		sandbox.RowLabelSandbox: "1",
	}, isolation, true)
}

// newPathSyncFixture wires a Controller whose executor pool IS the syncer's
// pool, so resolution and liveness agree exactly as they do in production.
func newPathSyncFixture(t *testing.T, pool *treeSyncFakePool) (*Controller, *pathSyncer) {
	t.Helper()
	ctrl := newTestController(t)
	ctrl.execPool = pool
	return ctrl, newPathSyncer(ctrl, pool)
}

// insertPathSyncChild adds a childstore session with the given parent and
// selector.
func insertPathSyncChild(t *testing.T, ctrl *Controller, id, parent, selector string) {
	t.Helper()
	labels := map[string]string{"rafiki/kind": "fundi"}
	if parent != "" {
		labels[childstore.LabelParent] = parent
		labels[childstore.LabelRoot] = parent
	}
	ctrl.st.Insert(&childstore.Session{
		ChildID:          id,
		Status:           protocol.StatusIdle,
		StartedAt:        time.Now(),
		Kind:             protocol.KindFundi,
		ExecutorSelector: selector,
		MaxDepth:         1,
		MaxChildren:      8,
		Labels:           labels,
	})
}

func controllerErr(t *testing.T, err error) *connectapi.ControllerError {
	t.Helper()
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("want *connectapi.ControllerError, got %T: %v", err, err)
	}
	return ce
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(content), mode), "write %s", path)
	assert.NewAborting(t).NoError(os.Chmod(path, mode), "chmod %s", path)
}

// pollUntil polls cond until it holds or the budget runs out.
func pollUntil(cond func() bool, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// sandboxRowFor builds a live NAMED sandbox row pointing at an executor.
func sandboxRowFor(id, name, ownerUserID, executorID, createdBy string) sandbox.Row {
	return sandbox.Row{
		ID:          id,
		Name:        name,
		OwnerUserID: ownerUserID,
		ExecutorID:  executorID,
		CreatedBy:   createdBy,
		State:       sandboxStateReady,
	}
}

// sandboxBlockRow builds a live SPAWN-BLOCK sandbox row (no name) owned by a
// child, with the given scope and creator.
func sandboxBlockRow(id, ownerUserID, executorID, ownerChild, createdBy string, scope protocol.SandboxScope) sandbox.Row {
	return sandbox.Row{
		ID:          id,
		OwnerUserID: ownerUserID,
		ExecutorID:  executorID,
		CreatedBy:   createdBy,
		OwnerChild:  ownerChild,
		Scope:       scope,
		State:       sandboxStateReady,
	}
}

// assertNotReachable is the uniform refusal every out-of-reach ref must get.
func assertNotReachable(t *testing.T, p *pathSyncer, callerChild, ref string) {
	t.Helper()
	ck := assert.NewCollecting(t)
	_, err := p.resolve(t.Context(), users.Identity{UserID: "u1"}, callerChild, ref)
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrNotFound, ce.Code, "%q code", ref)
	ck.Eq(fmt.Sprintf("executor %q is not reachable by this caller", ref), ce.Message, "%q message", ref)
}

// assertReachable resolves ref and requires it to land on wantID.
func assertReachable(t *testing.T, p *pathSyncer, callerChild, ref, wantID string) {
	t.Helper()
	ck := assert.NewCollecting(t)
	target, err := p.resolve(t.Context(), users.Identity{UserID: "u1"}, callerChild, ref)
	ck.Require().NoError(err, "%q must be reachable", ref)
	ck.Eq(wantID, target.exec.ID, "%q must resolve to %s", ref, wantID)
}

// --- path validation -------------------------------------------------------

func TestPathSyncRejectsRelativeAndUncleanPaths(t *testing.T) {
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)
	live := []execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}
	pool := newTreeSyncPool(live, map[string]executorpbconnect.ExecutorServiceClient{
		"exec-src": srcClient,
		"exec-dst": dstClient,
	})
	_, p := newPathSyncFixture(t, pool)
	owner := users.Identity{UserID: "u1"}

	good := protocol.SyncEndpoint{Executor: "src", Path: filepath.Join(srcRoot, "tree")}
	goodDst := protocol.SyncEndpoint{Executor: "dst", Path: filepath.Join(dstRoot, "copy")}

	// The message is asserted too: an executor would also refuse a relative or
	// unclean path, but only the daemon prefixes it this way — so the assertion
	// pins the daemon-side guard rather than the executor's copy of it.
	cases := []struct {
		name     string
		src, dst protocol.SyncEndpoint
		wantMsg  string
	}{
		{"relative source", protocol.SyncEndpoint{Executor: "src", Path: "tree"}, goodDst, `source path must be absolute and clean: "tree"`},
		{"relative destination", good, protocol.SyncEndpoint{Executor: "dst", Path: "copy"}, `destination path must be absolute and clean: "copy"`},
		{"unclean source", protocol.SyncEndpoint{Executor: "src", Path: srcRoot + "//tree"}, goodDst, fmt.Sprintf("source path must be absolute and clean: %q", srcRoot+"//tree")},
		{"unclean destination", good, protocol.SyncEndpoint{Executor: "dst", Path: dstRoot + "/./copy"}, fmt.Sprintf("destination path must be absolute and clean: %q", dstRoot+"/./copy")},
		{"empty path", good, protocol.SyncEndpoint{Executor: "dst", Path: ""}, `destination path must be absolute and clean: ""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			_, err := p.SyncPath(t.Context(), owner, "", protocol.SyncPathRequest{Src: tc.src, Dst: tc.dst})
			ce := controllerErr(t, err)
			ck.Eq(protocol.ErrInvalidArgs, ce.Code, "%s", tc.name)
			ck.Eq(tc.wantMsg, ce.Message, "%s message", tc.name)
		})
	}
}

func TestPathSyncRejectsZeroMaxBytes(t *testing.T) {
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)
	live := []execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}
	pool := newTreeSyncPool(live, map[string]executorpbconnect.ExecutorServiceClient{
		"exec-src": srcClient,
		"exec-dst": dstClient,
	})
	_, p := newPathSyncFixture(t, pool)

	srcDir := filepath.Join(srcRoot, "tree")
	assert.NewAborting(t).NoError(os.MkdirAll(srcDir, 0o755))
	writeFile(t, filepath.Join(srcDir, "a.txt"), "hello", 0o644)

	for _, max := range []int64{0, -1} {
		_, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
			Src:      protocol.SyncEndpoint{Executor: "src", Path: srcDir},
			Dst:      protocol.SyncEndpoint{Executor: "dst", Path: filepath.Join(dstRoot, "copy")},
			MaxBytes: &max,
		})
		ce := controllerErr(t, err)
		ck := assert.NewCollecting(t)
		ck.Eq(protocol.ErrInvalidArgs, ce.Code, "max_bytes=%d", max)
		// The message pins the DAEMON's refusal: the executor would also refuse a
		// present zero cap, but with its own wording.
		ck.Eq("max_bytes must be greater than zero", ce.Message, "max_bytes=%d message", max)
	}
	// A present, positive cap is accepted by this guard (the executor enforces
	// it): the destination must actually appear.
	ok := int64(1 << 20)
	_, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src:      protocol.SyncEndpoint{Executor: "src", Path: srcDir},
		Dst:      protocol.SyncEndpoint{Executor: "dst", Path: filepath.Join(dstRoot, "copy")},
		MaxBytes: &ok,
	})
	assert.NewAborting(t).NoError(err, "a positive max_bytes must be accepted")
}

// --- overwrite gate --------------------------------------------------------

func TestPathSyncOverwriteRefusedOnNativeDestination(t *testing.T) {
	ck := assert.NewCollecting(t)
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)

	srcDir := filepath.Join(srcRoot, "tree")
	ck.NoError(os.MkdirAll(srcDir, 0o755))
	writeFile(t, filepath.Join(srcDir, "new.txt"), "new", 0o644)

	dstDir := filepath.Join(dstRoot, "copy")
	ck.NoError(os.MkdirAll(dstDir, 0o755))
	writeFile(t, filepath.Join(dstDir, "stale.txt"), "stale", 0o644)

	// The row says native (Isolation ""); the self-report claims to be a
	// container. The ROW is the authority.
	dstLive := treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true)
	dstLive.Describe.Isolation = "container"
	dstLive.Describe.SandboxMountRoots = []string{"/work"}

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		dstLive,
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-src": srcClient, "exec-dst": dstClient})
	_, p := newPathSyncFixture(t, pool)

	_, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src:       protocol.SyncEndpoint{Executor: "src", Path: srcDir},
		Dst:       protocol.SyncEndpoint{Executor: "dst", Path: dstDir},
		Overwrite: true,
	})
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrPermissionDenied, ce.Code, "code")
	ck.Eq("overwrite is only allowed on container executors", ce.Message, "message")

	// Nothing was replaced.
	got, rerr := os.ReadFile(filepath.Join(dstDir, "stale.txt"))
	ck.NoError(rerr, "the destination must be untouched")
	ck.Eq("stale", string(got), "stale content")
}

func TestPathSyncOverwriteAllowedOnContainerDestination(t *testing.T) {
	ck := assert.NewCollecting(t)
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)

	srcDir := filepath.Join(srcRoot, "tree")
	ck.NoError(os.MkdirAll(srcDir, 0o755))
	writeFile(t, filepath.Join(srcDir, "new.txt"), "new", 0o644)

	dstDir := filepath.Join(dstRoot, "copy")
	ck.NoError(os.MkdirAll(dstDir, 0o755))
	writeFile(t, filepath.Join(dstDir, "stale.txt"), "stale", 0o644)

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "container", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-src": srcClient, "exec-dst": dstClient})
	_, p := newPathSyncFixture(t, pool)

	res, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src:       protocol.SyncEndpoint{Executor: "src", Path: srcDir},
		Dst:       protocol.SyncEndpoint{Executor: "dst", Path: dstDir},
		Overwrite: true,
	})
	ck.Require().NoError(err, "overwrite on a container destination")
	ck.Eq(int64(1), res.Files, "files")

	got, rerr := os.ReadFile(filepath.Join(dstDir, "new.txt"))
	ck.NoError(rerr, "the new tree must be in place")
	ck.Eq("new", string(got), "new content")
	_, serr := os.Stat(filepath.Join(dstDir, "stale.txt"))
	ck.True(os.IsNotExist(serr), "the old tree must be gone, got %v", serr)
}

// --- reachability: the generic arms ----------------------------------------

func TestPathSyncMissingAndForbiddenExecutorLookIdentical(t *testing.T) {
	ck := assert.NewCollecting(t)
	mineClient, _ := newTreeSyncExecutor(t)
	otherClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-mine", "u1", map[string]string{"machine": "mine"}, "", true),
		treeSyncExecutor("exec-other", "u2", map[string]string{"machine": "other"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-mine": mineClient, "exec-other": otherClient})
	_, p := newPathSyncFixture(t, pool)
	owner := users.Identity{UserID: "u1"}

	_, missingErr := p.resolve(t.Context(), owner, "", "gone")
	_, forbiddenErr := p.resolve(t.Context(), owner, "", "other")

	missing := controllerErr(t, missingErr)
	forbidden := controllerErr(t, forbiddenErr)
	ck.Eq(protocol.ErrNotFound, missing.Code, "missing code")
	ck.Eq(missing.Code, forbidden.Code, "the two must share a code")
	ck.Eq(`executor "gone" is not reachable by this caller`, missing.Message, "missing message")
	ck.Eq(`executor "other" is not reachable by this caller`, forbidden.Message, "forbidden message")
	// The ONLY difference is the ref the caller itself supplied.
	ck.Eq(
		strings.Replace(missing.Message, "gone", "REF", 1),
		strings.Replace(forbidden.Message, "other", "REF", 1),
		"the two refusals must be indistinguishable but for the ref",
	)
}

func TestPathSyncRefusesAnotherOwnersExecutor(t *testing.T) {
	ck := assert.NewCollecting(t)
	mineClient, _ := newTreeSyncExecutor(t)
	otherClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-mine", "u1", map[string]string{"machine": "mine"}, "", true),
		treeSyncExecutor("exec-other", "u2", map[string]string{"machine": "other"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-mine": mineClient, "exec-other": otherClient})
	_, p := newPathSyncFixture(t, pool)

	_, err := p.resolve(t.Context(), users.Identity{UserID: "u1"}, "", "other")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrNotFound, ce.Code, "code")
	ck.Eq(`executor "other" is not reachable by this caller`, ce.Message, "message")

	// And the verb refuses it, not just resolve.
	_, err = p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "mine", Path: "/work/src"},
		Dst: protocol.SyncEndpoint{Executor: "other", Path: "/work/dst"},
	})
	ce = controllerErr(t, err)
	ck.Eq(protocol.ErrNotFound, ce.Code, "SyncPath code")
}

// A DISABLED executor is not usable, for an operator just as for a child.
func TestPathSyncOperatorDoesNotReachDisabledExecutor(t *testing.T) {
	offClient, _ := newTreeSyncExecutor(t)
	off := treeSyncExecutor("exec-off", "u1", map[string]string{"machine": "off"}, "", true)
	off.Executor.Enabled = false
	pool := newTreeSyncPool([]execpool.LiveExecutor{off},
		map[string]executorpbconnect.ExecutorServiceClient{"exec-off": offClient})
	_, p := newPathSyncFixture(t, pool)

	assertNotReachable(t, p, "", "off")
	assertNotReachable(t, p, "", "exec-off")
}

// --- reachability: sandboxes enter only through their permitted rows ---------

// THE critical: a sandbox executor admits "" and so sits in EVERY empty-selector
// effective set. A child must not reach a sibling's sandbox through it.
func TestPathSyncChildCannotReachSiblingSandboxWithEmptySelector(t *testing.T) {
	homeClient, _ := newTreeSyncExecutor(t)
	sibClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home"}, "", true),
		treeSyncSandboxExecutor("exec-sibbox", "u1", "box-sib", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-home": homeClient, "exec-sibbox": sibClient})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	// An EMPTY selector chain: the effective set would otherwise contain the
	// sibling's sandbox executor.
	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	insertPathSyncChild(t, ctrl, "c_sib", "c_root", "")
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_sib", "sibbox", "u1", "exec-sibbox", "c_sib")))

	// By machine label, by id, by id fragment, by sandbox name, by row id.
	for _, ref := range []string{"box-sib", "exec-sibbox", "sibbox", "sbx_sib"} {
		assertNotReachable(t, p, "c_child", ref)
	}
	// The generic arm still works for the child.
	assertReachable(t, p, "c_child", "home", "exec-home")
}

// An unrelated TOP-LEVEL child's sandbox is not the caller's either.
func TestPathSyncChildCannotReachUnrelatedChildSandbox(t *testing.T) {
	homeClient, _ := newTreeSyncExecutor(t)
	unrelClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home"}, "", true),
		treeSyncSandboxExecutor("exec-unrelbox", "u1", "box-unrel", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-home": homeClient, "exec-unrelbox": unrelClient})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	insertPathSyncChild(t, ctrl, "c_unrel", "", "")
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_unrel", "unrelbox", "u1", "exec-unrelbox", "c_unrel")))

	for _, ref := range []string{"box-unrel", "exec-unrelbox", "unrelbox", "sbx_unrel"} {
		assertNotReachable(t, p, "c_child", ref)
	}
}

// A child reaches the sandbox it created itself.
func TestPathSyncChildReachesOwnSandbox(t *testing.T) {
	homeClient, _ := newTreeSyncExecutor(t)
	ownClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home"}, "", true),
		treeSyncSandboxExecutor("exec-ownbox", "u1", "box-own", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-home": homeClient, "exec-ownbox": ownClient})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_own", "ownbox", "u1", "exec-ownbox", "c_child")))

	// The CreatedBy arm: the caller created it.
	for _, ref := range []string{"box-own", "exec-ownbox", "ownbox", "sbx_own"} {
		assertReachable(t, p, "c_child", ref, "exec-ownbox")
	}
}

// The brief's original scenario: a child with a NON-empty selector chain
// reaches the sandbox it created, and never its sibling's.
func TestPathSyncChildReachesOwnSandboxNotSiblings(t *testing.T) {
	homeClient, _ := newTreeSyncExecutor(t)
	ownBoxClient, _ := newTreeSyncExecutor(t)
	sibBoxClient, _ := newTreeSyncExecutor(t)

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home", "env": "home"}, "", true),
		treeSyncSandboxExecutor("exec-ownbox", "u1", "box-own", "container"),
		treeSyncSandboxExecutor("exec-sibbox", "u1", "box-sib", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{
		"exec-home":   homeClient,
		"exec-ownbox": ownBoxClient,
		"exec-sibbox": sibBoxClient,
	})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	insertPathSyncChild(t, ctrl, "c_root", "", "env=home")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	insertPathSyncChild(t, ctrl, "c_sib", "c_root", "")

	// The child's own named sandbox, and its sibling's.
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_own", "ownbox", "u1", "exec-ownbox", "c_child")))
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_sib", "sibbox", "u1", "exec-sibbox", "c_sib")))

	// The parent's set alone would not admit its own sandbox (no env=home), so
	// naming the sandbox's EXECUTOR only resolves through the permitted rows.
	for _, ref := range []string{"box-own", "exec-ownbox", "ownbox", "sbx_own"} {
		assertReachable(t, p, "c_child", ref, "exec-ownbox")
	}
	// Its sibling's sandbox is in neither the effective set nor the rows it may
	// reach.
	for _, ref := range []string{"box-sib", "exec-sibbox", "sibbox", "sbx_sib"} {
		assertNotReachable(t, p, "c_child", ref)
	}
}

// A child reaches the SPAWN BLOCK written for it (OwnerChild == caller), even
// though neither it nor a descendant created the row and its scope is self.
func TestPathSyncChildReachesOwnSpawnBlock(t *testing.T) {
	homeClient, _ := newTreeSyncExecutor(t)
	blockClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home"}, "", true),
		treeSyncSandboxExecutor("exec-ownblock", "u1", "box-block", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-home": homeClient, "exec-ownblock": blockClient})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	insertPathSyncChild(t, ctrl, "c_sib", "c_root", "")
	// A self-scoped spawn block owned by the child, created by its parent: only
	// the OwnerChild arm admits it.
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxBlockRow("sbx_block", "u1", "exec-ownblock", "c_child", "c_root", protocol.ScopeSelf)))

	for _, ref := range []string{"box-block", "exec-ownblock", "sbx_block"} {
		assertReachable(t, p, "c_child", ref, "exec-ownblock")
	}
	// A sibling is not the block's owner, so it does not reach it.
	for _, ref := range []string{"box-block", "exec-ownblock", "sbx_block"} {
		assertNotReachable(t, p, "c_sib", ref)
	}
}

// A child reaches a sandbox a DESCENDANT of it created.
func TestPathSyncChildReachesDescendantCreatedSandbox(t *testing.T) {
	homeClient, _ := newTreeSyncExecutor(t)
	grandClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home"}, "", true),
		treeSyncSandboxExecutor("exec-grandbox", "u1", "box-grand", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-home": homeClient, "exec-grandbox": grandClient})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	insertPathSyncChild(t, ctrl, "c_grand", "c_child", "")
	// Created by the GRANDCHILD: only the descendant arm admits it to c_child.
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_grand", "grandbox", "u1", "exec-grandbox", "c_grand")))

	for _, ref := range []string{"box-grand", "exec-grandbox", "grandbox", "sbx_grand"} {
		assertReachable(t, p, "c_child", ref, "exec-grandbox")
	}
	// The grandchild reaches its own too.
	assertReachable(t, p, "c_grand", "grandbox", "exec-grandbox")
}

// A child reaches a SUBTREE-scoped block owned by an ancestor it descends from,
// and only a subtree-scoped one.
func TestPathSyncChildReachesAncestorSubtreeSandbox(t *testing.T) {
	homeClient, _ := newTreeSyncExecutor(t)
	subClient, _ := newTreeSyncExecutor(t)
	selfClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home"}, "", true),
		treeSyncSandboxExecutor("exec-subbox", "u1", "box-sub", "container"),
		treeSyncSandboxExecutor("exec-selfbox", "u1", "box-self", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{
		"exec-home": homeClient, "exec-subbox": subClient, "exec-selfbox": selfClient,
	})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	// A subtree block owned by the ancestor, and a SELF block with the same
	// shape — the second must stay out of reach.
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxBlockRow("sbx_sub", "u1", "exec-subbox", "c_root", "c_root", protocol.ScopeSubtree)))
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxBlockRow("sbx_self", "u1", "exec-selfbox", "c_root", "c_root", protocol.ScopeSelf)))

	for _, ref := range []string{"box-sub", "exec-subbox", "sbx_sub"} {
		assertReachable(t, p, "c_child", ref, "exec-subbox")
	}
	for _, ref := range []string{"box-self", "exec-selfbox", "sbx_self"} {
		assertNotReachable(t, p, "c_child", ref)
	}
}

// An operator reaches a sandbox only through its row, never through the generic
// owner-matched list: an orphaned sandbox executor is out of reach, while one
// with a live row is reachable by name AND by machine label.
func TestPathSyncOperatorReachesSandboxesOnlyThroughTheirRows(t *testing.T) {
	orphanClient, _ := newTreeSyncExecutor(t)
	childBoxClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncSandboxExecutor("exec-orphanbox", "u1", "box-orphan", "container"),
		treeSyncSandboxExecutor("exec-childbox", "u1", "box-child", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{
		"exec-orphanbox": orphanClient, "exec-childbox": childBoxClient,
	})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()
	insertPathSyncChild(t, ctrl, "c_child", "", "")

	// No row for the orphan: the generic arm must not carry it.
	assertNotReachable(t, p, "", "box-orphan")
	assertNotReachable(t, p, "", "exec-orphanbox")

	// Another child's sandbox, with a live row: the operator owns it, and
	// reaches it through the row.
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_child", "childbox", "u1", "exec-childbox", "c_child")))
	for _, ref := range []string{"box-child", "exec-childbox", "childbox", "sbx_child"} {
		assertReachable(t, p, "", ref, "exec-childbox")
	}
}

// The sandbox row is not enough: its executor's ROW must be owned by the same
// owner and enabled.
func TestPathSyncSandboxRowExecutorMustMatchOwner(t *testing.T) {
	foreignClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncSandboxExecutor("exec-foreignbox", "u2", "box-foreign", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-foreignbox": foreignClient})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()
	insertPathSyncChild(t, ctrl, "c_child", "", "")

	// The ROW belongs to u1; its executor belongs to u2.
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_foreign", "foreignbox", "u1", "exec-foreignbox", "c_child")))

	assertNotReachable(t, p, "", "foreignbox")
	assertNotReachable(t, p, "", "box-foreign")
	assertNotReachable(t, p, "", "exec-foreignbox")
}

func TestPathSyncSandboxRowExecutorMustBeEnabled(t *testing.T) {
	offBox := treeSyncSandboxExecutor("exec-offbox", "u1", "box-off", "container")
	offBox.Executor.Enabled = false
	pool := newTreeSyncPool([]execpool.LiveExecutor{offBox},
		map[string]executorpbconnect.ExecutorServiceClient{"exec-offbox": &observingClient{}})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()
	insertPathSyncChild(t, ctrl, "c_child", "", "")

	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_off", "offbox", "u1", "exec-offbox", "c_child")))

	assertNotReachable(t, p, "", "offbox")
	assertNotReachable(t, p, "", "box-off")
	assertNotReachable(t, p, "", "exec-offbox")
}

// --- reachability: name shadowing (no pass has priority) --------------------

// A sandbox named like a real machine, both reachable by the caller, is
// ambiguous — and nothing is sent to either.
func TestPathSyncAmbiguousRefRefusedWhenSandboxShadowsMachine(t *testing.T) {
	ck := assert.NewCollecting(t)
	homeObs := &observingClient{}
	boxObs := &observingClient{}
	homeClient, _ := newTreeSyncExecutor(t)
	boxClient, _ := newTreeSyncExecutor(t)
	homeObs.ExecutorServiceClient = homeClient
	boxObs.ExecutorServiceClient = boxClient

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-home", "u1", map[string]string{"machine": "home"}, "", true),
		treeSyncSandboxExecutor("exec-gbox", "u1", "box-g", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-home": homeObs, "exec-gbox": boxObs})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()

	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	insertPathSyncChild(t, ctrl, "c_grand", "c_child", "")
	// The descendant's sandbox is NAMED like the real machine "home".
	ck.NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_shadow", "home", "u1", "exec-gbox", "c_grand")))

	_, err := p.resolve(t.Context(), users.Identity{UserID: "u1"}, "c_child", "home")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrNotFound, ce.Code, "ambiguous code")
	ck.Eq(`executor "home" is not reachable by this caller (matches 2 executors)`, ce.Message, "ambiguous message")

	// Nothing was sent to either executor.
	_, err = p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "c_child", protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "home", Path: "/work/src"},
		Dst: protocol.SyncEndpoint{Executor: "home", Path: "/work/dst"},
	})
	ce = controllerErr(t, err)
	ck.Eq(protocol.ErrNotFound, ce.Code, "SyncPath code")
	for _, obs := range []*observingClient{homeObs, boxObs} {
		reads, writes := obs.counts()
		ck.Eq(0, reads, "no ReadTree may reach an ambiguous ref")
		ck.Eq(0, writes, "no WriteTree may reach an ambiguous ref")
	}
}

// The same name, with no clash, resolves.
func TestPathSyncSandboxNameWithoutClashResolves(t *testing.T) {
	boxClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncSandboxExecutor("exec-box", "u1", "box", "container"),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-box": boxClient})
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()
	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")
	assert.NewAborting(t).NoError(ctrl.sandboxStore.Insert(t.Context(),
		sandboxRowFor("sbx_box", "box", "u1", "exec-box", "c_child")))

	// Name and machine label coincide, and they name the SAME executor: one
	// match, not a false ambiguity.
	assertReachable(t, p, "c_child", "box", "exec-box")
	// Exact id and id fragment likewise dedupe.
	assertReachable(t, p, "c_child", "exec-box", "exec-box")
	assertReachable(t, p, "c_child", "c-box", "exec-box")
	// And the row id.
	assertReachable(t, p, "c_child", "sbx_box", "exec-box")
}

// --- relay -----------------------------------------------------------------

func TestPathSyncCopiesTreeBetweenExecutors(t *testing.T) {
	ck := assert.NewCollecting(t)
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)

	script := "#!/bin/sh\necho hi\n"
	srcDir := filepath.Join(srcRoot, "tree")
	ck.NoError(os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755))
	writeFile(t, filepath.Join(srcDir, "a.txt"), "hello", 0o644)
	writeFile(t, filepath.Join(srcDir, "run.sh"), script, 0o755)

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-src": srcClient, "exec-dst": dstClient})
	_, p := newPathSyncFixture(t, pool)

	dstDir := filepath.Join(dstRoot, "copy")
	res, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "src", Path: srcDir},
		Dst: protocol.SyncEndpoint{Executor: "dst", Path: dstDir},
	})
	ck.Require().NoError(err, "sync")
	ck.Eq(int64(2), res.Files, "files")
	ck.Eq(int64(len("hello")+len(script)), res.Bytes, "bytes")

	got, rerr := os.ReadFile(filepath.Join(dstDir, "a.txt"))
	ck.NoError(rerr, "a.txt")
	ck.Eq("hello", string(got), "a.txt content")

	runGot, rerr := os.ReadFile(filepath.Join(dstDir, "run.sh"))
	ck.NoError(rerr, "run.sh")
	ck.Eq(script, string(runGot), "run.sh content")
	fi, serr := os.Stat(filepath.Join(dstDir, "run.sh"))
	ck.NoError(serr, "stat run.sh")
	if fi != nil {
		ck.True(fi.Mode()&0o111 != 0, "the executable bit must survive: mode %v", fi.Mode())
	}
}

func TestPathSyncDestinationFailureIsReportedAsDestination(t *testing.T) {
	ck := assert.NewCollecting(t)
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)

	srcDir := filepath.Join(srcRoot, "tree")
	ck.NoError(os.MkdirAll(srcDir, 0o755))
	writeFile(t, filepath.Join(srcDir, "a.txt"), "hello", 0o644)

	// A non-empty destination without overwrite is refused by the executor.
	dstDir := filepath.Join(dstRoot, "copy")
	ck.NoError(os.MkdirAll(dstDir, 0o755))
	writeFile(t, filepath.Join(dstDir, "existing.txt"), "x", 0o644)

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-src": srcClient, "exec-dst": dstClient})
	_, p := newPathSyncFixture(t, pool)

	_, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "src", Path: srcDir},
		Dst: protocol.SyncEndpoint{Executor: "dst", Path: dstDir},
	})
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "code")
	ck.True(strings.HasPrefix(ce.Message, "destination: "), "the failure must name the destination, got %q", ce.Message)
}

// A destination that refuses MID-STREAM must surface its own reason: the
// relay's Send only sees io.EOF once the executor has answered and closed.
func TestPathSyncMidStreamDestinationRefusalKeepsItsReason(t *testing.T) {
	ck := assert.NewCollecting(t)
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)

	// A source larger than the caller's cap, so the destination aborts while
	// the relay is still streaming. It must be large enough that the abort
	// propagates back to the relay's Send — a source small enough to fit in the
	// transport's buffers would surface the error at CloseAndReceive instead and
	// never exercise the Send path.
	big := filepath.Join(srcRoot, "big.bin")
	f, ferr := os.Create(big)
	ck.Require().NoError(ferr, "create big file")
	ck.Require().NoError(f.Truncate(64<<20), "truncate")
	ck.Require().NoError(f.Close(), "close")

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-src": srcClient, "exec-dst": dstClient})
	_, p := newPathSyncFixture(t, pool)

	max := int64(10)
	_, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src:      protocol.SyncEndpoint{Executor: "src", Path: big},
		Dst:      protocol.SyncEndpoint{Executor: "dst", Path: filepath.Join(dstRoot, "copy")},
		MaxBytes: &max,
	})
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrInvalidArgs, ce.Code, "code")
	ck.Eq("destination: tree stream exceeds max_bytes 10", ce.Message, "the destination's own reason")
	ck.False(strings.Contains(ce.Message, "executor request failed"),
		"the opaque fallback must not replace the destination's reason, got %q", ce.Message)
}

func TestPathSyncCancelUnwindsBothStreams(t *testing.T) {
	ck := assert.NewCollecting(t)
	srcClient, srcRoot := newTreeSyncExecutor(t)
	dstClient, dstRoot := newTreeSyncExecutor(t)

	// A large source keeps the relay running long enough that cancellation
	// lands mid-transfer.
	big := filepath.Join(srcRoot, "big.bin")
	f, ferr := os.Create(big)
	ck.Require().NoError(ferr, "create big file")
	ck.Require().NoError(f.Truncate(256<<20), "truncate")
	ck.Require().NoError(f.Close(), "close")

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-src": srcClient, "exec-dst": dstClient})
	_, p := newPathSyncFixture(t, pool)

	dstDir := filepath.Join(dstRoot, "copy")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := p.SyncPath(ctx, users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
			Src: protocol.SyncEndpoint{Executor: "src", Path: big},
			Dst: protocol.SyncEndpoint{Executor: "dst", Path: dstDir},
		})
		done <- err
	}()

	// Cancel once the destination has begun extracting — its staging directory
	// is the observable proof that both streams were live.
	staging := func() bool {
		entries, rerr := os.ReadDir(dstRoot)
		if rerr != nil {
			return false
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".rafiki-sync-") {
				return true
			}
		}
		return false
	}
	if !pollUntil(staging, 10*time.Second) {
		_, serr := os.Stat(dstDir)
		t.Fatalf("the destination never began extracting (destination exists=%v, err=%v); cannot test cancellation", serr == nil, serr)
	}
	cancel()

	select {
	case err := <-done:
		ck.Error(err, "a cancelled transfer must return an error")
	case <-time.After(10 * time.Second):
		t.Fatal("SyncPath did not unwind after cancellation")
	}

	_, serr := os.Stat(dstDir)
	ck.True(os.IsNotExist(serr), "a cancelled transfer must not publish a destination, stat err=%v", serr)
	gone := pollUntil(func() bool {
		entries, rerr := os.ReadDir(dstRoot)
		if rerr != nil {
			return false
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".rafiki-sync-") {
				return false
			}
		}
		return true
	}, 10*time.Second)
	ck.True(gone, "the destination's staging directory must be cleaned up after cancellation")
}

// When ONE side fails, the derived context must be cancelled so the other
// stream unwinds too: both streams are opened with that one context, so a
// cancelled recorded context is the stream-closed assertion for both, and no
// handler is left running.
func TestPathSyncFailureCancelsBothStreams(t *testing.T) {
	ck := assert.NewCollecting(t)
	realSrc, srcRoot := newTreeSyncExecutor(t)
	realDst, dstRoot := newTreeSyncExecutor(t)
	srcObs := &observingClient{ExecutorServiceClient: realSrc}
	dstObs := &observingClient{ExecutorServiceClient: realDst}

	big := filepath.Join(srcRoot, "big.bin")
	f, ferr := os.Create(big)
	ck.Require().NoError(ferr, "create big file")
	ck.Require().NoError(f.Truncate(8<<20), "truncate")
	ck.Require().NoError(f.Close(), "close")

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-src": srcObs, "exec-dst": dstObs})
	_, p := newPathSyncFixture(t, pool)

	max := int64(10)
	_, err := p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src:      protocol.SyncEndpoint{Executor: "src", Path: big},
		Dst:      protocol.SyncEndpoint{Executor: "dst", Path: filepath.Join(dstRoot, "copy")},
		MaxBytes: &max,
	})
	ck.Error(err, "the destination's refusal must fail the transfer")

	reads, srcWrites := srcObs.counts()
	ck.Eq(1, reads, "the source stream must have been opened exactly once")
	ck.Eq(0, srcWrites, "the source is never a WriteTree target")
	_, dstWrites := dstObs.counts()
	ck.Eq(1, dstWrites, "the destination stream must have been opened exactly once")

	readCtx, _ := srcObs.streamContexts()
	_, writeCtx := dstObs.streamContexts()
	ck.True(readCtx != nil, "the source stream's context must be recorded")
	ck.True(writeCtx != nil, "the destination stream's context must be recorded")
	if readCtx != nil {
		ck.True(readCtx.Err() != nil, "the source stream's context must be cancelled when the destination fails")
	}
	if writeCtx != nil {
		ck.True(writeCtx.Err() != nil, "the destination stream's context must be cancelled")
	}
}

// --- misc guards -----------------------------------------------------------

func TestPathSyncResolveWithoutController(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := newTreeSyncPool(nil, nil)
	p := newPathSyncer(nil, pool)

	_, err := p.resolve(t.Context(), users.Identity{UserID: "u1"}, "", "anything")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrInternal, ce.Code, "code")
}

func TestPathSyncRefusesIncapableExecutor(t *testing.T) {
	ck := assert.NewCollecting(t)
	oldClient, _ := newTreeSyncExecutor(t)
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-old", "u1", map[string]string{"machine": "old"}, "", false),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-old": oldClient})
	_, p := newPathSyncFixture(t, pool)

	_, err := p.resolve(t.Context(), users.Identity{UserID: "u1"}, "", "old")
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "code")
	ck.Eq(`executor "old" does not support tree sync; upgrade it`, ce.Message, "message")

	_, err = p.SyncPath(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncPathRequest{
		Src: protocol.SyncEndpoint{Executor: "old", Path: "/work/src"},
		Dst: protocol.SyncEndpoint{Executor: "old", Path: "/work/dst"},
	})
	ce = controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "SyncPath code")
}

func TestPathSyncRefusesOfflineExecutor(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := newTreeSyncPool(nil, nil)
	ctrl, p := newPathSyncFixture(t, pool)
	ctrl.sandboxStore = newFakeSandboxStore()
	// The executor's ROW still exists — the sandbox is down, not gone — so the
	// sandbox row is reachable and the refusal is "not connected".
	execStore := newFakeExecStore()
	execStore.execs["exec-dead"] = executors.Executor{
		ID: "exec-dead", Enabled: true, OwnerUserID: "u1",
		Labels: map[string]string{"machine": "box-dead", sandbox.RowLabelSandbox: "1"},
	}
	ctrl.execStore = execStore

	insertPathSyncChild(t, ctrl, "c_root", "", "")
	insertPathSyncChild(t, ctrl, "c_child", "c_root", "")

	// A permitted sandbox row whose executor has no connection.
	ck.NoError(ctrl.sandboxStore.Insert(t.Context(), sandboxRowFor("sbx_dead", "deadbox", "u1", "exec-dead", "c_child")))

	for _, ref := range []string{"deadbox", "box-dead", "exec-dead"} {
		_, err := p.resolve(t.Context(), users.Identity{UserID: "u1"}, "c_child", ref)
		ce := controllerErr(t, err)
		ck.Eq(protocol.ErrNotFound, ce.Code, "%q code", ref)
		ck.Eq(fmt.Sprintf("executor %q is not connected", ref), ce.Message, "%q message", ref)
	}
}

// --- executorErr mapping ---------------------------------------------------

func TestExecutorErrMapping(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want string
		msg  string
	}{
		{"invalid argument", connect.NewError(connect.CodeInvalidArgument, errors.New("bad path")), protocol.ErrInvalidArgs, "source: bad path"},
		{"not found", connect.NewError(connect.CodeNotFound, errors.New("missing")), protocol.ErrNotFound, "source: missing"},
		{"permission denied", connect.NewError(connect.CodePermissionDenied, errors.New("nope")), protocol.ErrPermissionDenied, "source: nope"},
		{"failed precondition", connect.NewError(connect.CodeFailedPrecondition, errors.New("destination exists")), protocol.ErrFailedPrecondition, "source: destination exists"},
		{"resource exhausted", connect.NewError(connect.CodeResourceExhausted, errors.New("no space left")), protocol.ErrInvalidArgs, "source: no space left"},
		{"other connect code", connect.NewError(connect.CodeUnavailable, errors.New("host 10.0.0.1 refused")), protocol.ErrInternal, "source: executor request failed"},
		{"non-connect error", errors.New("dial tcp 10.0.0.1: connect: connection refused"), protocol.ErrInternal, "source: executor request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			err := executorErr("source", tc.in)
			ce := controllerErr(t, err)
			ck.Eq(tc.want, ce.Code, "code")
			ck.Eq(tc.msg, ce.Message, "message")
		})
	}
	// The side prefixes the message.
	ce := controllerErr(t, executorErr("destination", connect.NewError(connect.CodeNotFound, errors.New("gone"))))
	assert.NewCollecting(t).Eq("destination: gone", ce.Message, "side prefix")
}

// TestPathSyncExecutorErrMapping runs the pinned TestExecutorErrMapping body
// under the task's verify pattern: `go test -run TestPathSync` is an
// unanchored substring match, so without this shim the table would silently
// not run.
func TestPathSyncExecutorErrMapping(t *testing.T) {
	t.Run("TestExecutorErrMapping", TestExecutorErrMapping)
}
