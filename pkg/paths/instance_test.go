package paths

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// instancePaths is every path an executor instance affects. Comparing the whole
// set is what makes "the default instance keeps today's layout" a statement
// about the files, not about one of them.
type instancePaths struct {
	DataDir, CacheDir                       string
	ExecutorEnvFile, ExecutorOverridesFile  string
	ExecutorServiceLogPath, MachineNameFile string
}

func snapshotInstancePaths() instancePaths {
	return instancePaths{
		DataDir:                DataDir(),
		CacheDir:               CacheDir(),
		ExecutorEnvFile:        ExecutorEnvFile(),
		ExecutorOverridesFile:  ExecutorOverridesFile(),
		ExecutorServiceLogPath: ExecutorServiceLogPath(),
		MachineNameFile:        MachineNameFile(),
	}
}

// isolateInstanceXDG pins every base directory to a fixed path, so the expected
// strings below are absolute and platform-independent.
func isolateInstanceXDG(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xc")
	t.Setenv("XDG_DATA_HOME", "/tmp/xd")
	t.Setenv("XDG_STATE_HOME", "/tmp/xs")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/xr")
	t.Setenv("XDG_CACHE_HOME", "/tmp/xk")
	t.Setenv(ExecutorEnvFileEnv, "")
	t.Setenv(ExecutorOverridesFileEnv, "")
	t.Cleanup(resetInstanceForTest)
}

// The default instance is every install that predates instances: its paths must
// stay byte-identical, whether the instance was never named or named explicitly.
func TestInstanceDefaultKeepsTodayPaths(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateInstanceXDG(t)

	unset := snapshotInstancePaths()
	c.EqDeep(instancePaths{
		DataDir:                "/tmp/xd/rafiki",
		CacheDir:               "/tmp/xk/rafiki",
		ExecutorEnvFile:        "/tmp/xc/rafiki/executor.env",
		ExecutorOverridesFile:  "/tmp/xc/rafiki/executor-overrides.env",
		ExecutorServiceLogPath: "/tmp/xs/rafiki/executor.log",
		MachineNameFile:        "/tmp/xd/rafiki/executor-name",
	}, unset, "paths with no instance set")

	c.NoError(SetInstance(DefaultInstance), "SetInstance(DefaultInstance)")
	c.EqDeep(unset, snapshotInstancePaths(), "an explicitly default instance must resolve exactly the paths an unset one does")
}

// A non-default instance namespaces DataDir and CacheDir under profiles/<name>
// and nothing else: the daemon's sockets and the client's own state live in
// Config/State/Runtime, which every instance on one OS user shares.
func TestInstanceNamespacesDataAndCacheOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateInstanceXDG(t)

	c.NoError(SetInstance("work"), "SetInstance(work)")

	c.Eq("/tmp/xd/rafiki/profiles/work", DataDir(), "DataDir")
	c.Eq("/tmp/xk/rafiki/profiles/work", CacheDir(), "CacheDir")
	c.Eq("/tmp/xd/rafiki/profiles/work/executor-name", MachineNameFile(), "MachineNameFile follows DataDir")
	c.Eq("/tmp/xc/rafiki/executor-work.env", ExecutorEnvFile(), "ExecutorEnvFile")
	c.Eq("/tmp/xc/rafiki/executor-overrides-work.env", ExecutorOverridesFile(), "ExecutorOverridesFile")
	c.Eq("/tmp/xs/rafiki/executor-work.log", ExecutorServiceLogPath(), "ExecutorServiceLogPath")

	// Config/State/Runtime and everything derived from them are untouched.
	c.Eq("/tmp/xc/rafiki", ConfigDir(), "ConfigDir")
	c.Eq("/tmp/xs/rafiki", StateDir(), "StateDir")
	c.Eq("/tmp/xr/rafiki", RuntimeDir(), "RuntimeDir")
	c.Eq("/tmp/xr/rafiki/controller.sock", SocketPath(), "SocketPath")
	c.Eq("/tmp/xr/rafiki/executor.sock", ExecutorSocketPath(), "ExecutorSocketPath")
	c.Eq("/tmp/xs/rafiki/client-state.json", ClientStateFile(), "ClientStateFile")
}

// A rejected name must not be adopted: SetInstance validates BEFORE storing, so
// a bad -P cannot leave the process half-namespaced.
func TestInstanceRejectsBadNames(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateInstanceXDG(t)
	c.NoError(SetInstance("work"), "SetInstance(work)")

	for _, name := range []string{"", ".", "..", "a/b", "a b", strings.Repeat("a", 65)} {
		err := SetInstance(name)
		c.Error(err, "SetInstance(%q) must be refused", name)
		c.Eq("work", Instance(), "a refused name must not replace the current instance (after %q)", name)
	}

	c.NoError(SetInstance(strings.Repeat("a", 64)), "a 64-character name is the maximum and must be accepted")
}
