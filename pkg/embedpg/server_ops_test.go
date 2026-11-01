package embedpg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// fakePgCtl writes a pg_ctl at s.BinDir/bin that appends its arguments to a log
// file and then runs body. The environment is NOT a channel here: verifiable
// subprocess environ is limited to the whitelist in execEnv, so a script can
// only be told what to do by its own text.
func fakePgCtl(t *testing.T, body string) (Server, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	log := filepath.Join(dir, "pg_ctl.calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "pg_ctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write pg_ctl: %v", err)
	}
	return Server{BinDir: dir, DataDir: filepath.Join(dir, "pg")}, log
}

// shellQuote single-quotes a path for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pgCtlCalls returns the argument lines the fake recorded.
func pgCtlCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// Status is the exported question a caller asks BEFORE deciding to stop the
// server, so it must never start one: pg_ctl's own exit codes are the answer —
// 0 running, 3 not running, anything else a failure worth reporting.
func TestStatusReportsPgCtlExitCodes(t *testing.T) {
	c := assert.NewAborting(t)

	s, log := fakePgCtl(t, "exit 0")
	running, err := s.Status(context.Background())
	c.NoError(err, "Status returned error for exit 0")
	c.True(running, "exit 0 must mean running")
	c.EqDeep([]string{"status -D " + s.DataDir}, pgCtlCalls(t, log), "pg_ctl argv, got")

	s, _ = fakePgCtl(t, "exit 3")
	running, err = s.Status(context.Background())
	c.NoError(err, "exit 3 is \"not running\", not a failure")
	c.False(running, "exit 3 must mean not running")

	s, _ = fakePgCtl(t, `echo "pg_ctl: cannot stat the directory" >&2
exit 4`)
	_, err = s.Status(context.Background())
	c.Error(err, "want an error for an unexpected exit code, got nil")
	c.StrContains(err.Error(), "status failed", "error, got: %v", err)
	c.StrContains(err.Error(), "cannot stat the directory", "the failure must carry pg_ctl's own message, got: %v", err)
}

// Stop waits for a "fast" shutdown, bounded, and treats "nothing to stop" as
// success: a caller that already knows the server is down must not be handed an
// error for reaching the state it wanted.
func TestStopIsBoundedAndIdempotent(t *testing.T) {
	c := assert.NewAborting(t)

	s, log := fakePgCtl(t, "exit 0")
	c.NoError(s.Stop(context.Background()), "Stop returned error for a clean shutdown")
	calls := pgCtlCalls(t, log)
	c.Eq(1, len(calls), "pg_ctl must be called once, got: %v", calls)
	c.Eq("stop -m fast -w -t 60 -D "+s.DataDir, calls[0], "pg_ctl argv, got")
	c.True(strings.Contains(calls[0], "-w"), "Stop must WAIT for the postmaster to exit, got: %s", calls[0])

	// pg_ctl reports an already-stopped server as an ordinary failure.
	s, _ = fakePgCtl(t, `echo "pg_ctl: no server running" >&2
exit 1`)
	c.NoError(s.Stop(context.Background()), "a server that is not running is not an error")

	s, _ = fakePgCtl(t, `echo "pg_ctl: cannot stop server" >&2
exit 1`)
	err := s.Stop(context.Background())
	c.Error(err, "want an error for a failed shutdown, got nil")
	c.StrContains(err.Error(), "stop failed", "error, got: %v", err)
	c.StrContains(err.Error(), "cannot stop server", "the failure must carry pg_ctl's own message, got: %v", err)
}
