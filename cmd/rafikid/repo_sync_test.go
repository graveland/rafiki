// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// --- harness ----------------------------------------------------------------

// withTmpDir runs fn with TMPDIR pointing at dir. t.Setenv checks the error and
// restores the environment when the test ends, and also forbids t.Parallel —
// this harness relies on both.
func withTmpDir(t *testing.T, dir string, fn func()) {
	t.Helper()
	t.Setenv("TMPDIR", dir)
	fn()
}

// repoScratchClient pins ONE endpoint's scratch directory.
//
// The executor's scratch directory is os.TempDir()/rafiki-sync, read live on
// every call. Both real executors in this harness run inside the test process,
// so they would otherwise share one scratch directory: the source would write
// its bundle there and the daemon would then relay that file onto itself, which
// the destination refuses as "destination exists". Giving each endpoint its own
// TMPDIR only for the three unary RPCs that consult it keeps the two executors
// genuinely independent, as two machines are in production. Each handler runs
// to completion inside the window, so the value it observes is this endpoint's.
type repoScratchClient struct {
	t *testing.T
	executorpbconnect.ExecutorServiceClient
	tmp string
}

func (c *repoScratchClient) GitRefs(ctx context.Context, req *connect.Request[executorpb.GitRefsRequest]) (*connect.Response[executorpb.GitRefsResponse], error) {
	var resp *connect.Response[executorpb.GitRefsResponse]
	var err error
	withTmpDir(c.t, c.tmp, func() { resp, err = c.ExecutorServiceClient.GitRefs(ctx, req) })
	return resp, err
}

func (c *repoScratchClient) GitBundle(ctx context.Context, req *connect.Request[executorpb.GitBundleRequest]) (*connect.Response[executorpb.GitBundleResponse], error) {
	var resp *connect.Response[executorpb.GitBundleResponse]
	var err error
	withTmpDir(c.t, c.tmp, func() { resp, err = c.ExecutorServiceClient.GitBundle(ctx, req) })
	return resp, err
}

func (c *repoScratchClient) GitFetchBundle(ctx context.Context, req *connect.Request[executorpb.GitFetchBundleRequest]) (*connect.Response[executorpb.GitFetchBundleResponse], error) {
	var resp *connect.Response[executorpb.GitFetchBundleResponse]
	var err error
	withTmpDir(c.t, c.tmp, func() { resp, err = c.ExecutorServiceClient.GitFetchBundle(ctx, req) })
	return resp, err
}

// repoEndpoint is one real executor's working-tree root.
type repoEndpoint struct {
	root string
}

// repoSyncFixture wires a Controller and pathSyncer over two REAL executor
// servers (real git, separate temp roots and separate scratch directories), and
// observes the RPCs each endpoint is asked for.
type repoSyncFixture struct {
	p      *pathSyncer
	src    repoEndpoint
	dst    repoEndpoint
	srcObs *observingClient
	dstObs *observingClient
	owner  users.Identity
}

func newRepoSyncFixture(t *testing.T) *repoSyncFixture {
	return newRepoSyncFixtureWith(t, nil, nil)
}

