// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// gitHardening are the global git options every invocation in this file
// carries BEFORE its subcommand, as defense in depth against a hostile
// .git/config: the executor runs git inside repositories it did not create,
// and a repository's own configuration must not be able to run a hook, an
// fsmonitor program, or a remote transport helper.
//
// Each option is pinned by a test that fails when it is removed:
//
//   - core.fsmonitor=false -- TestGitFetchBundleIgnoresHostileRepoFsmonitor
//     (a fetch runs the repo's fsmonitor on its index refresh).
//   - core.hooksPath=/dev/null -- TestGitFetchBundleForcesHooksOffOnCheckout
//     (the one git call that runs a hook is the checkout of a freshly
//     initialized destination; the hostile hooksPath arrives through the
//     executor's git config, since a repo created by this call carries none).
//   - protocol.ext.allow=never -- TestGitFetchBundleRefusesExtInsteadOfRewrite
//     (a repo-local url.*.insteadOf can route the bundle path through ext::).
//   - core.sshCommand=false -- TestGitFetchBundleIgnoresHostileSSHCommand
//     (a repo-local url.*.insteadOf can route the bundle path through ssh://).
var gitHardening = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "protocol.ext.allow=never",
	"-c", "core.sshCommand=false",
}

// scratchDirName is the directory under the system temp dir that is the ONLY
// place a bundle file lives.
const scratchDirName = "rafiki-sync"

// scratchBundleMaxAge is how long an abandoned bundle may sit in the scratch
// directory before the next call sweeps it.
const scratchBundleMaxAge = time.Hour

// scratchDirEUID reports the effective uid the scratch directory must belong
// to. A package variable so a test can pin the ownership refusal without root.
var scratchDirEUID = os.Geteuid

// gitEnv returns the environment every git subprocess runs under: the
// executor's pinned startup env (nil meaning this process's environment) with
// the executor's own settings FORCED to the front of precedence. LC_ALL=C
// makes git's messages locale-independent, so matching "Refusing to create an
// empty bundle" cannot break under the pinned env's locale, and
// GIT_TERMINAL_PROMPT=0 means git can never block waiting for a credential on
// a terminal that does not exist. Any conflicting entry in the pinned env is
// dropped: the child's getenv returns the FIRST match, so appending alone
// would not override it.
func (s *Server) gitEnv(extra ...string) []string {
	base := s.opts.Env
	if base == nil {
		base = os.Environ()
	}
	forced := make([]string, 0, 2+len(extra))
	forced = append(forced, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	forced = append(forced, extra...)

	env := make([]string, 0, len(base)+len(forced))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "LC_ALL", "GIT_TERMINAL_PROMPT", "GIT_CEILING_DIRECTORIES":
			continue
		}
		env = append(env, kv)
	}
	return append(env, forced...)
}

// git runs one hardened git invocation in dir under the executor's pinned
// environment. It is the ONLY way this file invokes git: the argv array goes
// straight to gitRunner (gitpymodule_sync.go's runner seam), never a shell and
// never a string concatenated into one argument.
func (s *Server) git(dir string, args ...string) ([]byte, error) {
	return s.gitExtra(dir, nil, args...)
}

func (s *Server) gitExtra(dir string, extraEnv []string, args ...string) ([]byte, error) {
	full := make([]string, 0, len(gitHardening)+len(args))
	full = append(full, gitHardening...)
	full = append(full, args...)
	return gitRunner(dir, s.gitEnv(extraEnv...), full...)
}

// gitRepo runs one hardened git invocation with its working directory at repo
// and repository discovery fenced at repo's parent, so a SUBDIRECTORY of a
// repository can never be mistaken for one (GIT_CEILING_DIRECTORIES stops the
// search before the parent itself is examined).
func (s *Server) gitRepo(repo string, args ...string) ([]byte, error) {
	return s.gitExtra(repo, []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(repo)}, args...)
}

// gitAtRepo is gitRepo for a call that names repo with -C rather than a
// working directory. The fence is the same.
func (s *Server) gitAtRepo(repo string, args ...string) ([]byte, error) {
	full := make([]string, 0, len(args)+2)
	full = append(full, "-C", repo)
	full = append(full, args...)
	return s.gitExtra("", []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(repo)}, full...)
}

// validRepoPath refuses a repo that is not an absolute, clean path, or whose
// final component is a symlink. The path is a wire value that becomes a git
// working directory; a relative or unclean one would make git resolve it
// against this process's cwd, and a symlink would let it point anywhere.
// An absent path is allowed -- GitFetchBundle creates it.
func validRepoPath(repo string) error {
	if repo == "" || !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return fmt.Errorf("repo %q must be an absolute, clean path", repo)
	}
	if fi, err := os.Lstat(repo); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("repo %q must not be a symlink", repo)
	}
	return nil
}

