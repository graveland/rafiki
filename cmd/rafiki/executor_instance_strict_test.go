// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// stubExecutorBackendCalls records every resolution of the executor service
// backend. A refused verb must never reach one: that is what "no uninstall, no
// install, no start" means at this seam, because the backend is what touches the
// unit, the credential and the service manager.
func stubExecutorBackendCalls(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := executorServiceBackendFor
	executorServiceBackendFor = func() serviceBackend {
		calls = append(calls, "backend")
		return &fakeExecutorBackend{}
	}
	t.Cleanup(func() { executorServiceBackendFor = orig })
	return &calls
}

// `rafiki -P "my work" executor service uninstall` must NOT remove the default
// unit: the fallback is safe for a verb that only names paths, and uninstall is
// the verb where it is worst. The refusal names the declared value and lands
// before the backend — and therefore before any service manager — is resolved.
func TestExecutorInstanceServiceUninstallRefusesAnInvalidDeclaredName(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	resetInstance(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	calls := stubExecutorBackendCalls(t)

	cmd := executorVerb(c, newExecutorServiceUninstallCmd, "-P", "my work")
	err := cmd.RunE(cmd, nil)

	c.Require().Error(err, "an invalid declared instance name must refuse an uninstall")
	c.StrContains(err.Error(), `"my work"`, "the refusal names the offending name")
	c.StrContains(err.Error(), "cannot name an executor instance",
		"the refusal says the name is not a valid instance name")
	c.Eq(0, len(*calls), "no backend may be resolved: the default service must not be uninstalled")
	c.Eq(paths.DefaultInstance, paths.Instance(), "a refusal must not fall back to the default instance")
}

// The other direction, so the strict switch is pinned both ways: a VALID
// declared name must still reach the backend, for the instance it names. A verb
// that refused every uninstall would satisfy the test above and take away the
// only way to remove a per-instance unit.
func TestExecutorInstanceServiceUninstallActsOnTheDeclaredInstance(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	resetInstance(t)
	calls := stubExecutorBackendCalls(t)

	cmd := executorVerb(c, newExecutorServiceUninstallCmd, "-P", "work")
	c.NoError(cmd.RunE(cmd, nil), "a valid declared instance name must uninstall")
	c.Eq(1, len(*calls), "exactly one backend resolution")
	c.Eq("work", paths.Instance(), "the uninstall must act on the declared instance")
}

// `rafiki -P "my work" executor setup` must NOT overwrite the default
// credential, environment file and unit: it refuses before the first write, and
// before the enrollment exchange that would mint a token for the wrong instance.
func TestExecutorInstanceSetupRefusesAnInvalidDeclaredName(t *testing.T) {
	c := assert.NewAborting(t)
	isolateSetup(t)
	resetProfileCache()
	resetInstance(t)
	t.Setenv(paths.ExecutorEnvFileEnv, "") // let the instance name the file
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	calls := stubExecutorBackendCalls(t)

	// Minting would mean the refusal came after the flow had started working.
	origEnroll := executorEnrollClient
	executorEnrollClient = func(context.Context, *cobra.Command, string, []string) (string, error) {
		t.Error("executor setup minted an enrollment token: the refusal came too late")
		return "", errors.New("unexpected enrollment")
	}
	t.Cleanup(func() { executorEnrollClient = origEnroll })

	cmd := executorVerb(c, newExecutorSetupCmd, "-P", "my work")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)
	var out strings.Builder
	w := newWizard(cmd)
	w.out = &out
	err := runExecutorSetup(ctx, w, &out)

	c.Require().Error(err, "an invalid declared instance name must refuse executor setup")
	c.StrContains(err.Error(), `"my work"`, "the refusal names the offending name")
	c.StrContains(err.Error(), "cannot name an executor instance",
		"the refusal says the name is not a valid instance name")
	c.Eq(0, len(*calls), "no unit may be installed")
	_, envErr := os.Stat(paths.ExecutorEnvFile())
	c.True(os.IsNotExist(envErr), "no environment file may be written: %v", envErr)
	_, credErr := os.Stat(filepath.Join(dataHome, "rafiki", "executor.cred"))
	c.True(os.IsNotExist(credErr), "no credential file may be written: %v", credErr)
}

