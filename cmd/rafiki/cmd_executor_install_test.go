package main

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
)

// runExecutorCLIErr executes one `rafiki executor …` argv through the real root
// command and returns what it wrote plus its error, for the verbs a test
// expects to REFUSE.
func runExecutorCLIErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"executor"}, args...))
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.Execute()
	return out.String(), err
}

// TestCLIExecutorInstallDefaultsViaToThisMachine pins --via's default: the
// launcher an install goes through is THIS machine's executor, so the flag may
// be omitted, and an explicit --via still wins. A machine with no name omits
// the field entirely rather than guessing — the daemon then picks the caller's
// single native launcher, which is the only honest answer for a box that was
// never named.
func TestCLIExecutorInstallDefaultsViaToThisMachine(t *testing.T) {
	c := assert.NewAborting(t)
	srv := &executorStubControl{installRow: &rafikiv1.ExecutorRow{
		Id: "exec-9", Machine: "ublk-s2", InstalledVia: "laptop",
		InstalledTarget: "brent@ublk-s2", Enabled: true, Connected: true,
	}}
	executorTestDaemon(t, srv)

	t.Run("the machine's own name fills in an omitted --via", func(t *testing.T) {
		t.Setenv(paths.ExecutorName, "laptop")
		out := runExecutorCLI(t, "install", "brent@ublk-s2", "--name", "ublk-s2", "--root", "/src",
			"--label", "env=prod", "--admits", "owner=brent")
		c.Require().NotNil(srv.sawInstall, "the CLI sent no InstallExecutor request")
		c.Eq("brent@ublk-s2", srv.sawInstall.GetTarget(), "target")
		c.Eq("laptop", srv.sawInstall.GetVia(), "via defaults to this machine's executor name")
		c.Eq("ublk-s2", srv.sawInstall.GetName(), "name")
		c.Eq("/src", srv.sawInstall.GetRoot(), "root")
		c.Eq("owner=brent", srv.sawInstall.GetAdmits(), "admits")
		c.EqDeep(map[string]string{"env": "prod"}, srv.sawInstall.GetLabels(), "labels")
		// The row the daemon answered with is the row the table renders, so the
		// operator sees the id, the machine and WHERE it came from.
		for _, want := range []string{"exec-9", "ublk-s2", "laptop", "VIA"} {
			c.StrContains(out, want, "install output missing")
		}
	})

	t.Run("an explicit --via wins over this machine's name", func(t *testing.T) {
		t.Setenv(paths.ExecutorName, "laptop")
		runExecutorCLI(t, "install", "brent@ublk-s2", "--via", "rack")
		c.Require().NotNil(srv.sawInstall, "the CLI sent no InstallExecutor request")
		c.Eq("rack", srv.sawInstall.GetVia(), "an explicit --via must not be overridden")
	})

	t.Run("a machine with no name omits --via rather than guessing", func(t *testing.T) {
		// No env var and no name file: paths.MachineName fails, and the field
		// is left for the daemon to resolve.
		t.Setenv(paths.ExecutorName, "")
		out, err := runExecutorCLIErr(t, "install", "brent@ublk-s2")
		c.NoError(err, "an unnamed machine must still be able to install; output:\n%s", out)
		c.Require().NotNil(srv.sawInstall, "the CLI sent no InstallExecutor request")
		c.Eq("", srv.sawInstall.GetVia(), "an unreadable machine name must leave via empty")
	})

	t.Run("-j emits the row as canonical protojson", func(t *testing.T) {
		t.Setenv(paths.ExecutorName, "laptop")
		out := runExecutorCLI(t, "install", "brent@ublk-s2", "-j")
		for _, want := range []string{`"id": "exec-9"`, `"machine": "ublk-s2"`, `"installedVia": "laptop"`, `"installedTarget": "brent@ublk-s2"`} {
			c.StrContains(out, want, "json install output missing")
		}
	})
}

