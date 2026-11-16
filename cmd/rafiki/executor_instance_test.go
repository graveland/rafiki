// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// resetInstance restores the process-wide executor instance and clears its pin.
// Tests only: the instance is global on purpose (one per process, set before any
// path is read), so a test that names or pins one must not leave the package's
// other tests resolving their paths under profiles/<name>.
func resetInstance(t *testing.T) {
	t.Helper()
	t.Cleanup(paths.ResetInstanceForTest)
}

// executorVerb builds one executor-service verb under a root carrying the
// persistent -P, and parses args into it — the shape cobra produces for
// `rafiki [-P name] executor …`.
func executorVerb(c *assert.C, newVerb func() *cobra.Command, args ...string) *cobra.Command {
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	cmd := newVerb()
	root.AddCommand(cmd)
	c.NoError(cmd.ParseFlags(args), "parse %v", args)
	return cmd
}

// seedPointerProfiles writes two profiles and points the current-profile pointer
// at the second, so a test can tell the pointer's instance from a declared one.
func seedPointerProfiles(t *testing.T) {
	t.Helper()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work":     {Name: "work", Socket: filepath.Join(t.TempDir(), "work.sock")},
		"personal": {Name: "personal", Socket: filepath.Join(t.TempDir(), "personal.sock")},
	}}); err != nil {
		t.Fatalf("save profiles: %v", err)
	}
	if err := profile.SavePointer("personal"); err != nil {
		t.Fatalf("save pointer: %v", err)
	}
}

// A service's on-disk state must not move when someone runs `rafiki profile
// use`: every executor-service verb PINS its instance from the RAW -P /
// RAFIKI_PROFILE, else default — never the current-profile pointer that
// resolveProfile follows. Without the pin, `executor setup`'s enrollment lookup
// (which resolves a profile) flips the instance after the credential check and
// before the write, splitting them across two directories.
func TestExecutorInstanceServiceVerbsIgnoreCurrentProfilePointer(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	resetInstance(t)
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	seedPointerProfiles(t)

	// Mirror case: with no executor verb in play, an ordinary client command
	// follows the pointer — that is the per-profile namespacing that stays.
	p, err := resolveProfile(executorVerb(c, newExecutorServiceStatusCmd))
	c.NoError(err, "resolveProfile")
	c.Eq("personal", p.Name, "the pointer selects the profile")
	c.Eq("personal", paths.Instance(), "an ordinary client command takes the pointer's instance")
	c.Eq(filepath.Join(dataHome, "rafiki", "profiles", "personal"), paths.DataDir(), "DataDir from the pointer")

	// No -P: the verb pins the DEFAULT instance, and a later pointer resolution
	// — exactly what executor setup's enrollment client does — must not move it.
	resetProfileCache()
	c.NoError(applyExecutorInstance(executorVerb(c, newExecutorServiceStatusCmd)), "applyExecutorInstance, no -P")
	c.Eq(paths.DefaultInstance, paths.Instance(), "a service verb with no -P must resolve the default instance")
	c.Eq(filepath.Join(dataHome, "rafiki"), paths.DataDir(), "DataDir with no -P")
	c.Eq(filepath.Join(dataHome, "rafiki", "executor.cred"),
		filepath.Join(paths.DataDir(), "executor.cred"),
		"the credential the enrolment check reads is the one the write uses")
	c.Eq(filepath.Join(paths.ConfigDir(), "executor.env"), paths.ExecutorEnvFile(),
		"the env file install writes with no -P")

	p, err = resolveProfile(executorVerb(c, newExecutorSetupCmd))
	c.NoError(err, "resolveProfile after the pin")
	c.Eq("personal", p.Name, "the pointer still resolves")
	c.Eq(paths.DefaultInstance, paths.Instance(), "a pointer resolution must not move a pinned instance")
	c.Eq(filepath.Join(dataHome, "rafiki"), paths.DataDir(), "DataDir after the pointer resolution")
	c.Eq(filepath.Join(paths.ConfigDir(), "executor.env"), paths.ExecutorEnvFile(),
		"the env file stays in the pinned instance")

	// A declared name wins over the pointer for the whole process.
	resetProfileCache()
	c.NoError(applyExecutorInstance(executorVerb(c, newExecutorServiceStatusCmd, "-P", "work")), "applyExecutorInstance, -P work")
	c.Eq("work", paths.Instance(), "an explicit -P must name the instance")
	c.Eq(filepath.Join(dataHome, "rafiki", "profiles", "work"), paths.DataDir(), "DataDir with -P work")
	_, err = resolveProfile(executorVerb(c, newExecutorSetupCmd))
	c.NoError(err, "resolveProfile after the -P pin")
	c.Eq("work", paths.Instance(), "the pointer must not override a declared -P")
	c.Eq(filepath.Join(paths.ConfigDir(), "executor-work.env"), paths.ExecutorEnvFile(),
		"the env file install writes with -P work")

	// And the environment form, which the pointer never supplies.
	resetProfileCache()
	t.Setenv("RAFIKI_PROFILE", "work")
	c.NoError(applyExecutorInstance(executorVerb(c, newExecutorServiceStatusCmd)), "applyExecutorInstance from RAFIKI_PROFILE")
	_, err = resolveProfile(executorVerb(c, newExecutorSetupCmd))
	c.NoError(err, "resolveProfile after the env pin")
	c.Eq("work", paths.Instance(), "RAFIKI_PROFILE names the instance, and the pointer cannot move it")
}