// newRepoSyncFixtureWith builds the fixture and lets a test decorate either
// endpoint's client (nil means the unmodified real client). A decorator sits
// between the observability wrapper and the scratch-pinned real client, so a
// fabricated reply is attributed to the endpoint a test means.
func newRepoSyncFixtureWith(t *testing.T, decorateSrc, decorateDst func(executorpbconnect.ExecutorServiceClient) executorpbconnect.ExecutorServiceClient) *repoSyncFixture {
	t.Helper()
	srcReal, srcRoot := newTreeSyncExecutor(t)
	dstReal, dstRoot := newTreeSyncExecutor(t)

	srcScratch := t.TempDir()
	dstScratch := t.TempDir()
	var srcClient executorpbconnect.ExecutorServiceClient = &repoScratchClient{t: t, ExecutorServiceClient: srcReal, tmp: srcScratch}
	var dstClient executorpbconnect.ExecutorServiceClient = &repoScratchClient{t: t, ExecutorServiceClient: dstReal, tmp: dstScratch}
	if decorateSrc != nil {
		srcClient = decorateSrc(srcClient)
	}
	if decorateDst != nil {
		dstClient = decorateDst(dstClient)
	}
	srcObs := &observingClient{ExecutorServiceClient: srcClient}
	dstObs := &observingClient{ExecutorServiceClient: dstClient}

	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-src", "u1", map[string]string{"machine": "src"}, "", true),
		treeSyncExecutor("exec-dst", "u1", map[string]string{"machine": "dst"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{
		"exec-src": srcObs,
		"exec-dst": dstObs,
	})
	_, p := newPathSyncFixture(t, pool)
	return &repoSyncFixture{
		p:      p,
		src:    repoEndpoint{root: srcRoot},
		dst:    repoEndpoint{root: dstRoot},
		srcObs: srcObs,
		dstObs: dstObs,
		owner:  users.Identity{UserID: "u1"},
	}
}

// sync runs SyncRepo with the fixture's executors and roots.
func (f *repoSyncFixture) sync(t *testing.T, srcPath, dstPath, branch string, force bool) (protocol.SyncRepoResult, error) {
	t.Helper()
	return f.p.SyncRepo(t.Context(), f.owner, "", protocol.SyncRepoRequest{
		Src:    protocol.SyncEndpoint{Executor: "src", Path: srcPath},
		Dst:    protocol.SyncEndpoint{Executor: "dst", Path: dstPath},
		Branch: branch,
		Force:  force,
	})
}

// --- git helpers ------------------------------------------------------------

// gitExec runs one git command in dir under a hermetic environment: no user or
// system git config, and a fixed identity so commits are reproducible.
func gitExec(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates a repository at dir with branch checked out, then makes one
// commit per named file. It returns the tip oid.
func initRepo(t *testing.T, dir, branch string, files ...string) string {
	t.Helper()
	assert.NewAborting(t).NoError(os.MkdirAll(dir, 0o755), "mkdirall %s", dir)
	gitExec(t, dir, "init", "-q", "-b", branch)
	return commitFiles(t, dir, files...)
}

// commitFiles writes each named file, adds and commits them, and returns the
// new tip oid.
func commitFiles(t *testing.T, dir string, files ...string) string {
	t.Helper()
	for _, name := range files {
		writeFile(t, filepath.Join(dir, name), name, 0o644)
	}
	gitExec(t, dir, "add", "-A")
	gitExec(t, dir, "commit", "-q", "-m", "add "+strings.Join(files, ","))
	return gitExec(t, dir, "rev-parse", "HEAD")
}

// detach frees the current branch so a later fetch may update it. git refuses
// to fetch into the branch that is checked out, so a test that wants the
// destination's own update rules (fast-forward, force) rather than the
// checked-out refusal must first step off the branch.
func detach(t *testing.T, dir string) {
	t.Helper()
	gitExec(t, dir, "checkout", "-q", "--detach")
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return gitExec(t, dir, "rev-parse", rev)
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return gitExec(t, dir, "symbolic-ref", "--short", "HEAD")
}

// --- branch validation ------------------------------------------------------

func TestRepoSyncRefusesBadBranch(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, branch := range []string{"", "-upload-pack=x", "--force"} {
		t.Run(branch, func(t *testing.T) {
			f := newRepoSyncFixture(t)
			_, err := f.sync(t,
				filepath.Join(f.src.root, "repo"),
				filepath.Join(f.dst.root, "repo"),
				branch, false)
			ce := controllerErr(t, err)
			ck.Eq(protocol.ErrInvalidArgs, ce.Code, "branch %q code", branch)
			if branch == "" {
				ck.Eq("branch is required", ce.Message, "empty branch message")
			} else {
				ck.Eq("branch must not start with '-'", ce.Message, "dash branch message")
			}
			// The refusal is the daemon's, before any executor is touched.
			srcReads, _ := f.srcObs.counts()
			dstReads, _ := f.dstObs.counts()
			ck.Eq(0, srcReads, "no source RPC for branch %q", branch)
			ck.Eq(0, dstReads, "no destination RPC for branch %q", branch)
		})
	}
}

// --- source validation ------------------------------------------------------

func TestRepoSyncRefusesNonRepoSource(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)
	plain := filepath.Join(f.src.root, "not-a-repo")
	ck.NoError(os.MkdirAll(plain, 0o755))

	_, err := f.sync(t, plain, filepath.Join(f.dst.root, "repo"), "main", false)
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrNotFound, ce.Code, "code")
	ck.Eq("source is not a git repository", ce.Message, "message")
}

// --- seeding ----------------------------------------------------------------

