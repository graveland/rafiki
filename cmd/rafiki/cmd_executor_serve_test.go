// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

func TestExecutorProfileProxyNoManifestDoesNotBootstrap(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	cmd := newRootCmd()

	c.Eq("", executorProfileProxy(cmd), "executorProfileProxy")
	_, err := profile.Load()
	c.Error(err, "executorProfileProxy must not create a profiles.toml as a side effect")
}

func TestExecutorProfileProxyResolvesTheSelectedProfile(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", URL: "https://rafiki.example.net"},
	}}), "Save")
	c.NoError(profile.SavePointer("work"), "SavePointer")
	cmd := newRootCmd()

	got := executorProfileProxy(cmd)
	c.Eq("https://rafiki.example.net", got, "executorProfileProxy")
}

func TestExecutorProfileProxyUnknownProfileNameDegradesToEmpty(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", URL: "https://rafiki.example.net"},
	}}), "Save")
	cmd := newRootCmd()
	c.NoError(cmd.PersistentFlags().Set("profile", "does-not-exist"), "Set profile flag")

	c.Eq("", executorProfileProxy(cmd), "executorProfileProxy")
}

func TestExecutorProfileProxySocketOnlyProfileIsEmpty(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"local": {Name: "local", Socket: filepath.Join(t.TempDir(), "d.sock")},
	}}), "Save")
	c.NoError(profile.SavePointer("local"), "SavePointer")
	cmd := newRootCmd()

	c.Eq("", executorProfileProxy(cmd), "executorProfileProxy")
}

func TestExecutorProfileProxyExplicitFlagWithNoManifestStillAttemptsResolution(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	cmd := newRootCmd()
	c.NoError(cmd.PersistentFlags().Set("profile", "work"), "Set profile flag")

	c.Eq("", executorProfileProxy(cmd), "executorProfileProxy")
	_, err := profile.Load()
	c.Error(err, "an explicit -P on a machine with no manifest must still never bootstrap one — profile.Resolve refuses to bootstrap a requested name on its own")
}

// skillsSyncFromArgv drives the same path RunE uses — parse the flags, read
// them back, resolve them against the environment — for the combinations that
// matter: flag off and env unset (the default), flag off and env set, an
// explicit --skills-sync=false over a set env, the bare flag, and the
// --launch claude implication with and without an explicit refusal.
func skillsSyncFromArgv(t *testing.T, argv []string, env string) bool {
	t.Helper()
	t.Setenv("RAFIKI_EXECUTOR_SKILLS_SYNC", env)
	c := assert.NewAborting(t)
	cmd := newExecutorServeCmd()
	c.NoError(cmd.Flags().Parse(argv))
	on, err := cmd.Flags().GetBool("skills-sync")
	c.NoError(err)
	launchKinds, err := cmd.Flags().GetStringArray("launch")
	c.NoError(err)
	return skillsSyncEnabled(cmd, on, launchKinds)
}

func TestSkillsSyncDefaultsOff(t *testing.T) {
	assert.NewCollecting(t).False(skillsSyncFromArgv(t, nil, ""), "skills-sync defaulted on; it writes into the operator's home directory")
}

func TestSkillsSyncEnvTurnsItOn(t *testing.T) {
	assert.NewCollecting(t).True(skillsSyncFromArgv(t, nil, "1"), "a non-empty RAFIKI_EXECUTOR_SKILLS_SYNC must enable skills sync — a service unit cannot edit argv")
}

func TestSkillsSyncExplicitFalseBeatsTheEnvironment(t *testing.T) {
	assert.NewCollecting(t).False(skillsSyncFromArgv(t, []string{"--skills-sync=false"}, "1"), "an explicit --skills-sync=false must stay able to switch the feature off on a machine whose environment enables it")
}

func TestSkillsSyncFlagTurnsItOn(t *testing.T) {
	assert.NewCollecting(t).True(skillsSyncFromArgv(t, []string{"--skills-sync"}, ""), "--skills-sync must enable skills sync")
}

func TestSkillsSyncImpliedByLaunchClaude(t *testing.T) {
	if !skillsSyncFromArgv(t, []string{"--launch", "claude"}, "") {
		t.Error("--launch claude must imply skills sync — a claude host without the " +
			"corpus launches children that silently see no rafiki skills")
	}
}

func TestSkillsSyncExplicitFalseBeatsLaunchClaude(t *testing.T) {
	assert.NewCollecting(t).False(skillsSyncFromArgv(t, []string{"--launch", "claude", "--skills-sync=false"}, ""), "an explicit --skills-sync=false must beat the --launch claude implication")
}

