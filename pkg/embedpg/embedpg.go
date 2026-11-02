// Package embedpg runs a private PostgreSQL 18 for a single-machine rafiki
// install: it downloads and verifies the server binaries, initialises a data
// directory, and starts the server listening on a unix socket only. It is
// daemon-only: cmd/rafiki must never import it.
//
// Every subprocess it starts gets a minimal environment (see command): the
// postmaster outlives the command that started it, so nothing rafikid was
// started with — RAFIKI_SECRETS_KEY above all — may be readable from it.
package embedpg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/paths"
)

// Server is one private PostgreSQL instance: where its binaries live, where its
// data directory is, and how to start it. The zero value is not usable; build
// one from Default or fill the fields in.
type Server struct {
	BinDir   string       // extracted tree root: bin/, lib/, share/
	DataDir  string       // PGDATA, mode 0700; also the unix socket directory
	LogPath  string       // server log, pg_ctl -l
	LockPath string       // flock serialising Ensure across processes
	BaseURL  string       // release download base; DefaultBaseURL when empty
	Client   *http.Client // http.DefaultClient when nil
	Progress io.Writer    // download progress lines; nil = silent
}

// Default is the per-user install: binaries in the cache (re-fetchable), data
// in the data dir (precious), the log in the state dir.
func Default() Server {
	return Server{
		BinDir:   filepath.Join(paths.CacheDir(), "embedpg", Version+"-"+runtime.GOOS+"-"+runtime.GOARCH),
		DataDir:  filepath.Join(paths.DataDir(), "pg"),
		LogPath:  filepath.Join(paths.StateDir(), "pg.log"),
		LockPath: filepath.Join(paths.DataDir(), "pg.lock"),
	}
}

// Database is the database this server is FOR: the one rafikid migrates and
// serves from. initdb creates only the maintenance database, so this one does
// not exist until something creates it — and this package opens no connection,
// so it is cmd/rafikid (resolveDSN) that does, from MaintenanceDSN. The name
// lives here because naming a connection is what this package does.
const Database = "rafiki"

// MaintenanceDatabase is the database initdb always creates. A CREATE DATABASE
// cannot run inside a transaction block and cannot run from the database it is
// creating, so it is issued from here: the one database that always exists. It
// is exported because a caller that creates Database has to name both — an
// install that already holds rafiki's schema HERE is the one a fresh, empty
// Database would silently replace.
const MaintenanceDatabase = "postgres"

// DSN is the libpq keyword/value string that reaches this server's database.
// The host is the data directory — a path, not a hostname — which is how libpq
// names a unix socket directory; sslmode=disable because a socket is not a
// network.
//
// It names Database, which on a fresh cluster does not exist yet: dial it only
// after creating it, or over MaintenanceDSN.
func (s Server) DSN() string {
	return s.conninfo(Database)
}

// MaintenanceDSN is the same connection string pointing at the maintenance
// database instead: what a caller connects to in order to CREATE the database
// DSN names. Same host, port, user and sslmode — the same server, since only
// dbname differs — so nothing here decides which database rafikid uses.
func (s Server) MaintenanceDSN() string {
	return s.conninfo(MaintenanceDatabase)
}

// conninfo is the one place this package builds a connection string, so the two
// DSNs above cannot drift apart in anything but the database name.
func (s Server) conninfo(database string) string {
	return "host=" + quoteConninfo(s.DataDir) +
		" port=" + strconv.Itoa(Port) +
		" user=rafiki dbname=" + database + " sslmode=disable"
}

// quoteConninfo quotes one libpq keyword/value value: wrapped in single quotes,
// with backslashes and single quotes escaped. libpq's parser reads a backslash
// inside single quotes as an escape, so a path containing either character
// survives the round trip through pgconn.ParseConfig.
func quoteConninfo(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "'", `\'`)
	return "'" + v + "'"
}