// The failing sequence itself: `executor setup` with no credential mints a token,
// minting resolves the profile through newConnectEndpoint, and with no -P that
// follows the pointer. The pin must keep the write in the instance the credential
// check already read.
func TestExecutorInstanceSetupPinsThroughTheEnrollmentLookup(t *testing.T) {
	c := assert.NewAborting(t)
	isolateSetup(t)
	resetProfileCache()
	resetInstance(t)
	t.Setenv(paths.ExecutorEnvFileEnv, "") // let the instance name the file
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	seedPointerProfiles(t)
	seedMachineName(t, "test-box")
	stubExecutorSetup(t)

	// The token-minter, standing in for production's newConnectEndpoint: it
	// resolves the profile, which is the flip this test exists for.
	var resolved bool
	orig := executorEnrollClient
	executorEnrollClient = func(_ context.Context, cmd *cobra.Command, _ string, _ []string) (string, error) {
		resolved = true
		if _, err := resolveProfile(cmd); err != nil {
			return "", err
		}
		return "one-time-token", nil
	}
	t.Cleanup(func() { executorEnrollClient = orig })

	// The enrollment exchange dials a daemon that is not there; a short deadline
	// ends it at the connect, which is AFTER the env file is written.
	cmd := newExecutorSetupCmd()
	c.NoError(cmd.ParseFlags([]string{"--non-interactive", "--connect", "127.0.0.1:1"}), "parse setup flags")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)
	var out strings.Builder
	w := newWizard(cmd)
	w.out = &out
	err := runExecutorSetup(ctx, w, &out)

	c.Error(err, "the enrollment exchange has no daemon to reach")
	c.True(resolved, "the flow must have resolved a profile, or this test proves nothing")
	c.Eq(paths.DefaultInstance, paths.Instance(), "the pointer's profile must not move the pinned instance")
	c.Eq(filepath.Join(dataHome, "rafiki"), paths.DataDir(), "DataDir after the enrollment lookup")
	_, statErr := os.Stat(filepath.Join(paths.ConfigDir(), "executor.env"))
	c.NoError(statErr, "the env file the install wrote before enrolling")
	_, workErr := os.Stat(filepath.Join(paths.ConfigDir(), "executor-work.env"))
	c.True(os.IsNotExist(workErr), "the pointer's instance must not be written")
}

// recordingServiceBackend captures the spec installExecutorService hands the
// service manager, so a test can assert which instance the unit was written for.
type recordingServiceBackend struct {
	fakeExecutorBackend
	spec serviceSpec
}

func (r *recordingServiceBackend) Install(spec serviceSpec) error {
	r.spec = spec
	r.installed = true
	return nil
}

