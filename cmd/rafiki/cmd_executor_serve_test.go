// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/sandbox"

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
// anything else (no docker proxy, an http docker endpoint) implies none. The
// relay dir default is Linux-only, so pin the OS for this test.
func TestExecutorServeDefaultSandboxRelayDir(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	setRelayDirGOOS(t, "linux")

	c.Eq("/run/user/1000/rafiki/relay",
		defaultSandboxRelayDir(map[string]string{"docker": "unix:///var/run/docker.sock"}),
		"a local docker socket defaults the relay dir under the runtime dir")
	c.Eq("", defaultSandboxRelayDir(nil), "no docker proxy means no relay")
	c.Eq("", defaultSandboxRelayDir(map[string]string{"ollama": "http://localhost:11434"}),
		"a non-docker proxy means no relay")
	c.Eq("", defaultSandboxRelayDir(map[string]string{"docker": "http://remote:2375"}),
		"a remote docker endpoint cannot see a local relay dir")
}

// setRelayDirGOOS overrides the OS defaultSandboxRelayDir keys off, restored on
// cleanup so a test cannot leak a fake platform into its neighbours.
func setRelayDirGOOS(t *testing.T, goos string) {
	t.Helper()
	prev := relayDirGOOS
	relayDirGOOS = goos
	t.Cleanup(func() { relayDirGOOS = prev })
}

// A host-bound relay dir can never work through a macOS docker VM, so the
// default stays empty off Linux even for a local docker socket — the operator
// must opt in with --relay-dir.
func TestExecutorServeDefaultSandboxRelayDirIsLinuxOnly(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	setRelayDirGOOS(t, "darwin")

	c.Eq("", defaultSandboxRelayDir(map[string]string{"docker": "unix:///var/run/docker.sock"}),
		"a local docker socket must not imply a host-bound relay dir on darwin")
}

// Foothold mode and --relay-dir are two different relay mechanisms and cannot
// be combined.
func TestExecutorServeRelayFootholdExcludesRelayDir(t *testing.T) {
	c := assert.NewAborting(t)
	proxies := map[string]string{"docker": "unix:///var/run/docker.sock"}

	err := validateRelayFoothold("sandbox:latest", "/srv/relay", true, proxies)
	c.Require().Error(err, "a foothold image with a set --relay-dir must be refused")
	c.StrContains(err.Error(), "--relay-foothold-image and --relay-dir are mutually exclusive",
		"the refusal must name both flags")

	c.NoError(validateRelayFoothold("", "/srv/relay", true, proxies),
		"a relay dir without a foothold image is the ordinary host-bind path")
	c.NoError(validateRelayFoothold("sandbox:latest", "", false, proxies),
		"a foothold image with no relay dir is the foothold path")
}

// The foothold's engine dials the local docker unix socket directly, so an http
// docker endpoint or no docker proxy at all cannot host one.
func TestExecutorServeRelayFootholdRequiresLocalDockerSocket(t *testing.T) {
	c := assert.NewAborting(t)

	err := validateRelayFoothold("sandbox:latest", "", false, map[string]string{"docker": "http://remote:2375"})
	c.Require().Error(err, "an http docker endpoint cannot see the foothold's local socket")
	c.StrContains(err.Error(), "--proxy docker=unix://", "the refusal must say which proxy value is required")

	err = validateRelayFoothold("sandbox:latest", "", false, nil)
	c.Require().Error(err, "no docker proxy at all must be refused: there is no socket to dial")
	c.StrContains(err.Error(), "--proxy docker=unix://", "the refusal must say which proxy value is required")

	c.NoError(validateRelayFoothold("sandbox:latest", "", false, map[string]string{"docker": "unix:///var/run/docker.sock"}),
		"a local docker socket satisfies the requirement")
}

// A foothold relays sandbox connections to the daemon, so one with no daemon
// address is refused — the same three sources relayDirNeedsDaemon accepts.
func TestExecutorServeRelayFootholdNeedsDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("RAFIKI_URL", "")

	err := relayFootholdNeedsDaemon("sandbox:latest", "", "")
	c.Require().Error(err, "foothold mode with no daemon address must be refused")
	c.StrContains(err.Error(), "--relay-foothold-image", "the refusal must name the flag")
	c.StrContains(err.Error(), "--connect", "the refusal must say what is missing")

	c.NoError(relayFootholdNeedsDaemon("", "", ""), "no foothold image imposes no requirement")
	c.NoError(relayFootholdNeedsDaemon("sandbox:latest", "daemon.example.com:8443", ""), "--connect must satisfy the requirement")
	c.NoError(relayFootholdNeedsDaemon("sandbox:latest", "", "/run/rafikid.sock"), "--connect-socket must satisfy the requirement")

	t.Setenv("RAFIKI_URL", "https://rafiki.example.net")
	c.NoError(relayFootholdNeedsDaemon("sandbox:latest", "", ""), "a remote RAFIKI_URL must satisfy the requirement")
}

// The foothold relay must bind loopback ONLY: a wildcard bind would expose this
// unauthenticated relay (the daemon-side credential is the gate, not the relay)
// on every interface of the docker host.
func TestExecutorServeRelayFootholdBindsLoopbackOnly(t *testing.T) {
	c := assert.NewAborting(t)

	ln, port, err := bindFootholdRelay()
	c.Require().NoError(err, "bindFootholdRelay")
	defer ln.Close()

	c.True(port > 0, "a loopback bind must be assigned a port")
	addr, ok := ln.Addr().(*net.TCPAddr)
	c.Require().True(ok, "the foothold relay listener must be TCP")
	c.True(addr.IP.Equal(net.IPv4(127, 0, 0, 1)),
		"the foothold relay must bind 127.0.0.1, never a wildcard or 0.0.0.0")
}

// The foothold is the default exactly where defaultSandboxRelayDir is not: a
// local docker socket on a non-Linux host, with no explicit relay choice.
func TestResolveFootholdImage(t *testing.T) {
	c := assert.NewCollecting(t)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	local := map[string]string{"docker": "unix:///var/run/docker.sock"}

	setRelayDirGOOS(t, "darwin")
	c.Eq(sandbox.DefaultImage, resolveFootholdImage("", false, local), "darwin local socket defaults to the published image")
	c.Eq("mine:1", resolveFootholdImage("mine:1", false, local), "an explicit image wins")
	c.Eq("", resolveFootholdImage("", true, local), "an explicit --relay-dir opts out")
	c.Eq("", resolveFootholdImage("", false, map[string]string{"docker": "http://remote:2375"}), "a remote docker has no foothold")
	c.Eq("", resolveFootholdImage("", false, nil), "no docker proxy means no launcher")

	setRelayDirGOOS(t, "linux")
	c.Eq("", resolveFootholdImage("", false, local), "linux keeps the host-bind relay dir")
}