// Ensure makes the server ready and returns its DSN. It is safe to call from
// several processes at once: the whole body runs under an exclusive flock on
// LockPath, so a second caller waits and then finds the work already done.
//
// It starts the server and never creates Database: this package opens no
// connection, and creating a database is a connection. A caller that wants to
// dial the returned DSN creates it first (cmd/rafikid's resolveDSN).
func (s Server) Ensure(ctx context.Context) (string, error) {
	release, err := s.Lock(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return s.EnsureLocked(ctx)
}

// EnsureLocked is Ensure for a caller that ALREADY holds Lock: the same
// download, initdb, prepare and start, without taking the flock again. A
// physical restore needs it — it holds the lock across stop → rename → rename →
// start, and Ensure taking it a second time from one process would deadlock
// against itself — exactly as StopLocked is Stop's counterpart.
//
// Calling this without the lock is safe only when nobody else can touch the
// server: every other path that changes it takes the lock.
func (s Server) EnsureLocked(ctx context.Context) (string, error) {
	// The socket path is a hard limit of the platform, and it is checked before
	// anything is written or downloaded: discovering it after a 100 MB download
	// would be a wasted download on every attempt.
	if err := s.checkDataDir(); err != nil {
		return "", err
	}
	if err := s.ensureBinaries(ctx); err != nil {
		return "", err
	}
	if err := s.initIfNeeded(ctx); err != nil {
		return "", err
	}
	if err := s.checkVersion(); err != nil {
		return "", err
	}
	if err := s.prepareDataDir(); err != nil {
		return "", err
	}
	running, err := s.Status(ctx)
	if err != nil {
		return "", err
	}
	if !running {
		if err := s.start(ctx); err != nil {
			return "", err
		}
	}
	return s.DSN(), nil
}

// lockPollInterval is how long lockFile waits between non-blocking attempts at
// the flock: short enough that a cancelled caller reacts immediately, long
// enough that waiting costs nothing.
const lockPollInterval = 10 * time.Millisecond

// lockFile takes the exclusive flock on f without blocking, so a caller whose
// context is cancelled stops waiting. The lock is held for the whole of Ensure
// — a download, an initdb or a 60s pg_ctl start can each take a while — and a
// blocking flock would make that wait uninterruptible.
func lockFile(ctx context.Context, f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// command builds one embedpg subprocess. Every exec in this package goes
// through it, because the environment here is a security property and not a
// convenience: initdb starts a cluster, pg_ctl starts the postmaster, and that
// postmaster outlives the command that started it — its environment stays
// readable (ps eww, /proc/<pid>/environ) for as long as it runs, and it outlives
// every command that touches it — nothing here stops it as a side effect of
// anything. It IS stopped deliberately, by Server.StopLocked from the two
// physical verbs (`db backup --format txz|tgz` and `db restore`), which is why
// the environment still has to stay this narrow: the shutdown's own subprocess is
// short-lived, the postmaster it talks to is not. By the time Ensure runs, rafikid has already been
// started with RAFIKI_SECRETS_KEY and its provider API keys in its own
// environment, so handing a child os.Environ() would keep those alive in the one
// process that lives longest.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = execEnv()
	return cmd
}

// execEnv is the whole environment an embedpg subprocess gets: PATH, so the
// binaries resolve their own helpers, HOME, and TMPDIR and TZ when they are set.
// It is a whitelist rather than a filtered copy of os.Environ() on purpose — a
// new variable appearing in the daemon's environment cannot widen it by
// accident. TZ is not a secret and initdb reads it for the cluster's timezone,
// so dropping it would quietly give the server the system's zone instead of the
// daemon's. The cluster's locale is fixed by initdb's --locale=C, so no
// LANG/LC_* is needed, and a postmaster has no use for anything else here.
func execEnv() []string {
	env := make([]string, 0, 4)
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TZ"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// checkDataDir refuses a data directory that is not absolute. Postgres resolves
// a relative unix_socket_directories against its own data directory, so the
// socket would not be where the length check below thinks it is.
func (s Server) checkDataDir() error {
	if !filepath.IsAbs(s.DataDir) {
		return fmt.Errorf("embedpg: data dir %q is not absolute; set XDG_DATA_HOME to an absolute path", s.DataDir)
	}
	return s.checkSocketPath()
}

// socketPath is the unix socket file the server creates in the data directory.
func (s Server) socketPath() string {
	return filepath.Join(s.DataDir, ".s.PGSQL."+strconv.Itoa(Port))
}

// socketPathLimit is the longest sun_path value the platform accepts, leaving
// room for the terminating NUL: 104 bytes on macOS, 108 elsewhere.
func socketPathLimit() int {
	if runtime.GOOS == "darwin" {
		return 103
	}
	return 107
}

// checkSocketPath refuses a data directory whose socket path would not fit in
// sun_path. The remedy is the one the caller can act on: a shorter XDG_DATA_HOME.
func (s Server) checkSocketPath() error {
	sock := s.socketPath()
	limit := socketPathLimit()
	if len(sock) > limit {
		return fmt.Errorf("embedpg: socket path %s is %d bytes; the limit is %d; set XDG_DATA_HOME to a shorter directory",
			sock, len(sock), limit)
	}
	return nil
}

// initIfNeeded creates the data directory with initdb the first time. A data
// directory is built under DataDir+".init" and renamed into place, so a
// half-built one is never mistaken for a usable one, and a leftover from a
// crashed run is discarded before initdb sees it.
func (s Server) initIfNeeded(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.DataDir, "PG_VERSION")); err == nil {
		return nil
	}
	if err := s.refuseFreshInitBesideARestore(); err != nil {
		return err
	}
	staging := s.DataDir + ".init"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.DataDir), 0o700); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}

	initdb := filepath.Join(s.BinDir, "bin", "initdb")
	out, err := command(ctx, initdb,
		"-D", staging,
		"-U", "rafiki",
		"--auth-local=trust",
		"--auth-host=reject",
		"-E", "UTF8",
		"--locale=C",
	).CombinedOutput()
	if err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("embedpg: %s failed: %w: %s", initdb, err, strings.TrimSpace(string(out)))
	}

	// Everything this package owns lives in rafiki.conf, which Ensure rewrites
	// on every start; postgresql.conf stays as initdb wrote it apart from this
	// one include, so a later version of that file can be reasoned about.
	conf, err := os.OpenFile(filepath.Join(staging, "postgresql.conf"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("embedpg: %w", err)
	}
	_, werr := conf.WriteString(includeLine)
	cerr := conf.Close()
	if werr != nil || cerr != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("embedpg: %w", errors.Join(werr, cerr))
	}

	// initdb creates the directory with the umask's permissions; the data
	// directory is the access control for this server, so it is narrowed here
	// rather than left to whatever the caller's umask happens to be.
	if err := os.Chmod(staging, 0o700); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("embedpg: %w", err)
	}
	if err := os.Rename(staging, s.DataDir); err != nil {
		os.RemoveAll(staging)
		return fmt.Errorf("embedpg: %w", err)
	}
	return nil
}

