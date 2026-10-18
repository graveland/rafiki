// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/repos"
)

// The layout under reposRoot()/<name>/, shared with every reader through
// pkg/repos (repos.CacheDir, repos.OpenRepoCheckout):
//
//	git/             bare repo; every fetch lands here
//	commits/<sha>/   detached worktree per commit, its own .venv inside
//	current          symlink -> commits/<sha> (relative)
//
// One checkout per commit is what lets a running job keep its commit while a
// refresh moves `current` on: a job resolves `current` to its real path when
// it starts and holds that directory's flock, so the sweep below can never
// remove it out from under a live run.
func reposRoot() string {
	return repos.CacheDir()
}

// repoDir is one registered source's own cache directory -- the parent of its
// bare repo, its per-commit checkouts and the `current` symlink.
func repoDir(name string) string {
	return filepath.Join(reposRoot(), name)
}

// repoGitDir is the source's bare repo: the object store every commit
// checkout is a worktree of, and where every fetch lands.
func repoGitDir(name string) string {
	return filepath.Join(repoDir(name), "git")
}

// repoCommitsDir holds one detached worktree per checked-out commit.
func repoCommitsDir(name string) string {
	return filepath.Join(repoDir(name), "commits")
}

// repoCommitDir is one commit's checkout: its own working tree and its own
// .venv, so a job running out of it keeps both for its whole life.
func repoCommitDir(name, commit string) string {
	return filepath.Join(repoCommitsDir(name), commit)
}

// repoCurrentLink is the symlink naming the commit a NEW job starts on.
func repoCurrentLink(name string) string {
	return filepath.Join(repoDir(name), "current")
}

// currentCommit is the commit `current` points at, or "" when nothing has
// been checked out yet.
func currentCommit(name string) string {
	dir, err := filepath.EvalSymlinks(repoCurrentLink(name))
	if err != nil {
		return ""
	}
	return filepath.Base(dir)
}

// repoLocks serialises refreshes and ref resolutions of the SAME source on
// this executor: two concurrent SyncRepos of one name would race on the bare
// repo's FETCH_HEAD and on the worktree they both want to create.
//
// A package-level registry rather than a Server field: the lock guards the
// on-disk cache, which every Server a process builds shares.
var (
	repoLocksMu sync.Mutex
	repoLocks   = map[string]*sync.Mutex{}
)

// lockRepo takes the per-source lock and returns its release.
func lockRepo(name string) func() {
	repoLocksMu.Lock()
	mu, ok := repoLocks[name]
	if !ok {
		mu = &sync.Mutex{}
		repoLocks[name] = mu
	}
	repoLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// commitSHARe is what a wire-supplied commit must look like before it becomes
// a path segment under commits/. Anchored and lowercase-only, matching what
// git prints: anything else is refused rather than cleaned.
var commitSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ensureGitReposRoot establishes the repo cache root exactly the way
// SyncPyModules establishes pymoduleCacheDir(): only ever write into a
// directory rafiki marked, or one that does not exist yet. Checked BEFORE
// MkdirAll: creating the directory first would make the absent case
// unrecognisable -- an empty dir is not a managed one. A root that exists
// without the marker (or is not a directory at all) is refused, never
// adopted: a fetch, a worktree add and a sweep would otherwise run inside
// whatever an operator or another program left there.
func ensureGitReposRoot(root string) error {
	if err := assertManagedOrAbsent(root); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, managedMarker)); !os.IsNotExist(err) {
		return nil // exists and is already marked ours
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, managedMarker), []byte("repos\n"), 0o644); err != nil {
		return fmt.Errorf("mark repos dir: %w", err)
	}
	return nil
}

// requirementsOnlyVenvError is the venvError for a checkout whose only
// dependency manifest is a bare requirements.txt: `uv sync` cannot consume
// one, so the build is not attempted and venv_ready stays false rather than
// promising a venv that was never built.
const requirementsOnlyVenvError = "repo carries only requirements.txt; rafiki builds repo venvs with `uv sync`, which needs a pyproject.toml — add one that declares the dependency"