// `rafiki -P "my work" create` only NAMES paths — this machine's executor name
// and the home a child is spawned with — so it keeps working: it warns once on
// stderr and falls back to the default instance, which is the layout that name's
// install already had.
func TestExecutorInstanceCreateWarnsAndFallsBackOnAnInvalidDeclaredName(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetInstance(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	t.Setenv(paths.ExecutorName, "") // the per-instance FILE decides, not the env
	read := captureStderr(t)

	stub := &createStubControl{executors: []*rafikiv1.ExecutorRow{
		executorRow("default-box", true, map[string]string{"owner": "brent"}),
	}}
	serveCreateStub(t, stub)

	// A profile really named "my work" (pkg/profile allows the space), so both
	// applyExecutorInstance and resolveProfile notice the mistake in this one
	// command — and it is reported once, because a second identical line reads
	// as a bug.
	set, err := profile.Load()
	c.NoError(err, "profile.Load")
	sock := set.Profiles["scratch"].Socket
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
		"my work": {Name: "my work", Socket: sock},
	}}), "save a profile named like the bad instance")
	c.NoError(profile.SavePointer("scratch"), "point at scratch")
	resetProfileCache()
	seedInstanceMachineName(t, paths.DefaultInstance, "default-box")
	paths.ResetInstanceForTest()

	cmd := executorVerb(c, newCreateCmd, "-P", "my work", "--detached", "--executor-selector", "owner=brent")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	c.Require().NoError(runCreate(cmd, nil), "create must not fail on an unusable instance name")

	c.Require().NotNil(stub.spawned, "no Spawn reached the daemon")
	c.Eq("default-box", stub.spawned.GetHomeExecutor(),
		"create reads the default instance's machine name, not a refused one")
	c.Eq(paths.DefaultInstance, paths.Instance(), "the fallback is the default instance")
	c.Eq(1, strings.Count(read(), "cannot name an executor instance"),
		"exactly one warning line for one mistake")
}

// `rafiki upgrade` swaps the binaries before it refreshes services, so its
// declared name is validated BEFORE the first subprocess and before the swap: a
// name that cannot name an instance fails with nothing changed, and never falls
// back to the default instance (whose unit it would then restart).
func TestExecutorInstanceUpgradeRefusesAnInvalidDeclaredNameBeforeTheSwap(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	resetInstance(t)
	seedPointerProfiles(t) // the pointer must not be what crosses the boundary

	self, err := os.Executable()
	c.NoError(err, "os.Executable")
	before, err := os.ReadFile(self)
	c.NoError(err, "read the installed binary")

	deps, err := upgradeDepsFromCmd(executorVerb(c, newUpgradeCmd, "-P", "my work"))
	c.Require().Error(err, "an invalid declared instance name must refuse an upgrade")
	c.StrContains(err.Error(), `"my work"`, "the refusal names the offending name")
	c.StrContains(err.Error(), "cannot name an executor instance",
		"the refusal says the name is not a valid instance name")
	// The deps are the zero value: the refusal lands while they are built, which
	// is before the smoke check (the first subprocess) and before the swap.
	c.True(deps.Runner == nil, "no runner may be wired: no step can have run")
	c.Eq("", deps.Exe, "the target binary was never even resolved")

	// The full command path refuses too, and the binary on disk is untouched —
	// the recorded step order is empty because there are no steps.
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.AddCommand(newUpgradeCmd())
	root.SetArgs([]string{"-P", "my work", "upgrade"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	c.Require().Error(root.Execute(), "the cobra path must refuse as well")

	after, err := os.ReadFile(self)
	c.NoError(err, "re-read the installed binary")
	// bytes.Equal, not EqDeep: this is a ~70 MB test binary, and a deep
	// comparison of it costs tens of seconds.
	c.True(bytes.Equal(before, after), "the swap never ran: the binary is byte-identical")
}