// The names `rafikid db restore` leaves beside a data directory while it
// replaces it. They live here because the init path below has to recognise them,
// and a command and this package spelling them differently would be a guard that
// never fires.
const (
	// RestoreInProgressSuffix names the marker file a restore creates before it
	// moves the live directory aside and removes once the server is back up.
	RestoreInProgressSuffix = ".restore-in-progress"
	// ReplacedDataDirPrefix prefixes the directory a restore moved the previous
	// data directory to: `<data dir>.replaced-<unix>`.
	ReplacedDataDirPrefix = ".replaced-"
)

// RestoreInProgressPath is the marker `rafikid db restore` creates beside the
// data directory before it moves the live one aside, and removes once the server
// is up on the restored copy. Its presence means "between the two renames":
// there may be no data directory, and one is on its way in.
func (s Server) RestoreInProgressPath() string {
	return s.DataDir + RestoreInProgressSuffix
}

// refuseFreshInitBesideARestore refuses to initdb a missing data directory while
// a restore's marker or a replaced directory is beside it.
//
// A missing data directory next to a preserved one is never a fresh install: it
// is a restore interrupted between moving the old directory aside and putting the
// new one in place (two rename(2) calls, and nothing can run a rollback for a
// SIGKILL or a power loss there — rafikid's SIGINT/SIGTERM/SIGHUP handling
// covers only an orderly interrupt), or an operator who put a backup next to the
// install. initdb'ing an EMPTY cluster there would start a database that
// looks perfectly healthy while every row the operator has sits in the directory
// beside it — the worst outcome this package can produce — so it refuses, names
// what it found, and leaves the decision to a person.
func (s Server) refuseFreshInitBesideARestore() error {
	preserved := make([]string, 0, 2)
	if _, err := os.Lstat(s.RestoreInProgressPath()); err == nil {
		preserved = append(preserved, s.RestoreInProgressPath())
	}
	if matches, err := filepath.Glob(s.DataDir + ReplacedDataDirPrefix + "*"); err == nil {
		preserved = append(preserved, matches...)
	}
	if len(preserved) == 0 {
		return nil
	}
	return fmt.Errorf("embedpg: refusing to initialise a data directory at %s: %s is beside it. "+
		"A missing data directory next to a preserved one is an interrupted restore, not a fresh "+
		"install, and creating an empty cluster here would serve an empty database while the real "+
		"one stays on disk. Move the preserved directory back into place to use it again, or remove "+
		"it deliberately",
		s.DataDir, strings.Join(preserved, ", "))
}