// ResolveRepoRef resolves ref to a commit SHA on this executor, fetching the
// source into its bare repo first. The daemon asks ONE executor and fans the
// SHA out, so no two executors can disagree about which commit a refresh
// means; git itself only ever runs here, with the executor's own credentials.
//
// A git failure is REPORTED, not raised: the response's error field carries
// git's own output and the commit stays empty. The daemon needs to tell "this
// ref does not exist" from "this executor is unreachable", and only an RPC
// error means the latter.
func (s *Server) ResolveRepoRef(
	_ context.Context,
	req *connect.Request[executorpb.ResolveRepoRefRequest],
) (*connect.Response[executorpb.ResolveRepoRefResponse], error) {
	if !s.opts.RepoSync {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("this executor does not accept pymodule repo syncs"))
	}
	if err := validSegment(req.Msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("source name: %w", err))
	}
	// Same two gates SyncRepo applies, for the same reason: a leading dash
	// makes git parse the value as an option, and `git fetch
	// --upload-pack=<cmd>` executes <cmd> locally.
	if err := refuseLeadingDash("url", req.Msg.GetUrl()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := refuseLeadingDash("ref", req.Msg.GetRef()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	if err := ensureGitReposRoot(reposRoot()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	unlock := lockRepo(req.Msg.GetName())
	defer unlock()

	commit, out, err := resolveRepoRef(req.Msg.GetName(), req.Msg.GetUrl(), req.Msg.GetRef())
	if err != nil {
		return connect.NewResponse(&executorpb.ResolveRepoRefResponse{
			Error: uvError(out, err), // same 4096-byte cap as a failed uv build
		}), nil
	}
	return connect.NewResponse(&executorpb.ResolveRepoRefResponse{Commit: commit}), nil
}

// resolveRepoRef ensures the bare repo exists, fetches url at ref into it and
// reads back the commit FETCH_HEAD names. It returns git's combined output
// alongside any error so the caller can report the failure verbatim.
//
// The fetch names the URL explicitly instead of a configured remote: a source
// whose url was repointed must land the NEW url's commit, and a stale
// `origin` would silently keep serving the old one.
func resolveRepoRef(name, url, ref string) (string, []byte, error) {
	gitDir := repoGitDir(name)
	if out, err := ensureBareRepo(gitDir); err != nil {
		return "", out, err
	}
	if out, err := fetchRepoRef(gitDir, url, ref); err != nil {
		return "", out, err
	}
	out, err := gitOutput(gitDir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", out, fmt.Errorf("git rev-parse FETCH_HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil, nil
}

// ensureBareRepo creates the source's bare repo if it does not exist yet.
func ensureBareRepo(gitDir string) ([]byte, error) {
	if _, err := os.Stat(gitDir); err == nil {
		return nil, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", gitDir, err)
	}
	// git init takes the repository as its own argument, so the subprocess's
	// working directory is its PARENT; create that first or exec fails
	// before git ever runs.
	parent := filepath.Dir(gitDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", parent, err)
	}
	out, err := gitOutput(parent, "init", "--bare", gitDir)
	if err != nil {
		return out, fmt.Errorf("git init --bare: %w", err)
	}
	return nil, nil
}

// fetchRepoRef fetches url at ref into gitDir, by URL rather than through a
// configured remote so a repointed source is honoured.
func fetchRepoRef(gitDir, url, ref string) ([]byte, error) {
	out, err := gitOutput(gitDir, "fetch", "--no-tags", url, ref)
	if err != nil {
		return out, fmt.Errorf("git fetch %s %s: %w", url, ref, err)
	}
	return nil, nil
}

// SyncRepo refreshes ONE named repo in this executor's rafiki-managed repo
// cache onto the commit the daemon resolved: fetch the source at ref, check
// the commit out as its own detached worktree, build that checkout's one
// shared venv, discover the scripts, packages and skills it contains, and
// only then move `current` onto it.
//
// `current` moving LAST is the whole contract: a job that resolved `current`
// while the refresh was in flight stays on the old commit and keeps it, and a
// job that resolves it afterwards gets the new one. Nothing is mutated in
// place.
//
// A failed checkout does not error the RPC: it returns a response with
// checkout_error set and nothing changed on this executor. A git checkout
// left in an unknown state is not a state to discover or build against, but
// the daemon still wants the failure REPORTED rather than mistaking it for an
// unreachable executor.
func (s *Server) SyncRepo(
	_ context.Context,
	req *connect.Request[executorpb.SyncRepoRequest],
) (*connect.Response[executorpb.SyncRepoResponse], error) {
	if !s.opts.RepoSync {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("this executor does not accept pymodule repo syncs"))
	}

	// The name becomes a path segment under the cache root: guard it before
	// touching disk, the same way SyncPyModules guards its module names.
	name := req.Msg.GetName()
	if err := validSegment(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("source name: %w", err))
	}
	// A leading dash makes git parse the argument as an option instead of a
	// value: `git fetch origin --upload-pack=<cmd>` executes <cmd> locally on
	// this executor. Both values arrive from the daemon's store, but the
	// executor re-checks at its own boundary, before any subprocess runs --
	// the same shape validSegment applies to the name. Guarded on BOTH url
	// and ref: the fetch path is not exploitable in this shape today, but
	// the guard is cheaper than trusting that accident to hold.
	if err := refuseLeadingDash("url", req.Msg.GetUrl()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := refuseLeadingDash("ref", req.Msg.GetRef()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// The commit is resolved by ResolveRepoRef and becomes a path segment of
	// its own: refuse anything that is not the 40-hex SHA that contract names.
	commit := req.Msg.GetCommit()
	if !commitSHARe.MatchString(commit) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("commit %q must be the 40-character hex SHA ResolveRepoRef returned", commit))
	}

	// Establish the checkout root the same way the blob path establishes its
	// cache dir, BEFORE any fetch/worktree/sweep: refuse rather than adopt a
	// root this executor never created.
	if err := ensureGitReposRoot(reposRoot()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	unlock := lockRepo(name)
	defer unlock()

	resp, err := s.syncRepoCommit(name, req.Msg.GetUrl(), req.Msg.GetRef(), commit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

// syncRepoCommit is SyncRepo's body under the per-source lock.
func (s *Server) syncRepoCommit(name, url, ref, commit string) (*executorpb.SyncRepoResponse, error) {
	dir := repoCommitDir(name, commit)
	resp := &executorpb.SyncRepoResponse{}

	// Already current: no fetch, and no rebuild of a venv that is there. The
	// commit directory is the same tree it was, so only discovery and the
	// venv-completion check have anything to say.
	unchanged := currentCommit(name) == commit
	if unchanged {
		resp.VenvReady, resp.VenvError = repoVenvEnsure(dir)
	} else {
		if checkoutErr := checkoutCommit(name, url, ref, commit); checkoutErr != "" {
			// Nothing changed: report the checkout that is still current.
			resp.Commit = currentCommit(name)
			resp.CheckoutError = checkoutErr
			return resp, nil
		}
		venvErr := buildRepoVenv(dir)
		resp.VenvReady = venvErr == ""
		resp.VenvError = venvErr
	}

	scripts, packages, err := repos.Discover(dir)
	if err != nil {
		return nil, err
	}
	for _, sc := range scripts {
		resp.Scripts = append(resp.Scripts,
			&executorpb.RepoScript{Name: sc.Name, Description: sc.Description})
	}
	for _, p := range packages {
		resp.Packages = append(resp.Packages,
			&executorpb.RepoPackage{Name: p.Name, Description: p.Description})
	}

	// Skills are discovered against this commit's checkout with its venv, so
	// a [tool.rafiki] skill-source installed as a wheel is read out of THAT
	// commit's site-packages. Per-skill errors ride the response; they never
	// fail the refresh, and they never fail the scripts (design §5).
	report := repos.DiscoverSkills(dir, name, repoVenvDir(dir))
	for _, sk := range report.Skills {
		resp.Skills = append(resp.Skills, repoSkillProto(sk))
	}
	resp.SkillErrors = report.Errors

	// Flip last: everything above could still fail, and a `current` pointing
	// at a half-built checkout would hand the next job a broken tree.
	if !unchanged {
		if err := flipCurrent(name, commit); err != nil {
			return nil, err
		}
	}
	sweepRepoCommits(name, commit)

	resp.Commit = commit
	return resp, nil
}

// checkoutCommit brings the bare repo up to date and makes sure commit has a
// checkout of its own, returning "" on success or the reported
// checkout_error text on failure. Nothing about `current` is touched here.
func checkoutCommit(name, url, ref, commit string) string {
	gitDir := repoGitDir(name)
	if out, err := ensureBareRepo(gitDir); err != nil {
		return uvError(out, err)
	}
	if out, err := fetchRepoRef(gitDir, url, ref); err != nil {
		return uvError(out, err)
	}
	// The daemon resolved this SHA through another executor. If this fetch
	// did not bring it, this executor cannot serve it -- say so rather than
	// creating an empty checkout that would later run against nothing.
	if _, err := gitOutput(gitDir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Sprintf("commit %s not found after fetching %s", commit, ref)
	}

	dir := repoCommitDir(name, commit)
	if _, err := os.Stat(dir); err == nil {
		return "" // already checked out by an earlier refresh
	} else if !os.IsNotExist(err) {
		return fmt.Sprintf("stat %s: %v", dir, err)
	}
	if err := os.MkdirAll(repoCommitsDir(name), 0o755); err != nil {
		return fmt.Sprintf("create commits dir: %v", err)
	}
	if out, err := gitOutput(gitDir, "worktree", "add", "--detach", dir, commit); err != nil {
		return uvError(out, err)
	}
	return ""
}

// flipCurrent atomically points <name>/current at commits/<commit>. The link
// is created under a temporary name and renamed over the old one, so a reader
// never observes a missing or half-written `current`.
func flipCurrent(name, commit string) error {
	link := filepath.Join(repoDir(name), ".current-"+randomToken())
	if err := os.Symlink(filepath.Join("commits", commit), link); err != nil {
		return fmt.Errorf("stage current link: %w", err)
	}
	if err := os.Rename(link, repoCurrentLink(name)); err != nil {
		os.Remove(link)
		return fmt.Errorf("publish current link: %w", err)
	}
	return nil
}

// randomToken is the unique suffix on a staged `current` link, so two
// refreshes can never collide on the temporary name.
func randomToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// repoVenvDir is the checkout's venv directory, or "" when it has no usable
// venv -- the value repos.DiscoverSkills takes to decide whether a
// wheel-installed skill-source can be read.
func repoVenvDir(dir string) string {
	if !repoVenvPythonPresent(dir) {
		return ""
	}
	return filepath.Join(dir, venvDirName)
}

// repoVenvPythonPresent reports whether the checkout has a USABLE venv: the
// interpreter exists, not merely the directory. A build that failed part way
// through leaves neither, but a directory without an interpreter is no venv.
func repoVenvPythonPresent(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, venvDirName, "bin", "python3"))
	return err == nil
}

// repoVenvEnsure reports the commit checkout's venv state, completing the
// build when the checkout has no usable interpreter.
//
// The unchanged-commit path deliberately does not re-run `uv sync` on a venv
// that is already there — that is the design's point, and a refresh of the
// same commit must not pay for a rebuild. "Already there" means the
// INTERPRETER exists. A build that failed once — a network blip, a transient
// resolve error, uv not installed at that moment — left no interpreter, and
// skipping the build forever would make that failure permanent: only a new
// commit would ever repair it, with nothing telling the user so. So a missing
// interpreter means the build never completed, and it is completed here.
//
// The manifest gate is buildRepoVenv's, unchanged: a repo declaring no
// dependencies invokes nothing and is ready; a requirements.txt-only repo
// reports the same one-sentence refusal the fresh path does.
func repoVenvEnsure(dir string) (ready bool, venvError string) {
	if repoVenvPythonPresent(dir) {
		return true, ""
	}
	venvErr := buildRepoVenv(dir)
	return venvErr == "", venvErr
}

// repoSkillProto renders one discovered skill as its wire message.
func repoSkillProto(sk repos.DiscoveredSkill) *executorpb.RepoSkill {
	out := &executorpb.RepoSkill{
		Namespace:   sk.Namespace,
		Name:        sk.Name,
		Description: sk.Description,
		Body:        sk.Body,
		From:        sk.From,
	}
	for _, f := range sk.Files {
		out.Files = append(out.Files, &executorpb.SkillFile{
			Path:    f.Path,
			Mode:    f.Mode,
			Content: f.Content,
		})
	}
	return out
}

// refuseLeadingDash refuses a wire-supplied value that git would parse as an
// option rather than the value it is meant to be: `git fetch origin
// --upload-pack=<cmd>` executes <cmd> locally on this executor. kind names the
// field for the error message and must be the word a caller uses for it
// ("url", "ref", "branch").
func refuseLeadingDash(kind, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q begins with a dash and would parse as a git option, not a %s", kind, value, kind)
	}
	return nil
}

// gitRunner runs one git argv array in dir under env and returns its combined
// output. It is a package variable so a test can observe or refuse an
// invocation without running git; production is a plain exec.Command. env is
// passed through verbatim -- nil means inherit this process's environment.
var gitRunner = func(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// gitOutput runs one git command in dir and returns its combined output. It is
// deliberately NOT path-guarded: every caller passes a path built from a
// validSegment-validated name, and git's own ref resolution handles the
// arguments.
func gitOutput(dir string, args ...string) ([]byte, error) {
	return gitRunner(dir, nil, args...)
}

// buildRepoVenv builds the checkout's ONE shared venv at dir/.venv by running
// `uv sync` against the checkout's own manifest -- rafiki parses no manifest
// itself, it only points uv at the checkout root and lets uv figure out its
// own input. It returns "" when the venv is ready to use, or the capped
// combined output of the failed build when not.
//
// The manifest gate is file existence only, never parsing, and it is a
// three-way check:
//
//   - pyproject.toml present: `uv sync` builds against it (the normal path).
//   - only requirements.txt present: not ready, one clear sentence -- uv sync
//     cannot consume a bare requirements file, and reporting venv_ready=true
//     would promise a venv that was never built, its dependencies missing at
//     run time. The discovered inventory still rides the same response, the
//     same rule as any failed build.
//   - neither file: no dependencies declared at all -- nothing to build, the
//     venv is reported ready with no uv invocation, exactly as the response's
//     venv_ready contract promises.
func buildRepoVenv(dir string) string {
	_, err := os.Stat(filepath.Join(dir, "pyproject.toml"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Sprintf("stat pyproject.toml: %v", err)
	}
	if os.IsNotExist(err) {
		_, rerr := os.Stat(filepath.Join(dir, "requirements.txt"))
		switch {
		case rerr == nil:
			return requirementsOnlyVenvError
		case os.IsNotExist(rerr):
			return ""
		default:
			return fmt.Sprintf("stat requirements.txt: %v", rerr)
		}
	}

	// Same uv-required posture as buildModuleVenv: no pip fallback, and the
	// same resolution (RAFIKI_PYMODULE_UV, else PATH) -- do not reimplement.
	uv, err := exec.LookPath(uvPath())
	if err != nil {
		return fmt.Sprintf("uv not found on this executor (required to install pymodule dependencies): %v", err)
	}

	// Stage in a sibling temp dir so a run never observes a half-built venv;
	// the deferred RemoveAll is a no-op once the rename below has moved the
	// tree out. Same idiom as buildModuleVenv.
	staged, err := os.MkdirTemp(dir, ".rafiki-venv-staging-*")
	if err != nil {
		return fmt.Sprintf("stage venv: %v", err)
	}
	defer os.RemoveAll(staged)
	stagedVenv := filepath.Join(staged, "venv")

	// UV_PROJECT_ENVIRONMENT is how a single `uv sync` is pointed at a venv
	// location of the caller's choosing; without it uv builds into the
	// checkout's own .venv and the swap below would have nothing to move.
	cmd := exec.Command(uv, "sync", "--python", pymodulePythonInterpreter())
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+stagedVenv)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A failed build discards the staging tree and leaves any previous
		// venv in place untouched, exactly like a failed blob-sourced rebuild.
		return uvError(out, err)
	}

	// Swap into place. NOT a single atomic syscall -- there is a brief window
	// where .venv does not exist, the same acknowledged non-atomicity
	// buildModuleVenv carries; only ever replace a path rafiki itself staged.
	venvDir := filepath.Join(dir, venvDirName)
	if err := os.RemoveAll(venvDir); err != nil {
		return fmt.Sprintf("remove old venv: %v", err)
	}
	if err := os.Rename(stagedVenv, venvDir); err != nil {
		return fmt.Sprintf("publish venv: %v", err)
	}
	return ""
}
