package embedpg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// fakePgCtl writes a pg_ctl at s.BinDir/bin that appends its arguments to a log
// file and then runs body. The body sees "$1" (the verb) and "$@" as a real
// pg_ctl would. The environment is NOT a channel here: verifiable subprocess
// environ is limited to the whitelist in execEnv, so a script can only be told
// what to do by its own text.
func fakePgCtl(t *testing.T, body string) (Server, string) {
	t.Helper()
	s := testServer(t)
	bin := filepath.Join(s.BinDir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	log := filepath.Join(s.BinDir, "pg_ctl.calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "pg_ctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write pg_ctl: %v", err)
	}
	return s, log
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

// pgCtlStopNoServer is what the PINNED pg_ctl (PostgreSQL 18.3) really prints for
// `pg_ctl stop` on a cluster that is not running, copied from a real run against
// an initdb'd, stopped data directory:
//
//	pg_ctl: PID file "/tmp/rfstop/pg/postmaster.pid" does not exist
//	Is server running?
//
// and it exits 1. "no server running" — which an earlier version of Stop matched —
// is what STATUS prints, with exit 3. That is the whole reason the idempotence
// answer comes from Status and not from this text.
const pgCtlStopNoServer = `dir=""
prev=""
for a in "$@"; do
	if [ "$prev" = "-D" ]; then dir="$a"; fi
	prev="$a"
done
echo "pg_ctl: PID file \"$dir/postmaster.pid\" does not exist" >&2
echo "Is server running?" >&2
exit 1`

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

// Stop waits for a "fast" shutdown, bounded, and answers "the server is down"
// with success — including when pg_ctl itself reports failure, because the
// pinned 18.3 reports an already-stopped cluster as an error whose text is not
// the string its status prints.
func TestStopIsBoundedAndIdempotent(t *testing.T) {
	c := assert.NewAborting(t)

	s, log := fakePgCtl(t, "exit 0")
	c.NoError(s.Stop(context.Background()), "Stop returned error for a clean shutdown")
	calls := pgCtlCalls(t, log)
	c.Eq(1, len(calls), "pg_ctl must be called once, got: %v", calls)
	c.Eq("stop -m fast -w -t 60 -D "+s.DataDir, calls[0], "pg_ctl argv, got")
	c.True(strings.Contains(calls[0], "-w"), "Stop must WAIT for the postmaster to exit, got: %s", calls[0])

	// A cluster that is not running: pg_ctl fails (exit 1, the real text above)
	// and status says not running (exit 3). That is the state the caller wanted.
	s, _ = fakePgCtl(t, `case "$1" in
status) exit 3 ;;
stop) `+pgCtlStopNoServer+` ;;
esac`)
	c.NoError(s.Stop(context.Background()), "a server that is not running is not an error")

	// A stop that failed while the server is still up is a failure worth
	// reporting: status says running, so the caller must hear about it.
	s, _ = fakePgCtl(t, `case "$1" in
status) exit 0 ;;
stop) echo "pg_ctl: server does not shut down" >&2; exit 1 ;;
esac`)
	err := s.Stop(context.Background())
	c.Error(err, "want an error for a failed shutdown, got nil")
	c.StrContains(err.Error(), "stop failed", "error, got: %v", err)
	c.StrContains(err.Error(), "does not shut down", "the failure must carry pg_ctl's own message, got: %v", err)

	// If even the status check fails, the stop error is still the headline and
	// the status failure rides along rather than being lost.
	s, _ = fakePgCtl(t, `case "$1" in
status) echo "pg_ctl: cannot stat the directory" >&2; exit 4 ;;
stop) echo "pg_ctl: bad things" >&2; exit 1 ;;
esac`)
	err = s.Stop(context.Background())
	c.Error(err, "want an error when status cannot answer, got nil")
	c.StrContains(err.Error(), "bad things", "error, got: %v", err)
	c.StrContains(err.Error(), "cannot stat the directory", "the status failure must be named too, got: %v", err)
}

// Stop returns the lock before it returns: two Stops in a row must not deadlock
// against the first one's flock.
func TestStopReleasesTheLock(t *testing.T) {
	s, _ := fakePgCtl(t, "exit 0")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := assert.NewAborting(t)
	c.NoError(s.Stop(ctx), "first Stop")
	c.NoError(s.Stop(ctx), "second Stop must not wait on the first one's lock")
}

// StopLocked exists for a caller that already holds Lock — a physical backup,
// which must hold it across stop → copy → start. Taking it again there would
// deadlock the process against itself, so this pins that it does not.
func TestStopLockedRunsUnderAHeldLock(t *testing.T) {
	s, _ := fakePgCtl(t, "exit 0")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := assert.NewAborting(t)
	release, err := s.Lock(ctx)
	c.NoError(err, "Lock")
	defer release()

	c.NoError(s.StopLocked(ctx), "StopLocked under a held Lock (a deadlock would show as a deadline here)")
}

// Stop is a state change, so it takes the flock every other one takes: a held
// lock must make Stop wait (and give up when its context does), not quietly stop
// a server another process is starting.
func TestStopTakesTheLock(t *testing.T) {
	s, _ := fakePgCtl(t, "exit 0")
	holder, err := os.OpenFile(s.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("Flock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = s.Stop(ctx)
	elapsed := time.Since(start)

	c := assert.NewAborting(t)
	c.ErrorIs(err, context.Canceled)
	c.Less(2*time.Second, elapsed, "Stop waited %s behind a held lock", elapsed)
}

// Lock's release is idempotent, because the callers that hold the lock for a
// scope (a backup's defer, a test's defer) may release on more than one path.
func TestLockReleaseIsIdempotent(t *testing.T) {
	s := testServer(t)
	c := assert.NewAborting(t)

	release, err := s.Lock(context.Background())
	c.NoError(err, "Lock")
	release()
	release()

	// The lock is really free again: a second Lock succeeds promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release2, err := s.Lock(ctx)
	c.NoError(err, "a second Lock after a double release")
	release2()
}

// The stop of a stopped cluster against the REAL pg_ctl, which is the only thing
// that can prove the idempotence rule against the wording this package does not
// match. Gated like the other real-server test: the first run downloads the
// pinned binaries.
func TestStopIsIdempotentOnARealCluster(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs this against a real embedded server (downloads the pinned binaries once)")
	}
	c := assert.NewAborting(t)

	// os.UserCacheDir() must be read BEFORE XDG_CACHE_HOME is pointed at the
	// scratch tree, or the binaries would be looked for in the tree it is about
	// to name.
	cache, err := os.UserCacheDir()
	c.NoError(err, "UserCacheDir")
	root, err := os.MkdirTemp("/tmp", "epg") // short: the socket lives in the data dir
	c.NoError(err, "MkdirTemp")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(cache, "rafiki-test-embedpg"))

	s := Default()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := s.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Stop(context.Background())
		if err := os.RemoveAll(root); err != nil {
			t.Logf("remove %s: %v", root, err)
		}
	})

	c.NoError(s.Stop(ctx), "stopping a running server")
	c.NoError(s.Stop(ctx), "a stop of a stopped server must be success against the real pg_ctl")
	running, err := s.Status(ctx)
	c.NoError(err, "Status after two stops")
	c.False(running, "the server must be down")

	// And it can come back: Ensure is what a backup's restart calls.
	if _, err := s.Ensure(ctx); err != nil {
		t.Fatalf("Ensure after a stop: %v", err)
	}
}

