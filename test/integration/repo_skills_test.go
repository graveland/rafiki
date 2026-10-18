package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/executorsdb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// ─── a daemon with one repo-sync executor ────────────────────────────────────
//
// The suite's own daemons run with RAFIKI_EXECUTORS_ENABLED=0 and
// grantDaemon.enrollExecutor never passes --repo-sync, so these tests enroll
// their own executor over the daemon's local executor unix socket. The
// enrollment is ANONYMOUS in the sense the repo pusher cares about: its owner
// label is the daemon's own OS username, which resolves to no user row, so the
// pusher treats the executor as the unattributed owner ("") — exactly the
// owner a CLI over the daemon's unix socket writes its repos as.

// repoSkillsEnv is one booted daemon plus the CLI profile directory its
// commands share.
type repoSkillsEnv struct {
	d         *daemon
	configDir string
}

// bootRepoSkills boots a database-backed daemon with the executor plane on and
// enrolls one repo-sync executor, waiting until the daemon's POOL holds it.
func bootRepoSkills(t *testing.T) *repoSkillsEnv {
	t.Helper()

	// The owner label the pusher resolves: the daemon's OS user, with no user
	// row behind it, is the unattributed owner. A developer who happens to have
	// a rafiki user by that name in the shared test database would claim the
	// label instead, and the anonymous CLI would then have no eligible executor
	// — skip loudly rather than fail on someone's local data.
	osName := currentOSUser(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; repo skills end-to-end requires postgres")
	}
	if userRowExists(t, dsn, osName) {
		t.Skipf("the shared test database already has a user named %q, which would claim this test's executor label; drop that user or use a clean database", osName)
	}

	d := bootDaemonDB(t, nextDaemonID(), "RAFIKI_EXECUTORS_ENABLED=1")
	t.Cleanup(func() { _ = os.RemoveAll(d.homeDir) })
	enrollRepoExecutor(t, d, dsn, osName)
	waitForLiveRepoExecutor(t, d, 1)
	return &repoSkillsEnv{d: d, configDir: t.TempDir()}
}