func TestRepoSyncSeedsMissingDestination(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)
	srcRepo := filepath.Join(f.src.root, "repo")
	tip := initRepo(t, srcRepo, "main", "hello.txt")
	dstRepo := filepath.Join(f.dst.root, "repo")

	res, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ck.Require().NoError(err, "seed")
	ck.True(res.CreatedRepo, "the destination repository must be reported as created")
	ck.False(res.UpToDate, "a seed is not up to date")
	ck.Eq("", res.OldOID, "a created repository has no old tip")
	ck.Eq(tip, res.NewOID, "new tip")

	// Real git state on the destination: the branch exists, is checked out,
	// and carries the source's content.
	ck.Eq(tip, revParse(t, dstRepo, "refs/heads/main"), "destination branch tip")
	ck.Eq("main", currentBranch(t, dstRepo), "the seeded branch must be checked out")
	got, rerr := os.ReadFile(filepath.Join(dstRepo, "hello.txt"))
	ck.NoError(rerr, "seeded working tree")
	ck.Eq("hello.txt", string(got), "seeded content")
}

// --- incremental second round ----------------------------------------------

func TestRepoSyncSecondRoundCarriesOnlyNewCommits(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)
	srcRepo := filepath.Join(f.src.root, "repo")
	first := initRepo(t, srcRepo, "main", "one.txt")
	dstRepo := filepath.Join(f.dst.root, "repo")

	res, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ck.Require().NoError(err, "first round")
	ck.Eq(first, res.NewOID, "first tip")
	// Step off the seeded branch so the second round exercises the fetch's own
	// fast-forward rule rather than the checked-out refusal.
	detach(t, dstRepo)

	second := commitFiles(t, srcRepo, "two.txt")
	ck.NotEq(first, second, "the second commit must move the tip")

	_, dstWritesBefore := f.dstObs.counts()
	res, err = f.sync(t, srcRepo, dstRepo, "main", false)
	ck.Require().NoError(err, "second round")
	ck.False(res.CreatedRepo, "the second round must not recreate the repository")
	ck.False(res.UpToDate, "the second round has something to send")
	ck.Eq(first, res.OldOID, "old tip is what the destination had")
	ck.Eq(second, res.NewOID, "new tip is the source's tip")
	ck.Eq(second, revParse(t, dstRepo, "refs/heads/main"), "destination tip after the second round")

	// Only the new commit travelled: the destination already had everything
	// else, so the source's exclusion reduced the bundle to two.txt. The
	// destination's ref (not its working tree — a fetch never touches one) now
	// carries exactly the new file.
	ck.Eq("two.txt", gitExec(t, dstRepo, "show", "refs/heads/main:two.txt"), "new content")
	_, dstWritesAfter := f.dstObs.counts()
	ck.Eq(1, dstWritesAfter-dstWritesBefore, "the second round relays exactly one bundle")
}

// --- up to date -------------------------------------------------------------

func TestRepoSyncUpToDateTransfersNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)
	srcRepo := filepath.Join(f.src.root, "repo")
	tip := initRepo(t, srcRepo, "main", "one.txt")
	dstRepo := filepath.Join(f.dst.root, "repo")

	_, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ck.Require().NoError(err, "seed")

	_, dstWritesBefore := f.dstObs.counts()
	srcReadsBefore, _ := f.srcObs.counts()
	res, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ck.Require().NoError(err, "up-to-date round")
	ck.True(res.UpToDate, "nothing has changed")
	ck.Eq(tip, res.OldOID, "old tip")
	ck.Eq(tip, res.NewOID, "new tip")
	ck.False(res.CreatedRepo, "an existing repository is not created")

	// The short-circuit must not relay anything: no WriteTree reached the
	// destination, and no ReadTree left the source.
	_, writesAfter := f.dstObs.counts()
	srcReadsAfter, _ := f.srcObs.counts()
	ck.Eq(dstWritesBefore, writesAfter, "an up-to-date round must not write a tree")
	ck.Eq(srcReadsBefore, srcReadsAfter, "an up-to-date round must not open a ReadTree on the source")
}

// --- fast-forward rules -----------------------------------------------------