// validBranch refuses a branch name before any subprocess runs: empty, a
// leading dash (git would parse it as an option, not a value), the literal
// HEAD, anything containing the @{ reflog syntax, or anything
// `git check-ref-format --branch` rejects.
func (s *Server) validBranch(name string) error {
	if name == "" {
		return fmt.Errorf("branch name is empty")
	}
	if err := refuseLeadingDash("branch", name); err != nil {
		return err
	}
	if name == "HEAD" {
		return fmt.Errorf("branch %q names the current checkout, not a branch to sync", name)
	}
	if strings.Contains(name, "@{") {
		return fmt.Errorf("branch %q contains the reflog syntax @{ and is refused", name)
	}
	if out, err := s.git("", "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("branch %q is not a valid ref name: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// validOID refuses anything that is not a full lowercase-hex object id: git
// would otherwise read a leading dash as an option, and an abbreviated or
// uppercase id is not what the wire promises.
func validOID(oid string) error {
	if len(oid) != 40 && len(oid) != 64 {
		return fmt.Errorf("object id %q must be 40 or 64 lowercase hex characters", oid)
	}
	for i := 0; i < len(oid); i++ {
		c := oid[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("object id %q must be 40 or 64 lowercase hex characters", oid)
		}
	}
	return nil
}

// validBundlePath refuses a bundle_path that is not an absolute, clean regular
// file whose parent resolves to the scratch directory itself. Cleanliness is
// checked first, so <scratch>/link/../victim (where link is a symlink inside
// scratch) is refused rather than resolved into a file outside scratch. The
// parent is EvalSymlinks'd so a symlinked component cannot redirect the file,
// and Lstat (not Stat) refuses a symlink at the final component. Only after
// every check passes may the caller register the bundle for removal.
func validBundlePath(p, scratch string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("bundle_path %q must be an absolute, clean path", p)
	}
	resolvedScratch, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		return fmt.Errorf("resolve scratch dir %s: %w", scratch, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil || resolvedDir != resolvedScratch {
		return fmt.Errorf("bundle_path %q must be directly inside %s", p, scratch)
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("bundle_path %q must be a regular file", p)
	}
	return nil
}

// scratchDir returns the absolute directory bundle files live in, creating it
// 0700 if needed, and sweeps stale bundles on every call. It refuses to adopt
// anything it did not create: Mkdir (not MkdirAll) makes a pre-created path
// visible, Lstat refuses a symlink or a non-directory, and the directory must
// be owned by this executor's effective uid.
func scratchDir() (string, error) {
	dir := filepath.Join(os.TempDir(), scratchDirName)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create scratch dir %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("stat scratch dir %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("scratch path %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if uid := scratchDirEUID(); int(st.Uid) != uid {
			return "", fmt.Errorf("scratch dir %s is owned by uid %d, not this executor's uid %d", dir, st.Uid, uid)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("chmod scratch dir %s: %w", dir, err)
	}
	sweepStaleBundles(dir)
	return dir, nil
}

// sweepStaleBundles removes regular *.bundle and *.bundle.lock files older
// than scratchBundleMaxAge. Directories and other names are left alone, so a
// caller's own bookkeeping in the scratch dir survives. Best effort: a failed
// sweep must not fail the RPC that triggered it.
func sweepStaleBundles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-scratchBundleMaxAge)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".bundle") && !strings.HasSuffix(name, ".bundle.lock") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// randomHex returns n random bytes as 2n lowercase hex characters, for a file
// name that cannot collide with or be predicted by a caller.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// resolveCommit resolves rev to the object id of the commit it names, or
// returns an error carrying git's own output.
func (s *Server) resolveCommit(repo, rev string) (string, error) {
	out, err := s.gitRepo(repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %s", rev, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// repoGitDir returns repo's resolved absolute git directory, requiring the
// repository to be repo ITSELF: discovery is fenced at repo's parent and the
// reported git dir must equal <repo>/.git (or, for a bare layout, <repo>). A
// subdirectory of a repository is therefore not a repository. ok is false when
// repo is not one.
func (s *Server) repoGitDir(repo string) (string, bool) {
	out, err := s.gitRepo(repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", false
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return "", false
	}
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", false
	}
	if got == filepath.Join(resolvedRepo, ".git") || got == resolvedRepo {
		return got, true
	}
	return "", false
}

// GitRefs reports whether a repo exists on this executor and, if so, its
// branches plus the scratch directory bundle files may be written to.
func (s *Server) GitRefs(
	_ context.Context,
	req *connect.Request[executorpb.GitRefsRequest],
) (*connect.Response[executorpb.GitRefsResponse], error) {
	repo := req.Msg.GetRepo()
	if err := validRepoPath(repo); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	scratch, err := scratchDir()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &executorpb.GitRefsResponse{ScratchDir: scratch}

	// Absent, or present but not a directory (Lstat, so a symlink is not
	// followed): not a repository. The caller gets exists=false rather than an
	// error -- "there is nothing there" is a normal answer for a destination
	// that is about to be seeded.
	fi, err := os.Lstat(repo)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return connect.NewResponse(resp), nil
	}
	if _, ok := s.repoGitDir(repo); !ok {
		return connect.NewResponse(resp), nil
	}

	out, err := s.gitRepo(repo, "for-each-ref", "--format=%(refname:short)%09%(objectname)", "--", "refs/heads")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("git for-each-ref: %s", strings.TrimSpace(string(out))))
	}
	resp.Exists = true
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		name, oid, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		resp.Heads = append(resp.Heads, &executorpb.GitRef{Name: name, Oid: oid})
	}
	return connect.NewResponse(resp), nil
}

