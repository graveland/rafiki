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
	"time"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// gitHardening are the global git options every invocation in this file
// carries BEFORE its subcommand, as defense in depth against a hostile
// .git/config: the executor runs git inside repositories it did not create,
// and a repository's own configuration must not be able to run a hook, an
// fsmonitor program, or an external transport helper. core.sshCommand=false
// keeps a config-planted ssh command from executing even if a remote transport
// were ever reached.
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

// gitEnv is the environment every git subprocess runs under: the executor's
// pinned startup env plus GIT_TERMINAL_PROMPT=0, so git can never block
// waiting for a credential on a terminal that does not exist. A nil
// Options.Env means this process's environment, matching that field's
// documented meaning.
func (s *Server) gitEnv() []string {
	base := s.opts.Env
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base)+1)
	env = append(env, base...)
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// git runs one hardened git invocation in dir under the executor's pinned
// environment. It is the ONLY way this file invokes git: the argv array goes
// straight to gitRunner (gitpymodule_sync.go's runner seam), never a shell and
// never a string concatenated into one argument.
func (s *Server) git(dir string, args ...string) ([]byte, error) {
	full := make([]string, 0, len(gitHardening)+len(args))
	full = append(full, gitHardening...)
	full = append(full, args...)
	return gitRunner(dir, s.gitEnv(), full...)
}

// validRepoPath refuses a repo that is not an absolute, clean path. The path
// is a wire value that becomes a git working directory; a relative or unclean
// one would make git resolve it against this process's cwd.
func validRepoPath(repo string) error {
	if repo == "" || !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return fmt.Errorf("repo %q must be an absolute, clean path", repo)
	}
	return nil
}

// validBranch refuses a branch name before any subprocess runs: empty, a
// leading dash (git would parse it as an option, not a value), the literal
// HEAD, or anything `git check-ref-format --branch` rejects.
func (s *Server) validBranch(name string) error {
	if name == "" {
		return fmt.Errorf("branch name is empty")
	}
	if err := refuseLeadingDash("branch", name); err != nil {
		return err
	}
	if name == "HEAD" {
		return fmt.Errorf("branch %q is not a branch to sync", name)
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

// scratchDir returns the absolute directory bundle files live in, creating it
// 0700 if needed, and sweeps stale bundles on every call. A failed sweep is
// best effort: it must not fail the RPC that triggered it.
func scratchDir() (string, error) {
	dir := filepath.Join(os.TempDir(), scratchDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create scratch dir %s: %w", dir, err)
	}
	sweepStaleBundles(dir)
	return dir, nil
}

// sweepStaleBundles removes regular *.bundle files older than
// scratchBundleMaxAge. Directories and other names are left alone, so a
// caller's own bookkeeping in the scratch dir survives.
func sweepStaleBundles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-scratchBundleMaxAge)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".bundle") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// randomHex returns n random bytes as 2n lowercase hex characters, for a
// bundle file name that cannot collide with or be predicted by a caller.
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
	out, err := s.git(repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %s", rev, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
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

	// Absent, or present but not a directory: not a repository. The caller
	// gets exists=false rather than an error -- "there is nothing there" is a
	// normal answer for a destination that is about to be seeded.
	fi, err := os.Stat(repo)
	if err != nil || !fi.IsDir() {
		return connect.NewResponse(resp), nil
	}
	if _, err := s.git(repo, "rev-parse", "--git-dir"); err != nil {
		return connect.NewResponse(resp), nil
	}

	out, err := s.git(repo, "for-each-ref", "--format=%(refname:short)%09%(objectname)", "refs/heads")
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
		if _, err := s.git(repo, "cat-file", "-e", oid+"^{commit}"); err == nil {
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

	args := make([]string, 0, 4+len(prereqs))
	args = append(args, "bundle", "create", bundlePath, "refs/heads/"+branch)
	for _, oid := range prereqs {
		args = append(args, "^"+oid)
	}
	out, err := s.git(repo, args...)
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
	// Lstat, not Stat: a symlink placed in the scratch dir must not be able to
	// point git at a file outside it, and must not be the file we later remove.
	info, err := os.Lstat(bundlePath)
	if err != nil || !info.Mode().IsRegular() || filepath.Dir(bundlePath) != scratch {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("bundle_path %q must be a regular file directly inside %s", bundlePath, scratch))
	}
	// The bundle is consumed exactly once, whatever the outcome: a refused
	// fetch must not leave a file a later call could replay.
	defer func() { _ = os.Remove(bundlePath) }()

	created, err := s.prepareFetchRepo(repo, branch)
	if err != nil {
		return nil, err
	}

	// verify needs a repository to check the bundle's prerequisites against,
	// which is why a missing repo is initialized first. A bundle that is
	// corrupt, or whose prerequisites this repo lacks, is refused here.
	if out, err := s.git(repo, "bundle", "verify", bundlePath); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("git bundle verify: %s", strings.TrimSpace(string(out))))
	}

	if created {
		refspec := "refs/heads/" + branch + ":refs/heads/" + branch
		out, err := s.git("", "-C", repo, "-c", "transfer.fsckObjects=true",
			"fetch", "--no-tags", "--update-head-ok", bundlePath, refspec)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out))))
		}
		// The freshly initialized repo has an unborn branch, so the fetch above
		// updated it with --update-head-ok but left the working tree empty.
		if out, err := s.git("", "-C", repo, "checkout", "-f", branch); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("git checkout: %s", strings.TrimSpace(string(out))))
		}
		newOID, _ := s.resolveCommit(repo, "refs/heads/"+branch)
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
	out, err := s.git("", "-C", repo, "-c", "transfer.fsckObjects=true",
		"fetch", "--no-tags", bundlePath, refspec)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out))))
	}
	newOID, _ := s.resolveCommit(repo, "refs/heads/"+branch)
	return connect.NewResponse(&executorpb.GitFetchBundleResponse{
		OldOid: oldOID,
		NewOid: newOID,
	}), nil
}

// prepareFetchRepo decides whether repo must be created, and creates it. It
// reports created=true when repo was absent or an empty directory. An existing
// non-empty directory that is not a repository is refused, never initialized
// over: this executor must not adopt a directory it did not create.
func (s *Server) prepareFetchRepo(repo, branch string) (created bool, err error) {
	fi, statErr := os.Stat(repo)
	switch {
	case statErr != nil && !os.IsNotExist(statErr):
		return false, connect.NewError(connect.CodeInternal, fmt.Errorf("stat repo: %w", statErr))
	case os.IsNotExist(statErr):
		created = true
	case !fi.IsDir():
		return false, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("repo %q exists and is not a directory", repo))
	default:
		empty, derr := dirIsEmpty(repo)
		if derr != nil {
			return false, connect.NewError(connect.CodeInternal, fmt.Errorf("read repo dir: %w", derr))
		}
		if empty {
			created = true
		} else if _, gerr := s.git(repo, "rev-parse", "--git-dir"); gerr != nil {
			return false, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("repo %q exists but is not a git repository", repo))
		}
	}

	if !created {
		return false, nil
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return false, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo dir: %w", err))
	}
	if out, err := s.git("", "init", "-b", branch, repo); err != nil {
		return false, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("git init: %s", strings.TrimSpace(string(out))))
	}
	return true, nil
}

// dirIsEmpty reports whether dir holds no entries.
func dirIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}