func TestRepoSyncNonFastForwardRefusedUnlessForce(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)

	// Unrelated histories: neither tip is an ancestor of the other.
	dstRepo := filepath.Join(f.dst.root, "repo")
	dstTip := initRepo(t, dstRepo, "main", "dst.txt")
	detach(t, dstRepo)
	srcRepo := filepath.Join(f.src.root, "repo")
	srcTip := initRepo(t, srcRepo, "main", "src.txt")

	_, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "non-fast-forward must be refused")
	ck.True(strings.HasPrefix(ce.Message, "dst fetch: "), "the refusal names the failing step, got %q", ce.Message)
	ck.Eq(dstTip, revParse(t, dstRepo, "refs/heads/main"), "a refused fetch leaves the destination tip alone")

	res, err := f.sync(t, srcRepo, dstRepo, "main", true)
	ck.Require().NoError(err, "force accepts the rewrite")
	ck.Eq(dstTip, res.OldOID, "old tip")
	ck.Eq(srcTip, res.NewOID, "new tip")
	ck.False(res.CreatedRepo, "not a created repository")
	ck.Eq(srcTip, revParse(t, dstRepo, "refs/heads/main"), "destination tip after force")
}

func TestRepoSyncRefusesCheckedOutDestinationBranch(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)

	// The source fast-forwards the destination: the ONLY reason to refuse is
	// that the destination has the branch checked out.
	dstRepo := filepath.Join(f.dst.root, "repo")
	base := initRepo(t, dstRepo, "main", "base.txt")
	srcRepo := filepath.Join(f.src.root, "repo")
	ck.NoError(os.MkdirAll(filepath.Dir(srcRepo), 0o755))
	gitExec(t, f.src.root, "clone", "-q", dstRepo, srcRepo)
	tip := commitFiles(t, srcRepo, "next.txt")
	ck.NotEq(base, tip, "the clone's new commit must move the tip")

	_, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrFailedPrecondition, ce.Code, "a checked-out destination branch must be refused")
	ck.True(strings.HasPrefix(ce.Message, "dst fetch: "), "the refusal names the failing step, got %q", ce.Message)
	ck.Eq(base, revParse(t, dstRepo, "refs/heads/main"), "the destination tip is unchanged")
}

// --- round trip -------------------------------------------------------------

func TestRepoSyncRoundTrip(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)

	// A → B seeds B.
	aRepo := filepath.Join(f.src.root, "a")
	aTip := initRepo(t, aRepo, "main", "a.txt")
	bRepo := filepath.Join(f.dst.root, "b")

	res, err := f.sync(t, aRepo, bRepo, "main", false)
	ck.Require().NoError(err, "A → B")
	ck.True(res.CreatedRepo, "B is created")
	ck.Eq(aTip, res.NewOID, "B's tip is A's tip")

	// Commit on B, then step A off its branch so the reverse fetch is the
	// fast-forward under test rather than the checked-out refusal.
	bTip := commitFiles(t, bRepo, "b.txt")
	detach(t, aRepo)

	res, err = f.sync(t, bRepo, aRepo, "main", false)
	ck.Require().NoError(err, "B → A")
	ck.False(res.CreatedRepo, "A already exists")
	ck.Eq(aTip, res.OldOID, "A's old tip")
	ck.Eq(bTip, res.NewOID, "A's new tip is B's commit")
	ck.Eq(bTip, revParse(t, aRepo, "refs/heads/main"), "A's tip after the round trip")
}

// --- step attribution -------------------------------------------------------

