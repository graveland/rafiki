package main

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/multigres/testkit/assert"
)

const reaperHelperEnv = "RAFIKI_REAPER_TEST_HELPER"

// TestSuperviseReapsAdoptedOrphans re-execs the test binary as a stand-in for
// PID 1: PR_SET_CHILD_SUBREAPER makes it adopt orphans exactly as init would,
// without needing a PID namespace.
func TestSuperviseReapsAdoptedOrphans(t *testing.T) {
	if os.Getenv(reaperHelperEnv) == "1" {
		reaperHelperMain()
		return
	}
	c := assert.NewAborting(t)
	self, err := os.Executable()
	c.NoError(err, "os.Executable")
	proc, err := os.StartProcess(self, []string{self, "-test.run=^TestSuperviseReapsAdoptedOrphans$"}, &os.ProcAttr{
		Env:   append(os.Environ(), reaperHelperEnv+"=1"),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	c.NoError(err, "start helper")
	st, err := proc.Wait()
	c.NoError(err, "wait helper")
	c.Eq(0, st.ExitCode(), "helper exit code (0 = main child exited 7 and no zombie was left)")
}

// The orphan is a grandchild whose parent exits at once, so it reparents to
// the helper; the main child then waits long enough for the orphan to exit
// and checks that nothing of the helper's is left defunct.
const reaperScript = `(true &) ; sleep 0.5
if cat /proc/[0-9]*/stat 2>/dev/null | awk -v p=$PPID '{sub(/^.*\) /,"")} $1=="Z" && $2==p {found=1} END {exit !found}'; then exit 9; fi
exit 7`

func reaperHelperMain() {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		os.Stderr.WriteString("prctl: " + err.Error() + "\n")
		os.Exit(2)
	}
	code, err := superviseAsInit("/bin/sh", []string{"sh", "-c", reaperScript})
	if err != nil {
		os.Stderr.WriteString("supervise: " + err.Error() + "\n")
		os.Exit(3)
	}
	if code != 7 {
		os.Exit(1)
	}
	os.Exit(0)
}