func TestSkillsSyncNotImpliedByOtherLaunchKinds(t *testing.T) {
	assert.NewCollecting(t).False(skillsSyncFromArgv(t, []string{"--launch", "other"}, ""), "only --launch claude implies skills sync")
}

// pymoduleGitSyncFromArgv drives the same path RunE uses for
// --pymodule-git-sync: parse the flags, read them back, resolve them against
// the environment (see skillsSyncFromArgv for the shape this mirrors).
func pymoduleGitSyncFromArgv(t *testing.T, argv []string, env string) bool {
	t.Helper()
	t.Setenv("RAFIKI_EXECUTOR_PYMODULE_GIT_SYNC", env)
	c := assert.NewAborting(t)
	cmd := newExecutorServeCmd()
	c.NoError(cmd.Flags().Parse(argv))
	on, err := cmd.Flags().GetBool("pymodule-git-sync")
	c.NoError(err)
	launchKinds, err := cmd.Flags().GetStringArray("launch")
	c.NoError(err)
	return pymoduleGitSyncEnabled(cmd, on, launchKinds)
}

func TestPymoduleGitSyncDefaultsOff(t *testing.T) {
	assert.NewCollecting(t).False(pymoduleGitSyncFromArgv(t, nil, ""), "pymodule-git-sync defaulted on; it runs git clones against registered URLs")
}

func TestPymoduleGitSyncEnvTurnsItOn(t *testing.T) {
	assert.NewCollecting(t).True(pymoduleGitSyncFromArgv(t, nil, "1"), "a non-empty RAFIKI_EXECUTOR_PYMODULE_GIT_SYNC must enable git-source sync — a service unit cannot edit argv")
}

func TestPymoduleGitSyncExplicitFalseBeatsTheEnvironment(t *testing.T) {
	assert.NewCollecting(t).False(pymoduleGitSyncFromArgv(t, []string{"--pymodule-git-sync=false"}, "1"), "an explicit --pymodule-git-sync=false must stay able to switch the feature off on a machine whose environment enables it")
}

func TestPymoduleGitSyncFlagTurnsItOn(t *testing.T) {
	assert.NewCollecting(t).True(pymoduleGitSyncFromArgv(t, []string{"--pymodule-git-sync"}, ""), "--pymodule-git-sync must enable git-source sync")
}

// --launch script implies both pymodule syncs: a script child resolves its
// pymodule from THIS machine's synced cache (blob corpus with venvs, git
// checkouts), and an executor whose caches stay empty hosts scripts that can
// never resolve. The same flag-vs-env-vs-launch precedence skills sync uses:
// an explicit false wins, the environment turns it on regardless.
func TestPymoduleSyncImpliedByLaunchScript(t *testing.T) {
	t.Setenv("RAFIKI_EXECUTOR_PYMODULES_SYNC", "")
	c := assert.NewCollecting(t)
	cmd := newExecutorServeCmd()
	c.Require().NoError(cmd.Flags().Parse([]string{"--launch", "script"}))
	on, err := cmd.Flags().GetBool("pymodules-sync")
	c.Require().NoError(err)
	launchKinds, _ := cmd.Flags().GetStringArray("launch")
	c.True(pymodulesSyncEnabled(cmd, on, launchKinds), "--launch script must imply pymodules sync — a script host without the corpus hosts scripts that cannot resolve")
}

func TestPymoduleGitSyncImpliedByLaunchScript(t *testing.T) {
	assert.NewCollecting(t).True(pymoduleGitSyncFromArgv(t, []string{"--launch", "script"}, ""), "--launch script must imply git-source sync — a script host that refuses checkouts only hosts half the scripts")
}

func TestPymoduleSyncExplicitFalseBeatsLaunchScript(t *testing.T) {
	t.Setenv("RAFIKI_EXECUTOR_PYMODULES_SYNC", "")
	c := assert.NewCollecting(t)
	cmd := newExecutorServeCmd()
	c.Require().NoError(cmd.Flags().Parse([]string{"--launch", "script", "--pymodules-sync=false"}))
	on, err := cmd.Flags().GetBool("pymodules-sync")
	c.Require().NoError(err)
	launchKinds, _ := cmd.Flags().GetStringArray("launch")
	c.False(pymodulesSyncEnabled(cmd, on, launchKinds), "an explicit --pymodules-sync=false must beat the --launch script implication")
}