func TestRepoSyncNamesFailingStep(t *testing.T) {
	ck := assert.NewCollecting(t)

	// A symlinked repo path is refused by the executor's own path validation.
	symlinkRepo := func(t *testing.T, root string) string {
		t.Helper()
		real := filepath.Join(root, "real")
		assert.NewAborting(t).NoError(os.MkdirAll(real, 0o755))
		link := filepath.Join(root, "link")
		assert.NewAborting(t).NoError(os.Symlink(real, link))
		return link
	}

	t.Run("dst refs", func(t *testing.T) {
		f := newRepoSyncFixture(t)
		srcRepo := filepath.Join(f.src.root, "repo")
		initRepo(t, srcRepo, "main", "one.txt")

		_, err := f.sync(t, srcRepo, symlinkRepo(t, f.dst.root), "main", false)
		ce := controllerErr(t, err)
		ck.True(strings.HasPrefix(ce.Message, "dst refs: "), "got %q", ce.Message)
	})

	t.Run("src refs", func(t *testing.T) {
		f := newRepoSyncFixture(t)
		_, err := f.sync(t, symlinkRepo(t, f.src.root), filepath.Join(f.dst.root, "repo"), "main", false)
		ce := controllerErr(t, err)
		ck.True(strings.HasPrefix(ce.Message, "src refs: "), "got %q", ce.Message)
	})

	t.Run("src bundle", func(t *testing.T) {
		f := newRepoSyncFixture(t)
		srcRepo := filepath.Join(f.src.root, "repo")
		initRepo(t, srcRepo, "main", "one.txt")

		_, err := f.sync(t, srcRepo, filepath.Join(f.dst.root, "repo"), "missing", false)
		ce := controllerErr(t, err)
		ck.Eq(protocol.ErrNotFound, ce.Code, "a missing source branch is not found")
		ck.True(strings.HasPrefix(ce.Message, "src bundle: "), "got %q", ce.Message)
	})

	t.Run("dst fetch", func(t *testing.T) {
		f := newRepoSyncFixture(t)
		dstRepo := filepath.Join(f.dst.root, "repo")
		initRepo(t, dstRepo, "main", "base.txt")
		srcRepo := filepath.Join(f.src.root, "repo")
		gitExec(t, f.src.root, "clone", "-q", dstRepo, srcRepo)
		commitFiles(t, srcRepo, "next.txt")

		_, err := f.sync(t, srcRepo, dstRepo, "main", false)
		ce := controllerErr(t, err)
		ck.True(strings.HasPrefix(ce.Message, "dst fetch: "), "got %q", ce.Message)
	})
}

// --- fabricated endpoints ---------------------------------------------------

// repoRecordingSource records every GitBundle exclude list the daemon sends,
// then answers with the real executor — so what the daemon forwards to a
// source can be asserted without changing the flow.
type repoRecordingSource struct {
	executorpbconnect.ExecutorServiceClient
	mu       sync.Mutex
	excludes [][]string
}

func (c *repoRecordingSource) GitBundle(ctx context.Context, req *connect.Request[executorpb.GitBundleRequest]) (*connect.Response[executorpb.GitBundleResponse], error) {
	c.mu.Lock()
	c.excludes = append(c.excludes, append([]string(nil), req.Msg.GetExcludeOids()...))
	c.mu.Unlock()
	return c.ExecutorServiceClient.GitBundle(ctx, req)
}

func (c *repoRecordingSource) lastExcludes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.excludes) == 0 {
		return nil
	}
	return c.excludes[len(c.excludes)-1]
}

// repoScriptedBundleSource answers GitBundle with a fabricated reply; every
// other RPC reaches the real executor.
type repoScriptedBundleSource struct {
	executorpbconnect.ExecutorServiceClient
	reply *executorpb.GitBundleResponse
}

func (c *repoScriptedBundleSource) GitBundle(context.Context, *connect.Request[executorpb.GitBundleRequest]) (*connect.Response[executorpb.GitBundleResponse], error) {
	return connect.NewResponse(c.reply), nil
}

// repoScriptedRefsDestination rewrites the destination's GitRefs reply (the
// heads only, keeping the real scratch dir) to exercise a destination the
// daemon does not control.
type repoScriptedRefsDestination struct {
	executorpbconnect.ExecutorServiceClient
	rewrite func(*executorpb.GitRefsResponse)
}

func (c *repoScriptedRefsDestination) GitRefs(ctx context.Context, req *connect.Request[executorpb.GitRefsRequest]) (*connect.Response[executorpb.GitRefsResponse], error) {
	resp, err := c.ExecutorServiceClient.GitRefs(ctx, req)
	if err != nil {
		return resp, err
	}
	c.rewrite(resp.Msg)
	return resp, nil
}

// --- up-to-date means the destination BRANCH is at the tip -------------------

// A destination head other than the requested branch holding the source tip
// must not short-circuit: the requested branch does not exist yet.
func TestRepoSyncCreatesBranchSharingCommitWithAnotherHead(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)

	dstRepo := filepath.Join(f.dst.root, "repo")
	tip := initRepo(t, dstRepo, "main", "one.txt")
	// The source's dev sits on the SAME commit as the destination's main.
	srcRepo := filepath.Join(f.src.root, "repo")
	gitExec(t, f.src.root, "clone", "-q", dstRepo, srcRepo)
	gitExec(t, srcRepo, "branch", "dev")

	res, err := f.sync(t, srcRepo, dstRepo, "dev", false)
	ck.Require().NoError(err, "the destination has no dev and must be made one")
	ck.False(res.UpToDate, "a branch the destination lacks is not up to date")
	ck.Eq("", res.OldOID, "dev did not exist on the destination")
	ck.Eq(tip, res.NewOID, "dev's tip")
	ck.Eq(tip, revParse(t, dstRepo, "refs/heads/dev"), "dev must exist on the destination at the shared commit")
}