// checkVersion refuses a data directory written by another major version.
// There is no upgrade path here: pg_upgrade is not something a background start
// can do, and opening an 17 cluster with 18 binaries does not work.
func (s Server) checkVersion() error {
	b, err := os.ReadFile(filepath.Join(s.DataDir, "PG_VERSION"))
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	if got := strings.TrimSpace(string(b)); got != MajorVersion {
		return fmt.Errorf("embedpg: data dir %s is PostgreSQL %s; these binaries are %s (major upgrades are not supported)",
			s.DataDir, got, MajorVersion)
	}
	return nil
}

// prepareDataDir writes everything this package owns inside a data directory
// that already exists: rafiki.conf, and the include in postgresql.conf that is
// the only way rafiki.conf takes effect. Ensure calls it on every start, after
// the version check and before the server is started, so a directory it did not
// create — an upgrade, a restored backup, one built by hand — gets the same
// treatment as one initdb made here. Without it Postgres ignores rafiki.conf
// entirely and falls back to its own defaults, which listen on TCP localhost.
//
// The 0700 mode is re-asserted here for the same reason: initIfNeeded narrows a
// directory it just created, and nothing else re-checks a mode that has drifted
// since (a copy, a restore, a chmod by hand). The data directory is the access
// control for this server — it is also the socket directory, and pg_hba is
// `trust` for local — so a group-readable one admits group members as superuser.
func (s Server) prepareDataDir() error {
	if err := os.Chmod(s.DataDir, 0o700); err != nil {
		return fmt.Errorf("embedpg: data dir %s: %w", s.DataDir, err)
	}
	if err := s.writeRafikiConf(); err != nil {
		return err
	}
	return s.ensureInclude()
}

// includeLine is the one line rafiki.conf is reachable through.
const includeLine = "\ninclude 'rafiki.conf'\n"