// cli runs one CLI command against the env's daemon and returns its stdout,
// stderr and error. Both streams are captured separately because the contract
// splits them: successes (and the refreshed summary) on stdout, skill errors
// and failures on stderr.
func (e *repoSkillsEnv) cli(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := cliCmdIn(t, e.d, e.configDir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// currentOSUser is the username the daemon's own osUser() resolves, which is
// this process's: the executor subprocess runs as the same user.
func currentOSUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	assert.NewAborting(t).NoError(err, "current OS user")
	return u.Username
}

// userRowExists reports whether the shared database already has a user with
// this username.
func userRowExists(t *testing.T, dsn, username string) bool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	assert.NewAborting(t).NoError(err, "connect for the user probe")
	defer pool.Close()
	var n int
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.users WHERE username = $1`, username).Scan(&n),
		"count users named %q", username)
	return n > 0
}

// enrollRepoExecutor mints an enrollment token and starts a native
// `rafiki executor serve` that reverse-dials the daemon's executor unix socket
// with --repo-sync. The per-enrollment `test-run` marker identifies the row:
// the shared database accumulates executor rows, and matching on the caller's
// labels would find a stale one from an earlier run.
func enrollRepoExecutor(t *testing.T, d *daemon, dsn, ownerLabel string) string {
	t.Helper()
	c := assert.NewAborting(t)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect for the executor store")
	t.Cleanup(pool.Close)
	store := executorsdb.NewPostgresStore(pool)

	marker := "t" + uniqueSuffix()
	token, err := store.MintToken(ctx, executors.NewToken{
		Labels:        map[string]string{"owner": ownerLabel, "test-run": marker},
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint token")

	execSock := filepath.Join(d.homeDir, "rafiki", "executor.sock")
	// The executor's own XDG dirs are isolated, and its TMPDIR with them: the
	// repo cache lives under XDG_CACHE_HOME, so this is what keeps one test's
	// checkouts out of the developer's ~/.cache and out of another test's.
	home := t.TempDir()
	root := t.TempDir()
	credFile := filepath.Join(t.TempDir(), "cred")
	cmd := exec.Command(cliBinary(), "executor", "serve",
		"--connect-socket", execSock,
		"--enroll-token", token,
		"--credential-file", credFile,
		"--root", root,
		"--repo-sync",
	)
	cmd.Env = subprocessEnv(
		"HOME="+home,
		"TMPDIR="+home,
		"XDG_RUNTIME_DIR="+home,
		"XDG_STATE_HOME="+home,
		"XDG_DATA_HOME="+home,
		"XDG_CONFIG_HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
	)
	c.NoError(cmd.Start(), "start repo-sync executor")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		execs, _ := store.List(ctx)
		for _, e := range execs {
			if e.Labels["test-run"] == marker && !e.LastSeenAt.IsZero() {
				return e.ID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("repo-sync executor never enrolled and became live")
	return ""
}

// waitForLiveRepoExecutor polls until the daemon's POOL reports n live
// executors. Enrollment's TouchSeen precedes the pool's live-map insert, and a
// repo refresh only reaches LIVE executors, so this wait is what makes the
// first `repo add` deterministic. The live set is only observable through
// selection: a throwaway spawn naming a selector nothing matches is refused
// with the live count in the message.
func waitForLiveRepoExecutor(t *testing.T, d *daemon, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
			Cwd:              t.TempDir(),
			Kind:             "fundi",
			Model:            "anthropic/claude-x",
			NoSession:        true,
			ExecutorSelector: "env=definitely-not-a-match",
		}))
		cancel()
		if err != nil && strings.Contains(err.Error(), fmt.Sprintf("%d live executor(s)", want)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the repo-sync executor never became live in the daemon's pool (%d expected)", want)
}

// ─── the git fixture ────────────────────────────────────────────────────────

// repoGit runs one git command in dir with an isolated config and identity, so
// a developer's global gitconfig never reaches the fixture.
func repoGit(t *testing.T, dir string, args ...string) string {
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

// repoFixture is one served repo: bare is what the executor fetches from, work
// is where later commits are made (and pushed to bare).
type repoFixture struct {
	bare string
	work string
}

// newRepoFixture builds a git repo holding skills/demo/SKILL.md and
// skills/demo/references/a.md, and a bare clone the executor can fetch.
//
// pluginName, when non-empty, is written to .claude-plugin/plugin.json — the
// namespace the checkout's skills are stored under. Two fixtures declaring the
// same plugin name therefore collide on that namespace, which is what the
// collision test needs.
func newRepoFixture(t *testing.T, pluginName string) *repoFixture {
	t.Helper()
	work := t.TempDir()
	repoGit(t, work, "init", "-q", "-b", "main")
	if pluginName != "" {
		writeRepoFile(t, work, ".claude-plugin/plugin.json",
			fmt.Sprintf("{\"name\": %q}\n", pluginName))
	}
	writeRepoFile(t, work, "skills/demo/SKILL.md",
		"---\nname: demo\ndescription: a demo skill\n---\n\n# demo\n\nDo the demo thing.\n")
	writeRepoFile(t, work, "skills/demo/references/a.md", "reference a\n")
	repoGit(t, work, "add", "-A")
	repoGit(t, work, "commit", "-q", "-m", "initial")

	bare := filepath.Join(t.TempDir(), "served.git")
	repoGit(t, work, "clone", "-q", "--bare", work, bare)
	return &repoFixture{bare: bare, work: work}
}

// addSkill writes one more skill into the working tree and pushes the commit
// to the served bare repo, so the next refresh resolves a NEW commit.
func (f *repoFixture) addSkill(t *testing.T, name, description string) {
	t.Helper()
	writeRepoFile(t, f.work, "skills/"+name+"/SKILL.md",
		fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n# %s\n", name, description, name))
	repoGit(t, f.work, "add", "-A")
	repoGit(t, f.work, "commit", "-q", "-m", "add "+name)
	repoGit(t, f.work, "push", "-q", f.bare, "main")
}

// writeRepoFile writes a fixture file, creating its directories.
func writeRepoFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	assert.NewAborting(t).NoError(os.MkdirAll(filepath.Dir(path), 0o755), "mkdir %s", filepath.Dir(path))
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(content), 0o644), "write %s", path)
}

// ─── the tests ──────────────────────────────────────────────────────────────

// TestRepoSkillsAddListsSkill is the whole flow end to end: `repo add` clones
// the fixture on the executor, the daemon stores the skills it discovered under
// the repo's own namespace, and `skills list` shows them — at the GLOBAL layer,
// because the caller is the anonymous unix-socket identity.
func TestRepoSkillsAddListsSkill(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	e := bootRepoSkills(t)
	repo := newRepoFixture(t, "")
	name := "skillrepo" + strings.ReplaceAll(uniqueSuffix(), "-", "")
	t.Cleanup(func() { _, _, _ = e.cli(t, "repo", "remove", name) })

	stdout, stderr, err := e.cli(t, "repo", "add", name, repo.bare)
	c.NoError(err, "repo add failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	// The summary line names the source, the commit and all three counts.
	c.StrContains(stdout, "refreshed "+name+" at ", "add output:\n%s", stdout)
	c.StrContains(stdout, ": 0 script(s), 0 package(s), 1 skill(s)", "add output:\n%s", stdout)
	c.NotStrContains(stdout, "(unchanged)", "a first add reported unchanged:\n%s", stdout)

	list, listErr, err := e.cli(t, "skills", "list")
	c.NoError(err, "skills list failed: %v\nstdout: %s\nstderr: %s", err, list, listErr)
	// No plugin.json, so the checkout's skills are namespaced by the repo name.
	c.StrContains(list, name+":demo", "skills list does not show the repo's skill:\n%s", list)
	c.StrContains(list, "global", "the repo's skill is not at the global layer:\n%s", list)
	c.StrContains(list, "repo:"+name, "the repo's skill does not name its source marker:\n%s", list)

	// The skill's supporting file came with it.
	show, showErr, err := e.cli(t, "skills", "show", name+":demo")
	c.NoError(err, "skills show failed: %v\nstdout: %s\nstderr: %s", err, show, showErr)
	c.StrContains(show, "Do the demo thing.", "skills show lost the body:\n%s", show)
	c.StrContains(show, "references/a.md", "skills show lost the supporting file:\n%s", show)

	// And the registration records what it resolved.
	repos, reposErr, err := e.cli(t, "repo", "list")
	c.NoError(err, "repo list failed: %v\nstdout: %s\nstderr: %s", err, repos, reposErr)
	c.StrContains(repos, name, "repo list lost the registration:\n%s", repos)
	c.StrContains(repos, "COMMIT", "repo list has no COMMIT column:\n%s", repos)
	c.StrContains(repos, "REFRESH", "repo list has no REFRESH column:\n%s", repos)
}

// TestRepoSkillsRefreshPicksUpNewCommit pins the refresh half: a new commit on
// the served repo is resolved, fanned out, and its skills stored — and the
// skill the previous commit shipped stays, because the set is replaced whole
// rather than appended to.
func TestRepoSkillsRefreshPicksUpNewCommit(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	e := bootRepoSkills(t)
	repo := newRepoFixture(t, "")
	name := "refreshrepo" + strings.ReplaceAll(uniqueSuffix(), "-", "")
	t.Cleanup(func() { _, _, _ = e.cli(t, "repo", "remove", name) })

	_, stderr, err := e.cli(t, "repo", "add", name, repo.bare)
	c.NoError(err, "repo add failed: %v\nstderr: %s", err, stderr)

	repo.addSkill(t, "extra", "a skill the second commit adds")

	stdout, stderr, err := e.cli(t, "repo", "refresh", name)
	c.NoError(err, "repo refresh failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	c.StrContains(stdout, "refreshed "+name+" at ", "refresh output:\n%s", stdout)
	c.StrContains(stdout, ": 0 script(s), 0 package(s), 2 skill(s)", "refresh output:\n%s", stdout)
	c.NotStrContains(stdout, "(unchanged)", "a refresh that resolved a new commit reported unchanged:\n%s", stdout)

	list, listErr, err := e.cli(t, "skills", "list")
	c.NoError(err, "skills list failed: %v\nstdout: %s\nstderr: %s", err, list, listErr)
	c.StrContains(list, name+":extra", "the new commit's skill never reached the corpus:\n%s", list)
	c.StrContains(list, name+":demo", "the previous commit's skill was dropped instead of replaced:\n%s", list)

	// Refreshing again with nothing new says so: the ref did not move.
	again, againErr, err := e.cli(t, "repo", "refresh", name)
	c.NoError(err, "second refresh failed: %v\nstdout: %s\nstderr: %s", err, again, againErr)
	c.StrContains(again, "(unchanged)", "a refresh of an unmoved ref did not report unchanged:\n%s", again)
}

// TestRepoSkillsRemoveDropsSkills pins remove's whole effect: the repo's skills
// go with it (the source marker is the only handle on them), the registration
// goes, and a later refresh of everything has nothing to do.
func TestRepoSkillsRemoveDropsSkills(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	e := bootRepoSkills(t)
	repo := newRepoFixture(t, "")
	name := "removerepo" + strings.ReplaceAll(uniqueSuffix(), "-", "")

	_, stderr, err := e.cli(t, "repo", "add", name, repo.bare)
	c.NoError(err, "repo add failed: %v\nstderr: %s", err, stderr)
	list, _, err := e.cli(t, "skills", "list")
	c.NoError(err, "skills list failed")
	c.StrContains(list, name+":demo", "precondition: the repo's skill is not in the corpus:\n%s", list)

	removeOut, removeErr, err := e.cli(t, "repo", "remove", name)
	c.NoError(err, "repo remove failed: %v\nstdout: %s\nstderr: %s", err, removeOut, removeErr)
	c.StrContains(removeOut, "removed "+name, "remove output:\n%s", removeOut)

	list, _, err = e.cli(t, "skills", "list")
	c.NoError(err, "skills list after remove failed")
	c.NotStrContains(list, name+":demo", "the removed repo's skill is still in the corpus:\n%s", list)
	// The skill is gone for good, not merely disabled: --all shows disabled rows.
	all, _, err := e.cli(t, "skills", "list", "--all")
	c.NoError(err, "skills list --all failed")
	c.NotStrContains(all, name+":demo", "the removed repo's skill survived as a disabled row:\n%s", all)

	repos, _, err := e.cli(t, "repo", "list")
	c.NoError(err, "repo list after remove failed")
	c.NotStrContains(repos, name, "the removed registration is still listed:\n%s", repos)
}

// TestRepoSkillsCollisionRollsBackAdd pins the one skill failure that fails a
// registration: two repos whose .claude-plugin/plugin.json name the same
// plugin claim one namespace, and a namespace is owned whole by its source. The
// second add is refused AND its registration is rolled back — a repo that can
// never store its skills must not linger as a registration.
func TestRepoSkillsCollisionRollsBackAdd(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	e := bootRepoSkills(t)
	suffix := strings.ReplaceAll(uniqueSuffix(), "-", "")
	firstName := "collideone" + suffix
	secondName := "collidetwo" + suffix
	plugin := "sharedplugin" + suffix
	t.Cleanup(func() { _, _, _ = e.cli(t, "repo", "remove", firstName) })

	first := newRepoFixture(t, plugin)
	_, stderr, err := e.cli(t, "repo", "add", firstName, first.bare)
	c.NoError(err, "the first add failed: %v\nstderr: %s", err, stderr)
	// The namespace comes from plugin.json, not the repo name.
	list, _, err := e.cli(t, "skills", "list")
	c.NoError(err, "skills list failed")
	c.StrContains(list, plugin+":demo", "the first repo's skills are not under its plugin namespace:\n%s", list)

	second := newRepoFixture(t, plugin)
	stdout, stderr, err := e.cli(t, "repo", "add", secondName, second.bare)
	c.Error(err, "the second add of a colliding namespace succeeded\nstdout: %s\nstderr: %s", stdout, stderr)
	c.StrContains(stderr, "already provided by repo:"+firstName, "the refusal does not name the holder:\n%s", stderr)

	// The failed registration is gone; the first one is untouched.
	repos, _, err := e.cli(t, "repo", "list")
	c.NoError(err, "repo list failed")
	c.StrContains(repos, firstName, "the first registration was lost:\n%s", repos)
	c.NotStrContains(repos, secondName, "the rolled-back registration is still listed:\n%s", repos)

	// And nothing was written for it: the namespace still holds exactly the
	// first repo's skills.
	list, _, err = e.cli(t, "skills", "list")
	c.NoError(err, "skills list after the collision failed")
	c.StrContains(list, plugin+":demo", "the first repo's skills were lost:\n%s", list)
	c.NotStrContains(list, "repo:"+secondName, "the rolled-back repo left skill rows behind:\n%s", list)
}