// The credential check and the write are provably ONE instance for one
// invocation: the credential seeded in the default directory satisfies the
// check (no token is minted), and the unit the install writes carries that same
// credential path, no --profile, and the default environment file — with the
// pointer naming another profile throughout.
func TestExecutorInstanceSetupWritesTheCheckedInstance(t *testing.T) {
	c := assert.NewAborting(t)
	isolateSetup(t)
	resetProfileCache()
	resetInstance(t)
	t.Setenv(paths.ExecutorEnvFileEnv, "") // let the instance name the file
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	seedPointerProfiles(t)
	seedMachineName(t, "test-box")
	seedExecutorCredential(t) // in the DEFAULT data dir, which is the instance at check time

	// Minting would mean the check read a different directory from the write.
	origEnroll := executorEnrollClient
	executorEnrollClient = func(context.Context, *cobra.Command, string, []string) (string, error) {
		t.Error("executor setup minted a token: the credential check and the write disagreed on the instance")
		return "", errors.New("unexpected enrollment")
	}
	t.Cleanup(func() { executorEnrollClient = origEnroll })

	rec := &recordingServiceBackend{}
	origBackend := executorServiceBackendFor
	executorServiceBackendFor = func() serviceBackend { return rec }
	origWait := executorSetupWaitLive
	executorSetupWaitLive = func(context.Context, *cobra.Command, string) error { return nil }
	t.Cleanup(func() {
		executorServiceBackendFor = origBackend
		executorSetupWaitLive = origWait
	})

	if _, err := runExecutorSetupForTest(t, "--non-interactive"); err != nil {
		t.Fatalf("runExecutorSetup: %v", err)
	}
	c.True(rec.installed, "the unit must have been installed")

	c.Eq(paths.DefaultInstance, paths.Instance(), "the pointer must not have moved the instance")
	c.False(slices.Contains(rec.spec.Args, "--profile"),
		"a default-instance unit must not carry --profile: %v", rec.spec.Args)
	c.Eq(filepath.Join(dataHome, "rafiki", "executor.cred"), flagValue(rec.spec.Args, "--credential-file"),
		"the unit's credential file is the one the check read")
	_, statErr := os.Stat(filepath.Join(paths.ConfigDir(), "executor.env"))
	c.NoError(statErr, "the env file the install wrote")
	_, workErr := os.Stat(filepath.Join(paths.ConfigDir(), "executor-work.env"))
	c.True(os.IsNotExist(workErr), "the pointer's instance must not be written")
}

// flagValue returns the argument following name, or "" when it is absent.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// `executor serve` takes its instance from the RAW -P value, never from
// profiles.toml: a remote target installed over ssh has no profiles.toml, and a
// service whose state moved when someone ran `rafiki profile use` would be a
// service serving another instance's credential.
func TestExecutorInstanceServeUsesRawFlagNotProfilesFile(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetInstance(t)
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))

	root := newRootCmd()
	serve := newExecutorServeCmd()
	root.AddCommand(serve)
	// ParseFlags merges the root's persistent flags into serve's set, exactly as
	// cobra does when it executes `rafiki -P ublk executor serve`.
	c.NoError(serve.ParseFlags([]string{"-P", "ublk"}), "parse -P ublk")
	c.Eq("ublk", executorInstanceName(serve), "executorInstanceName reads the inherited flag")

	// The first thing serve's RunE does, before any path is resolved.
	c.NoError(applyExecutorInstance(serve), "applyExecutorInstance")

	c.Eq("ublk", paths.Instance(), "instance")
	c.Eq(filepath.Join(dataHome, "rafiki", "profiles", "ublk"), paths.DataDir(), "DataDir")
	c.Eq(filepath.Join(dataHome, "rafiki", "profiles", "ublk", "executor.cred"),
		filepath.Join(paths.DataDir(), "executor.cred"), "the credential file is per instance")

	_, err := profile.Load()
	c.Error(err, "serve must not resolve — or bootstrap — a profiles.toml to learn its instance")
}

// A non-default instance with no CLAUDE_CONFIG_DIR of its own would write the
// same ~/.claude/skills the default instance writes. The implied forms stay off
// and an explicit --skills-sync=true is refused, naming the fix.
func TestExecutorInstanceSkillsSyncRefusedForNonDefault(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetInstance(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("RAFIKI_EXECUTOR_SKILLS_SYNC", "")

	c.NoError(paths.SetInstance("work"), "SetInstance(work)")

	explicit := newExecutorServeCmd()
	c.NoError(explicit.Flags().Set("skills-sync", "true"), "set --skills-sync=true")
	err := refuseSkillsSyncForNonDefaultInstance(explicit, true)
	c.Error(err, "an explicit --skills-sync=true must be refused for a non-default instance")
	c.StrContains(err.Error(),
		`--skills-sync writes ~/.claude/skills, which instance "default" also writes; `+
			`set CLAUDE_CONFIG_DIR for instance "work" to enable it`,
		"refusal text")

	// --launch claude implies the sync for the default instance; here it is
	// switched off rather than refused, because nothing asked for it explicitly.
	implied := newExecutorServeCmd()
	c.False(skillsSyncEnabled(implied, false, []string{"claude"}),
		"the --launch claude implication must be off for a non-default instance with no CLAUDE_CONFIG_DIR")
	c.NoError(refuseSkillsSyncForNonDefaultInstance(implied, false), "an explicit --skills-sync=false is never refused")

	// An instance with its own corpus is unaffected, in both directions.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	c.NoError(refuseSkillsSyncForNonDefaultInstance(explicit, true),
		"an instance with its own CLAUDE_CONFIG_DIR may sync")
	c.True(skillsSyncEnabled(implied, false, []string{"claude"}),
		"an instance with its own CLAUDE_CONFIG_DIR takes the --launch claude implication")

	// And the default instance always may: it is the one that owns the shared dir.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	c.NoError(paths.SetInstance(paths.DefaultInstance), "SetInstance(DefaultInstance)")
	c.NoError(refuseSkillsSyncForNonDefaultInstance(explicit, true), "the default instance may always sync")
	c.True(skillsSyncEnabled(implied, false, []string{"claude"}),
		"the default instance takes the --launch claude implication")
}