// writeRafikiConf writes the settings this package owns, overwriting whatever a
// previous run (or a curious operator) left there.
func (s Server) writeRafikiConf() error {
	body := "# Written by rafikid on every start; edits are overwritten.\n" +
		"listen_addresses = ''\n" +
		"port = " + strconv.Itoa(Port) + "\n" +
		"unix_socket_directories = '" + quoteConf(s.DataDir) + "'\n" +
		// The socket inherits the data directory's mode by default, but the
		// setting is what makes the guarantee independent of the directory's
		// mode on disk; 0700 is octal, as postgresql.conf reads it.
		"unix_socket_permissions = 0700\n" +
		// pg_textsearch hooks the planner at load time, so CREATE EXTENSION alone
		// leaves its BM25 index unusable. Every artifact ships it
		// (build/embedpg), and rafiki.conf is rewritten on every start, so a data
		// directory created before it was preloaded picks it up too.
		"shared_preload_libraries = 'pg_textsearch'\n"

	path := filepath.Join(s.DataDir, "rafiki.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	// WriteFile's mode applies only when it creates the file; the file holds a
	// path, so its permissions are pinned on every write.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	return nil
}

// quoteConf quotes a value for a single-quoted postgresql.conf string. Both the
// quote and the backslash are escapes inside one, so a data directory holding
// either would otherwise be read as a different path.
func quoteConf(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, "'", "''")
}

// ensureInclude makes postgresql.conf include rafiki.conf. initIfNeeded adds it
// to a data directory it just created; a directory that already has a
// PG_VERSION never went through that path, hence the scan. Idempotent, because
// prepareDataDir runs on every Ensure.
func (s Server) ensureInclude() error {
	path := filepath.Join(s.DataDir, "postgresql.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "include 'rafiki.conf'" {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	_, werr := f.WriteString(includeLine)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return fmt.Errorf("embedpg: %w", errors.Join(werr, cerr))
	}
	return nil
}

// Status reports whether the server is running, WITHOUT starting it.
//
// It is exported because a caller that has to stop the server (rafikid's
// physical backup and restore) must know whether it owes a restart, and
// Server.Ensure is the wrong question for that: Ensure STARTS a server that was
// down, so asking it would change the very thing the caller is trying not to
// change. pg_ctl's exit codes are the interface: 0 means running, 3 means not
// running, anything else is a failure worth reporting.
//
// It takes no lock: it reads the postmaster's pid file and changes nothing.
func (s Server) Status(ctx context.Context) (bool, error) {
	pgctl := filepath.Join(s.BinDir, "bin", "pg_ctl")
	out, err := command(ctx, pgctl, "status", "-D", s.DataDir).CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return false, nil
	}
	return false, fmt.Errorf("embedpg: %s status failed: %w: %s", pgctl, err, strings.TrimSpace(string(out)))
}

// SyncTree flushes every file under dir to disk with initdb --sync-only, the
// call PostgreSQL itself makes to trust a directory's contents. A restore
// unpacks a data directory out of an archive and starts a server on it with NO
// crash recovery to replay, so a file still in the page cache when the power
// goes leaves a cluster that starts and is corrupt.
func (s Server) SyncTree(ctx context.Context, dir string) error {
	initdb := filepath.Join(s.BinDir, "bin", "initdb")
	out, err := command(ctx, initdb, "--sync-only", "-D", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("embedpg: initdb --sync-only %s: %w: %s", dir, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// StopTimeout bounds a whole Stop: pg_ctl's own -t wait (stopWaitSeconds) plus
// grace for pg_ctl to report that it gave up. A context that expired exactly at
// -t would kill pg_ctl at the instant it would have printed "server does not
// shut down", leaving a bare "signal: killed" and no message.
const (
	stopWaitSeconds = 60
	StopTimeout     = (stopWaitSeconds + 30) * time.Second
)

// Lock takes the exclusive flock that serialises every state change on the
// server — Ensure's download/initdb/start, Stop, and a backup's whole
// stop → copy → start — and returns the release. Holding it is what lets a
// caller know no other rafikid process will start the server under its feet:
// a physical backup takes it precisely so a concurrent `rafikid db psql` cannot
// run Ensure and restart the postmaster halfway through the archive.
//
// The release is idempotent and safe to defer from the moment Lock returns.
func (s Server) Lock(ctx context.Context) (release func(), err error) {
	lock, err := s.acquireLock(ctx)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		})
	}, nil
}