// A refused install must reach the operator as the daemon's own message,
// because that message is where ssh's stderr travels: an invented summary would
// delete the one clue that says what to fix on the via machine.
func TestCLIExecutorInstallSurfacesTheDaemonRefusal(t *testing.T) {
	c := assert.NewAborting(t)
	// The daemon refuses a target whose host key does not verify, carrying
	// ssh's own stderr as the message.
	srv := &executorStubControl{installErr: connect.NewError(connect.CodeFailedPrecondition,
		errors.New("probe ublk-s2 failed: Host key verification failed."))}
	executorTestDaemon(t, srv)
	t.Setenv(paths.ExecutorName, "laptop")

	out, err := runExecutorCLIErr(t, "install", "brent@ublk-s2")
	c.Require().Error(err, "a refused install must fail the command; output:\n%s", out)
	c.StrContains(err.Error(), "Host key verification failed", "ssh's message must reach the operator, got: %v", err)
}

// `rafiki executor uninstall <name|id>` passes the ref through verbatim: the
// daemon is what resolves a name, an id or a trailing fragment, so the verb
// must not pre-resolve anything of its own.
func TestCLIExecutorInstallUninstallPassesTheRefThrough(t *testing.T) {
	c := assert.NewAborting(t)
	srv := &executorStubControl{}
	executorTestDaemon(t, srv)

	out := runExecutorCLI(t, "uninstall", "ublk-s2")
	c.EqDeep([]string{"ublk-s2"}, srv.sawUninstall, "the ref handed to the daemon")
	c.StrContains(out, "uninstalled", "the verb must confirm what it removed")
}

// A refused uninstall reaches the operator as the daemon's own message: it is
// where "this was enrolled, not installed" and "that belongs to another user"
// travel, and both are decisions the operator acts on.
func TestCLIExecutorInstallUninstallSurfacesTheRefusal(t *testing.T) {
	c := assert.NewAborting(t)
	srv := &executorStubControl{uninstallErr: connect.NewError(connect.CodeFailedPrecondition,
		errors.New("executor ublk-s2 was not installed over ssh"))}
	executorTestDaemon(t, srv)

	out, err := runExecutorCLIErr(t, "uninstall", "ublk-s2")
	c.Require().Error(err, "a refused uninstall must fail the command; output:\n%s", out)
	c.StrContains(err.Error(), "was not installed over ssh", "the daemon's message must reach the operator, got: %v", err)
}

// TestCLIExecutorLsShowsVia is `executor ls`'s VIA column: where an installed
// executor came from is the fact an operator needs to tell two rows of the same
// machine apart, and a dash (never a blank cell) marks a row that was enrolled
// rather than installed.
func TestCLIExecutorLsShowsVia(t *testing.T) {
	c := assert.NewAborting(t)
	srv := &executorStubControl{rows: []*rafikiv1.ExecutorRow{
		{Id: fixturePrefix + fixtureTailA, Machine: "ublk-s2", InstalledVia: "laptop",
			InstalledTarget: "brent@ublk-s2", Enabled: true, Connected: true},
		{Id: fixturePrefix + fixtureTailB, Machine: "rack", Enabled: true, Connected: true},
	}}
	executorTestDaemon(t, srv)

	out := runExecutorCLI(t, "list")
	for _, header := range []string{"ID", "MACHINE", "VIA", "STATUS", "LABELS", "ADMITS", "CONNECTED", "LAST SEEN"} {
		c.StrContains(out, header, "missing header")
	}
	c.StrContains(out, "laptop", "the installed row's launcher must show under VIA:\n"+out)
	c.StrContains(out, "-", "a row that was not installed must render VIA as a dash:\n"+out)

	// JSON carries the field itself, which is what a script reads.
	jout := runExecutorCLI(t, "list", "-j")
	c.StrContains(jout, `"installedVia": "laptop"`, "json output missing installedVia")
}

// TestCLIExecutorInstallLsShowsVia is the -run-visible shim for
// TestCLIExecutorLsShowsVia. `go test -run` is an unanchored SUBSTRING match, so
// the pinned name is invisible to a filter that selects TestCLIExecutorInstall;
// this name carries the body into that filter without renaming it.
func TestCLIExecutorInstallLsShowsVia(t *testing.T) {
	t.Run("ls renders the VIA column", TestCLIExecutorLsShowsVia)
}