// EnsureLocked is what a caller that already holds Lock calls — a physical
// restore holds it across stop → rename → start, and Ensure taking the flock a
// second time from one process would deadlock against itself. The fixture is a
// server that is already up: no download (bin/postgres exists), no initdb
// (PG_VERSION exists), and pg_ctl status exits 0, so it never starts anything.
func TestEnsureLockedRunsUnderAHeldLock(t *testing.T) {
	// A short path: the socket file has to fit in sun_path.
	dir, err := os.MkdirTemp("/tmp", "epg")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	s := Server{
		BinDir:   filepath.Join(dir, "bin"),
		DataDir:  filepath.Join(dir, "data"),
		LogPath:  filepath.Join(dir, "pg.log"),
		LockPath: filepath.Join(dir, "pg.lock"),
	}
	for _, binary := range []string{"postgres", "pg_ctl"} {
		if err := os.MkdirAll(filepath.Join(s.BinDir, "bin"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(s.BinDir, "bin", binary), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", binary, err)
		}
	}
	if err := os.MkdirAll(s.DataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.DataDir, "PG_VERSION"), []byte(MajorVersion+"\n"), 0o600); err != nil {
		t.Fatalf("write PG_VERSION: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.DataDir, "postgresql.conf"), []byte("# as initdb left it\n"), 0o600); err != nil {
		t.Fatalf("write postgresql.conf: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := assert.NewAborting(t)

	release, err := s.Lock(ctx)
	c.NoError(err, "Lock")
	defer release()

	dsn, err := s.EnsureLocked(ctx)
	c.NoError(err, "EnsureLocked under a held Lock (a deadlock would show as the deadline here)")
	c.Eq(s.DSN(), dsn, "EnsureLocked's DSN")
}