// GitBundle writes one branch to a bundle file in this executor's scratch
// directory, skipping the objects the caller already has.
func (s *Server) GitBundle(
	_ context.Context,
	req *connect.Request[executorpb.GitBundleRequest],
) (*connect.Response[executorpb.GitBundleResponse], error) {
	repo := req.Msg.GetRepo()
	if err := validRepoPath(repo); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	branch := req.Msg.GetBranch()
	if err := s.validBranch(branch); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	exclude := req.Msg.GetExcludeOids()
	for _, oid := range exclude {
		if err := validOID(oid); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	if _, ok := s.repoGitDir(repo); !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("repo %q is not a git repository", repo))
	}

	tip, err := s.resolveCommit(repo, "refs/heads/"+branch)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("branch %q: %w", branch, err))
	}
	for _, oid := range exclude {
		if oid == tip {
			return connect.NewResponse(&executorpb.GitBundleResponse{
				UpToDate: true,
				TipOid:   tip,
			}), nil
		}
	}

	// Drop prerequisites this repo does not have: git would refuse to create a
	// bundle that names a commit it cannot resolve, and a caller may offer the
	// tips of a destination that is ahead of or unrelated to this source.
	prereqs := make([]string, 0, len(exclude))
	for _, oid := range exclude {
		if _, err := s.gitRepo(repo, "cat-file", "-e", "--", oid+"^{commit}"); err == nil {
			prereqs = append(prereqs, oid)
		}
	}

	scratch, err := scratchDir()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	name, err := randomHex(8)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("bundle name: %w", err))
	}
	bundlePath := filepath.Join(scratch, name+".bundle")

	args := make([]string, 0, 5+len(prereqs))
	args = append(args, "bundle", "create", "--", bundlePath, "refs/heads/"+branch)
	for _, oid := range prereqs {
		args = append(args, "^"+oid)
	}
	out, err := s.gitRepo(repo, args...)
	if err != nil {
		// Every object the branch needs is already on the far side: there is
		// nothing to send, which is the same answer as an explicit tip match.
		if strings.Contains(string(out), "Refusing to create empty bundle") {
			return connect.NewResponse(&executorpb.GitBundleResponse{
				UpToDate: true,
				TipOid:   tip,
			}), nil
		}
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("git bundle create: %s", strings.TrimSpace(string(out))))
	}
	fi, err := os.Stat(bundlePath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stat bundle: %w", err))
	}
	return connect.NewResponse(&executorpb.GitBundleResponse{
		BundlePath: bundlePath,
		Bytes:      fi.Size(),
		TipOid:     tip,
	}), nil
}

// destKind classifies what is at a destination repo path.
type destKind int

const (
	destAbsent destKind = iota
	destEmpty
	destRepo
	destNotRepo
)

// inspectDest reports what is at repo WITHOUT creating or modifying anything.
// gitDir is the resolved absolute git directory when repo is a repository.
func (s *Server) inspectDest(repo string) (destKind, string, error) {
	fi, err := os.Lstat(repo)
	switch {
	case err != nil && !os.IsNotExist(err):
		return destAbsent, "", connect.NewError(connect.CodeInternal, fmt.Errorf("stat repo: %w", err))
	case os.IsNotExist(err):
		return destAbsent, "", nil
	case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
		return destNotRepo, "", nil
	}
	empty, err := dirIsEmpty(repo)
	if err != nil {
		return destAbsent, "", connect.NewError(connect.CodeInternal, fmt.Errorf("read repo dir: %w", err))
	}
	if empty {
		return destEmpty, "", nil
	}
	gitDir, ok := s.repoGitDir(repo)
	if !ok {
		return destNotRepo, "", nil
	}
	return destRepo, gitDir, nil
}