// seedInstanceMachineName writes this machine's executor name for ONE instance.
// The name file follows DataDir, so two instances on one OS user can name the
// same box differently — which is what makes the instance a name was read under
// observable.
func seedInstanceMachineName(t *testing.T, instance, name string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.NoError(paths.PinInstance(instance), "PinInstance(%q)", instance)
	c.NoError(paths.SetMachineName(name), "SetMachineName(%q) for instance %q", name, instance)
}

// `rafiki service refresh` restarts the installed EXECUTOR unit, whose name,
// label and log path are all per instance. The instance it acts on is the
// DECLARED one — -P/--profile, else a non-empty $RAFIKI_PROFILE, else default —
// never the current-profile pointer: the pointer is a client's convenience, and
// a refresh that followed it would restart a service the operator did not name
// while leaving the one they did untouched (`-P work` is the only way to name a
// per-instance unit at all).
func TestExecutorInstanceServiceRefreshUsesTheDeclaredInstance(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	resetInstance(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	seedPointerProfiles(t)
	c.NoError(profile.SavePointer("work"), "point the current-profile pointer at work")

	// The daemon's unit is not per instance; leave it uninstalled so the refresh
	// does the executor half only.
	stubServiceBackend(t, &fakeServiceBackend{})

	self, err := os.Executable()
	c.NoError(err, "os.Executable")

	// One entry per executor-backend resolution: the instance it was resolved
	// under, and the log file that instance's unit writes. The unit name itself
	// is a pure function of the same instance (TestExecutorInstanceUnitNames).
	var resolved []string
	orig := executorServiceBackendFor
	t.Cleanup(func() { executorServiceBackendFor = orig })
	fake := &fakeServiceBackend{status: serviceStatus{Installed: true, Binary: self}}
	executorServiceBackendFor = func() serviceBackend {
		resolved = append(resolved, paths.Instance()+" "+paths.ExecutorServiceLogPath())
		return fake
	}
	// refreshServices resolves a backend twice (Status, then the restart).
	both := func(instance, logFile string) []string {
		one := instance + " " + filepath.Join(paths.StateDir(), logFile)
		return []string{one, one}
	}

	// No -P, with the pointer's instance already in place — exactly what a caller
	// that resolved the pointer leaves behind. The DEFAULT instance's unit is the
	// one to inspect and restart.
	verb := executorVerb(c, newServiceRefreshCmd)
	p, err := resolveProfile(verb)
	c.NoError(err, "resolveProfile")
	c.Eq("work", p.Name, "the pointer selects the profile")
	c.Eq("work", paths.Instance(), "an ordinary profile resolution takes the pointer's instance")

	c.NoError(refreshServices(context.Background(), verb, newFakeRunner(), io.Discard), "refresh with no -P")
	c.True(fake.restarted, "the executor service must be restarted")
	c.EqDeep(both(paths.DefaultInstance, "executor.log"), resolved,
		"every executor backend resolution must be the default instance's, not the pointer's")

	// A declared -P names the work instance's unit and log, which is the only way
	// to reach a per-instance service at all.
	resolved, fake.restarted = nil, false
	resetProfileCache()
	c.NoError(refreshServices(context.Background(), executorVerb(c, newServiceRefreshCmd, "-P", "work"),
		newFakeRunner(), io.Discard), "refresh with -P work")
	c.True(fake.restarted, "the work executor service must be restarted")
	c.EqDeep(both("work", "executor-work.log"), resolved,
		"-P work must resolve the work instance's unit and log")
}

// `rafiki create` reads this machine's executor NAME to fill a child's home, and
// it can hand off to `executor setup`, which installs a system service. Both
// describe the DECLARED instance — -P, else $RAFIKI_PROFILE, else default — so a
// pointer naming another profile cannot make the name create reports and the
// unit setup installs come from different instances.
func TestExecutorInstanceCreateReadsTheDeclaredInstanceMachineName(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetInstance(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	t.Setenv(paths.ExecutorName, "") // the per-instance FILE decides, not the env

	// Two instances, two names: which one create reads is the whole question.
	seedInstanceMachineName(t, paths.DefaultInstance, "default-box")
	seedInstanceMachineName(t, "work", "work-box")
	paths.ResetInstanceForTest()

	stub := &createStubControl{executors: []*rafikiv1.ExecutorRow{
		executorRow("default-box", true, map[string]string{"owner": "brent"}),
		executorRow("work-box", true, map[string]string{"owner": "brent"}),
	}}
	serveCreateStub(t, stub)

	// Name the pointer's profile "work" and serve it on the same socket, so the
	// pointer's instance is the one whose machine name must NOT be reported.
	set, err := profile.Load()
	c.NoError(err, "profile.Load")
	sock := set.Profiles["scratch"].Socket
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: sock},
	}}), "save the work profile")
	c.NoError(profile.SavePointer("work"), "point at work")
	resetProfileCache()

	cmd := createCmdForRun(t, "--detached", "--executor-selector", "owner=brent")
	c.Require().NoError(runCreate(cmd, nil), "runCreate")
	c.Require().NotNil(stub.spawned, "no Spawn reached the daemon")
	c.Eq("default-box", stub.spawned.GetHomeExecutor(),
		"create must name this machine from the default instance, not the pointer's")
}