// The destination's requested branch is at an older commit while ANOTHER
// destination head holds the source tip: the branch must fast-forward, not be
// reported up to date.
func TestRepoSyncFastForwardsBranchWhileAnotherHeadHoldsTheTip(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newRepoSyncFixture(t)

	srcRepo := filepath.Join(f.src.root, "repo")
	older := initRepo(t, srcRepo, "main", "one.txt")
	tip := commitFiles(t, srcRepo, "two.txt")

	dstRepo := filepath.Join(f.dst.root, "repo")
	gitExec(t, f.dst.root, "clone", "-q", srcRepo, dstRepo)
	detach(t, dstRepo)
	gitExec(t, dstRepo, "update-ref", "refs/heads/main", older)
	gitExec(t, dstRepo, "branch", "other", tip)

	res, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ck.Require().NoError(err, "main must fast-forward")
	ck.False(res.UpToDate, "main is behind, so the relay is not up to date")
	ck.Eq(older, res.OldOID, "main's old tip")
	ck.Eq(tip, res.NewOID, "main's new tip")
	ck.Eq(tip, revParse(t, dstRepo, "refs/heads/main"), "main must be fast-forwarded to the tip")
}

// --- same-executor refusal --------------------------------------------------

func TestRepoSyncRefusesSameExecutor(t *testing.T) {
	ck := assert.NewCollecting(t)
	client, root := newTreeSyncExecutor(t)
	obs := &observingClient{ExecutorServiceClient: &repoScratchClient{t: t, ExecutorServiceClient: client, tmp: t.TempDir()}}
	pool := newTreeSyncPool([]execpool.LiveExecutor{
		treeSyncExecutor("exec-both", "u1", map[string]string{"machine": "both"}, "", true),
	}, map[string]executorpbconnect.ExecutorServiceClient{"exec-both": obs})
	_, p := newPathSyncFixture(t, pool)

	repo := filepath.Join(root, "repo")
	initRepo(t, repo, "main", "one.txt")

	_, err := p.SyncRepo(t.Context(), users.Identity{UserID: "u1"}, "", protocol.SyncRepoRequest{
		Src:    protocol.SyncEndpoint{Executor: "both", Path: repo},
		Dst:    protocol.SyncEndpoint{Executor: "both", Path: repo},
		Branch: "main",
	})
	ce := controllerErr(t, err)
	ck.Eq(protocol.ErrInvalidArgs, ce.Code, "code")
	ck.Eq("source and destination are the same executor", ce.Message, "message")

	reads, writes := obs.counts()
	ck.Eq(0, reads, "the refusal must precede every RPC")
	ck.Eq(0, writes, "the refusal must precede every RPC")
}

// --- source/destination-controlled values -----------------------------------

func TestRepoSyncRefusesUnusableSourceBundlePath(t *testing.T) {
	ck := assert.NewCollecting(t)
	tip := strings.Repeat("a", 40)
	for _, hostile := range []string{"", ".", "..", "/", "a/..", "a\x00b"} {
		t.Run(fmt.Sprintf("%q", hostile), func(t *testing.T) {
			f := newRepoSyncFixtureWith(t, func(c executorpbconnect.ExecutorServiceClient) executorpbconnect.ExecutorServiceClient {
				return &repoScriptedBundleSource{ExecutorServiceClient: c, reply: &executorpb.GitBundleResponse{
					BundlePath: hostile,
					TipOid:     tip,
				}}
			}, nil)
			initRepo(t, filepath.Join(f.src.root, "repo"), "main", "one.txt")

			_, err := f.sync(t, filepath.Join(f.src.root, "repo"), filepath.Join(f.dst.root, "repo"), "main", false)
			ce := controllerErr(t, err)
			ck.Eq(protocol.ErrInvalidArgs, ce.Code, "bundle_path %q code", hostile)
			ck.Eq("source returned an unusable bundle path", ce.Message, "bundle_path %q message", hostile)
		})
	}
}