// acquireLock opens LockPath and takes the flock on it, creating the directory
// the lock lives in. It is the ONE lock implementation: Ensure, Stop and Lock
// all reach it.
func (s Server) acquireLock(ctx context.Context) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(s.LockPath), 0o700); err != nil {
		return nil, fmt.Errorf("embedpg: %w", err)
	}
	lock, err := os.OpenFile(s.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("embedpg: %w", err)
	}
	if err := lockFile(ctx, lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("embedpg: locking %s: %w", s.LockPath, err)
	}
	return lock, nil
}

// Stop shuts the server down with pg_ctl stop -m fast, under the package's
// flock. A server that is not running is not an error: there is nothing to
// stop, which is the state the caller asked for.
//
// The caller owns whether a cancellation should reach this: pass
// context.WithoutCancel's result when a shutdown must finish even if the work
// that triggered it was interrupted.
func (s Server) Stop(ctx context.Context) error {
	release, err := s.Lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	return s.StopLocked(ctx)
}

// StopLocked is Stop for a caller that ALREADY holds Lock. A backup needs it:
// it must hold the flock across stop → copy → start, and taking it twice from
// one process would deadlock against itself. Calling this without the lock is
// safe only when nobody else can touch the server — reach for Stop instead.
func (s Server) StopLocked(ctx context.Context) error {
	pgctl := filepath.Join(s.BinDir, "bin", "pg_ctl")
	ctx, cancel := context.WithTimeout(ctx, StopTimeout)
	defer cancel()

	out, err := command(ctx, pgctl, "stop", "-m", "fast", "-w",
		"-t", strconv.Itoa(stopWaitSeconds), "-D", s.DataDir).CombinedOutput()
	if err == nil {
		return nil
	}

	// "Nothing to stop" is success, and pg_ctl gives it no reliable signal: on
	// the pinned 18.3 a stop with no server prints
	//   pg_ctl: PID file "<data dir>/postmaster.pid" does not exist
	//   Is server running?
	// and exits 1, while "no server running" is what STATUS prints (exit 3).
	// Matching text would therefore pin an implementation detail of one
	// release, so the answer comes from Status instead: a server that really is
	// down is the state the caller wanted, whatever pg_ctl said on its way
	// there, and a server that is still up is a failure worth reporting.
	running, serr := s.Status(ctx)
	if serr == nil && !running {
		return nil
	}
	if serr != nil {
		return fmt.Errorf("embedpg: %s stop failed: %w: %s (and checking whether the server is running failed too: %v)",
			pgctl, err, strings.TrimSpace(string(out)), serr)
	}
	return fmt.Errorf("embedpg: %s stop failed: %w: %s", pgctl, err, strings.TrimSpace(string(out)))
}

// start runs pg_ctl start, which waits for the server to accept connections
// (-w) up to 60 seconds. A failure is reported with the server's own log tail,
// since pg_ctl itself usually says only that the server did not start.
func (s Server) start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o700); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	pgctl := filepath.Join(s.BinDir, "bin", "pg_ctl")
	out, err := command(ctx, pgctl,
		"start", "-w", "-t", "60", "-D", s.DataDir, "-l", s.LogPath,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("embedpg: %s start failed: %w: %s\n%s",
			pgctl, err, strings.TrimSpace(string(out)), tailLines(s.LogPath, 20))
	}
	return nil
}

// tailLines is the last n lines of the file at path, or "" when it cannot be
// read. Used only to make a failed start legible.
func tailLines(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "last " + strconv.Itoa(len(lines)) + " log lines:\n" + strings.Join(lines, "\n")
}