// `rafiki upgrade` execs `service refresh`, so the instance that child refreshes
// has to be the operator's DECLARED one: `rafiki -P work upgrade` must bring the
// work unit onto the new binary, while with no -P the default unit is refreshed
// exactly as before. The value crosses the exec boundary RAW — the child derives
// its own instance from it (applyExecutorInstance), so a current-profile pointer
// cannot decide which unit an upgrade restarts.
func TestExecutorInstanceUpgradePassesTheDeclaredProfileToServiceRefresh(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	seedPointerProfiles(t) // the pointer must not be what crosses the boundary
	c.Eq("personal", profile.LoadPointer(), "the pointer names another profile")

	// -P work: the child is told the instance the operator named, not the
	// pointer's.
	cmd := executorVerb(c, newUpgradeCmd, "-P", "work")
	wired, err := upgradeDepsFromCmd(cmd)
	c.NoError(err, "upgradeDepsFromCmd with -P work")
	c.Eq("work", wired.Profile, "the RAW -P value must cross the exec boundary")

	tr := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r := newFakeRunner()
	vr := versionRunner{fakeRunner: r, versionTag: "v0.2.0"}
	dir := oldInstall(t)
	self := filepath.Join(dir, "rafiki")
	deps, _, _ := upgradeDepsFor(t, tr, vr, self, "v0.1.0", t.TempDir())
	deps.Profile = wired.Profile
	c.Require().NoError(runUpgrade(context.Background(), deps, upgradeOpts{}), "upgrade with -P work")
	c.True(sawRun(r, self, "-P", "work", "service", "refresh"),
		"the refresh child must be told the instance the operator named; runs: %v", r.runCalls)

	// No -P: nothing is added, so the child refreshes the default unit.
	plain, err := upgradeDepsFromCmd(executorVerb(c, newUpgradeCmd))
	c.NoError(err, "upgradeDepsFromCmd with no -P")
	c.Eq("", plain.Profile, "no -P must not cross the exec boundary")

	tr2 := newTestRelease(t, "v0.2.0", runtime.GOOS, runtime.GOARCH, testReleaseOpts{})
	r2 := newFakeRunner()
	vr2 := versionRunner{fakeRunner: r2, versionTag: "v0.2.0"}
	dir2 := oldInstall(t)
	self2 := filepath.Join(dir2, "rafiki")
	deps2, _, _ := upgradeDepsFor(t, tr2, vr2, self2, "v0.1.0", t.TempDir())
	deps2.Profile = plain.Profile
	c.Require().NoError(runUpgrade(context.Background(), deps2, upgradeOpts{}), "upgrade with no -P")
	c.True(sawRun(r2, self2, "service", "refresh"),
		"with no -P the refresh child takes no instance flag; runs: %v", r2.runCalls)
	c.EqDeep([]string{"service", "refresh"}, refreshArgv(""), "no -P means no flag on the child's argv")
}
