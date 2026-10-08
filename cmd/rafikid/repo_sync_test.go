// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// withTmpDir runs fn with TMPDIR pointing at dir, restoring the previous value
// (or its absence) afterwards.
func withTmpDir(dir string, fn func()) {
	prev, had := os.LookupEnv("TMPDIR")
	os.Setenv("TMPDIR", dir)
	defer func() {
		if had {
			os.Setenv("TMPDIR", prev)
		} else {
			os.Unsetenv("TMPDIR")
		}
	}()
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
	executorpbconnect.ExecutorServiceClient
	tmp string
}

func (c *repoScratchClient) GitRefs(ctx context.Context, req *connect.Request[executorpb.GitRefsRequest]) (*connect.Response[executorpb.GitRefsResponse], error) {
	var resp *connect.Response[executorpb.GitRefsResponse]
	var err error
	withTmpDir(c.tmp, func() { resp, err = c.ExecutorServiceClient.GitRefs(ctx, req) })
	return resp, err
}

func (c *repoScratchClient) GitBundle(ctx context.Context, req *connect.Request[executorpb.GitBundleRequest]) (*connect.Response[executorpb.GitBundleResponse], error) {
	var resp *connect.Response[executorpb.GitBundleResponse]
	var err error
	withTmpDir(c.tmp, func() { resp, err = c.ExecutorServiceClient.GitBundle(ctx, req) })
	return resp, err
}

func (c *repoScratchClient) GitFetchBundle(ctx context.Context, req *connect.Request[executorpb.GitFetchBundleRequest]) (*connect.Response[executorpb.GitFetchBundleResponse], error) {
	var resp *connect.Response[executorpb.GitFetchBundleResponse]
	var err error
	withTmpDir(c.tmp, func() { resp, err = c.ExecutorServiceClient.GitFetchBundle(ctx, req) })
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
	t.Helper()
	srcReal, srcRoot := newTreeSyncExecutor(t)
	dstReal, dstRoot := newTreeSyncExecutor(t)

	srcScratch := t.TempDir()
	dstScratch := t.TempDir()
	srcObs := &observingClient{ExecutorServiceClient: &repoScratchClient{ExecutorServiceClient: srcReal, tmp: srcScratch}}
	dstObs := &observingClient{ExecutorServiceClient: &repoScratchClient{ExecutorServiceClient: dstReal, tmp: dstScratch}}

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