func TestPymoduleGitSyncExplicitFalseBeatsLaunchScript(t *testing.T) {
	assert.NewCollecting(t).False(pymoduleGitSyncFromArgv(t, []string{"--launch", "script", "--pymodule-git-sync=false"}, ""), "an explicit --pymodule-git-sync=false must beat the --launch script implication")
}

// --relay-dir binds a socket every sandbox connection is spliced through, so a
// relay with nowhere to dial is refused rather than started. The daemon address
// may come from --connect, --connect-socket, or a remote RAFIKI_URL — the same
// three sources resolveExecutorConnectFlags accepts.
func TestExecutorServeRelayDirRequiresConnect(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("RAFIKI_URL", "")

	err := relayDirNeedsDaemon("/srv/relay", "", "")
	c.Error(err, "--relay-dir with no daemon address must be refused")
	c.StrContains(err.Error(), "--relay-dir", "the refusal must name the flag")
	c.StrContains(err.Error(), "--connect", "the refusal must say what is missing")

	c.NoError(relayDirNeedsDaemon("", "", ""), "no relay dir imposes no requirement")
	c.NoError(relayDirNeedsDaemon("/srv/relay", "daemon.example.com:8443", ""), "--connect must satisfy the requirement")
	c.NoError(relayDirNeedsDaemon("/srv/relay", "", "/run/rafikid.sock"), "--connect-socket must satisfy the requirement")

	t.Setenv("RAFIKI_URL", "https://rafiki.example.net")
	c.NoError(relayDirNeedsDaemon("/srv/relay", "", ""), "a remote RAFIKI_URL must satisfy the requirement")
}

// --sandbox-mount-root gates which host paths a container may bind-mount, so a
// relative or missing root is refused up front rather than surfacing much later
// as an opaque refusal in the docker proxy guard.
func TestExecutorServeResolveSandboxMountRootsValidatesEachRoot(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()

	got, err := resolveSandboxMountRoots([]string{dir})
	c.NoError(err, "an absolute existing directory must be accepted")
	c.EqDeep([]string{dir}, got, "the accepted roots must be returned verbatim")

	_, err = resolveSandboxMountRoots([]string{"relative/path"})
	c.Require().Error(err, "a relative root must be refused")
	c.StrContains(err.Error(), "--sandbox-mount-root", "the refusal must name the flag")

	_, err = resolveSandboxMountRoots([]string{filepath.Join(dir, "does-not-exist")})
	c.Require().Error(err, "a missing root must be refused")
	c.StrContains(err.Error(), "--sandbox-mount-root", "the refusal must name the flag")

	file := filepath.Join(dir, "a-file")
	c.Require().NoError(os.WriteFile(file, []byte("x"), 0o644))
	_, err = resolveSandboxMountRoots([]string{file})
	c.Require().Error(err, "a path that is not a directory must be refused")
}

// --relay-dir is made by the relay if missing, so only absoluteness is checked.
func TestExecutorServeResolveSandboxRelayDirRequiresAbsolute(t *testing.T) {
	c := assert.NewAborting(t)

	got, err := resolveSandboxRelayDir("")
	c.NoError(err)
	c.Eq("", got, "an unset relay dir stays unset")

	got, err = resolveSandboxRelayDir("/srv/relay")
	c.NoError(err)
	c.Eq("/srv/relay", got, "an absolute relay dir is accepted as given")

	_, err = resolveSandboxRelayDir("relay")
	c.Require().Error(err, "a relative relay dir must be refused")
	c.StrContains(err.Error(), "--relay-dir", "the refusal must name the flag")
}

// A local docker socket implies the relay dir, beside the daemon's own sockets;
// anything else (no docker proxy, an http docker endpoint) implies none.
func TestExecutorServeDefaultSandboxRelayDir(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")

	c.Eq("/run/user/1000/rafiki/relay",
		defaultSandboxRelayDir(map[string]string{"docker": "unix:///var/run/docker.sock"}),
		"a local docker socket defaults the relay dir under the runtime dir")
	c.Eq("", defaultSandboxRelayDir(nil), "no docker proxy means no relay")
	c.Eq("", defaultSandboxRelayDir(map[string]string{"ollama": "http://localhost:11434"}),
		"a non-docker proxy means no relay")
	c.Eq("", defaultSandboxRelayDir(map[string]string{"docker": "http://remote:2375"}),
		"a remote docker endpoint cannot see a local relay dir")
}