// verifyBundle runs `git bundle verify` against a throwaway BARE repository
// inside the scratch directory, removed when done. Verifying this way means a
// bundle is checked before the destination is created or touched; when the
// destination already has an object store (altObjects non-empty) it is
// attached as an alternate, so a bundle whose prerequisites the destination
// already holds still verifies.
func (s *Server) verifyBundle(scratch, bundlePath, altObjects string) error {
	name, err := randomHex(8)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("verify dir name: %w", err))
	}
	dir := filepath.Join(scratch, ".verify-"+name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("create verify dir: %w", err))
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if out, err := s.git("", "init", "--bare", "-q", "--", dir); err != nil {
		return connect.NewError(connect.CodeInternal,
			fmt.Errorf("git init --bare: %s", strings.TrimSpace(string(out))))
	}
	if altObjects != "" {
		altDir := filepath.Join(dir, "objects", "info")
		if err := os.MkdirAll(altDir, 0o700); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("create verify alternates dir: %w", err))
		}
		if err := os.WriteFile(filepath.Join(altDir, "alternates"), []byte(altObjects+"\n"), 0o600); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("write verify alternates: %w", err))
		}
	}
	if out, err := s.git("", "-C", dir, "bundle", "verify", "--", bundlePath); err != nil {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("git bundle verify: %s", strings.TrimSpace(string(out))))
	}
	return nil
}

// GitFetchBundle fetches a bundle file into a repo on this executor, refusing
// a checked-out branch and a non-fast-forward update unless force.
func (s *Server) GitFetchBundle(
	_ context.Context,
	req *connect.Request[executorpb.GitFetchBundleRequest],
) (*connect.Response[executorpb.GitFetchBundleResponse], error) {
	repo := req.Msg.GetRepo()
	if err := validRepoPath(repo); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	branch := req.Msg.GetBranch()
	if err := s.validBranch(branch); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	scratch, err := scratchDir()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	bundlePath := req.Msg.GetBundlePath()
	if err := validBundlePath(bundlePath, scratch); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// The bundle is consumed exactly once, whatever the outcome. Registered
	// only AFTER every path check has passed, so a refused path is never the
	// file this call removes.
	defer func() { _ = os.Remove(bundlePath) }()

	// Nothing is created or modified before this point. A non-empty directory
	// that is not a repository is refused, never initialized over.
	kind, gitDir, err := s.inspectDest(repo)
	if err != nil {
		return nil, err
	}
	if kind == destNotRepo {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("repo %q exists but is not a git repository", repo))
	}

	// Verify BEFORE creating anything: a corrupt bundle must not leave a
	// half-created destination behind.
	altObjects := ""
	if kind == destRepo {
		altObjects = filepath.Join(gitDir, "objects")
	}
	if err := s.verifyBundle(scratch, bundlePath, altObjects); err != nil {
		return nil, err
	}

	createdDir := kind == destAbsent
	createdRepo := false
	cleanup := func() {
		if createdDir {
			_ = os.RemoveAll(repo)
		} else if createdRepo {
			_ = os.RemoveAll(filepath.Join(repo, ".git"))
		}
	}
	if kind != destRepo {
		if createdDir {
			if err := os.MkdirAll(repo, 0o755); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo dir: %w", err))
			}
		}
		if out, err := s.git("", "init", "-b", branch, "--", repo); err != nil {
			cleanup()
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("git init: %s", strings.TrimSpace(string(out))))
		}
		createdRepo = true
	}

	if createdRepo {
		refspec := "refs/heads/" + branch + ":refs/heads/" + branch
		out, err := s.gitAtRepo(repo, "-c", "transfer.fsckObjects=true",
			"fetch", "--no-tags", "--update-head-ok", "--", bundlePath, refspec)
		if err != nil {
			cleanup()
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out))))
		}
		// The freshly initialized repo has an unborn branch, so the fetch above
		// updated it with --update-head-ok but left the working tree empty.
		if out, err := s.gitAtRepo(repo, "checkout", "-f", branch); err != nil {
			cleanup()
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("git checkout: %s", strings.TrimSpace(string(out))))
		}
		newOID, err := s.resolveCommit(repo, "refs/heads/"+branch)
		if err != nil {
			cleanup()
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return connect.NewResponse(&executorpb.GitFetchBundleResponse{
			NewOid:      newOID,
			CreatedRepo: true,
		}), nil
	}

	oldOID, _ := s.resolveCommit(repo, "refs/heads/"+branch) // empty when absent
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	if req.Msg.GetForce() {
		refspec = "+" + refspec
	}
	out, err := s.gitAtRepo(repo, "-c", "transfer.fsckObjects=true",
		"fetch", "--no-tags", "--", bundlePath, refspec)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out))))
	}
	newOID, err := s.resolveCommit(repo, "refs/heads/"+branch)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&executorpb.GitFetchBundleResponse{
		OldOid: oldOID,
		NewOid: newOID,
	}), nil
}

// dirIsEmpty reports whether dir holds no entries.
func dirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}