func TestRepoSyncRefusesUnusableSourceTip(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, bad := range []string{"", "nothex", strings.Repeat("a", 39), strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		t.Run(bad, func(t *testing.T) {
			f := newRepoSyncFixtureWith(t, func(c executorpbconnect.ExecutorServiceClient) executorpbconnect.ExecutorServiceClient {
				return &repoScriptedBundleSource{ExecutorServiceClient: c, reply: &executorpb.GitBundleResponse{
					BundlePath: "ok.bundle",
					TipOid:     bad,
				}}
			}, nil)
			initRepo(t, filepath.Join(f.src.root, "repo"), "main", "one.txt")

			_, err := f.sync(t, filepath.Join(f.src.root, "repo"), filepath.Join(f.dst.root, "repo"), "main", false)
			ce := controllerErr(t, err)
			ck.Eq(protocol.ErrInvalidArgs, ce.Code, "tip %q code", bad)
			ck.Eq(fmt.Sprintf("src bundle: source returned an unusable tip oid %q", bad), ce.Message, "tip %q message", bad)
		})
	}
}

// A destination head with a malformed oid is dropped, not forwarded: the real
// executor would refuse the whole GitBundle request otherwise.
func TestRepoSyncExcludesMalformedDestinationHeads(t *testing.T) {
	ck := assert.NewCollecting(t)
	rec := &repoRecordingSource{}
	f := newRepoSyncFixtureWith(t,
		func(c executorpbconnect.ExecutorServiceClient) executorpbconnect.ExecutorServiceClient {
			rec.ExecutorServiceClient = c
			return rec
		},
		func(c executorpbconnect.ExecutorServiceClient) executorpbconnect.ExecutorServiceClient {
			return &repoScriptedRefsDestination{ExecutorServiceClient: c, rewrite: func(r *executorpb.GitRefsResponse) {
				r.Heads = append(r.Heads, &executorpb.GitRef{Name: "bogus", Oid: "not-a-valid-oid"})
			}}
		})

	dstRepo := filepath.Join(f.dst.root, "repo")
	tip := initRepo(t, dstRepo, "main", "one.txt")
	srcRepo := filepath.Join(f.src.root, "repo")
	gitExec(t, f.src.root, "clone", "-q", dstRepo, srcRepo)

	res, err := f.sync(t, srcRepo, dstRepo, "main", false)
	ck.Require().NoError(err, "the malformed destination head must not fail the sync")
	ck.True(res.UpToDate, "main is already at the tip")
	ck.EqDeep([]string{tip}, rec.lastExcludes(), "only the valid destination head may be forwarded")
}

// More destination heads than the cap gets an empty exclude list — a full
// bundle, never an unbounded GitBundle request; exactly the cap still forwards.
func TestRepoSyncCapsDestinationExcludes(t *testing.T) {
	ck := assert.NewCollecting(t)
	fabricated := func(n int) []*executorpb.GitRef {
		heads := make([]*executorpb.GitRef, 0, n)
		for i := 0; i < n; i++ {
			heads = append(heads, &executorpb.GitRef{Name: fmt.Sprintf("b%d", i), Oid: fmt.Sprintf("%040x", i+1)})
		}
		return heads
	}
	for _, tc := range []struct {
		heads int
		want  int
	}{
		{heads: 256, want: 256},
		{heads: 257, want: 0},
	} {
		t.Run(fmt.Sprintf("%d-heads", tc.heads), func(t *testing.T) {
			rec := &repoRecordingSource{}
			f := newRepoSyncFixtureWith(t,
				func(c executorpbconnect.ExecutorServiceClient) executorpbconnect.ExecutorServiceClient {
					rec.ExecutorServiceClient = c
					return rec
				},
				func(c executorpbconnect.ExecutorServiceClient) executorpbconnect.ExecutorServiceClient {
					return &repoScriptedRefsDestination{ExecutorServiceClient: c, rewrite: func(r *executorpb.GitRefsResponse) {
						r.Heads = fabricated(tc.heads)
					}}
				})

			dstRepo := filepath.Join(f.dst.root, "repo")
			initRepo(t, dstRepo, "main", "one.txt")
			detach(t, dstRepo)
			srcRepo := filepath.Join(f.src.root, "repo")
			gitExec(t, f.src.root, "clone", "-q", dstRepo, srcRepo)

			_, err := f.sync(t, srcRepo, dstRepo, "main", false)
			ck.Require().NoError(err, "the sync must succeed with %d destination heads", tc.heads)
			ck.Eq(tc.want, len(rec.lastExcludes()), "forwarded exclude count for %d heads", tc.heads)
		})
	}
}
