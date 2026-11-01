package embedpg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multigres/testkit/assert"
)

// The unix socket path lives inside the data directory, so a long data
// directory is a hard failure — and one that must be reported before a single
// byte is downloaded, since the operator can only fix it by moving the data dir.
func TestSocketPathTooLong(t *testing.T) {
	srv, hits := serveJar(t, []byte("must not be requested"))
	s := Server{
		BinDir:   filepath.Join(t.TempDir(), "bin"),
		DataDir:  filepath.Join(t.TempDir(), strings.Repeat("a", 120)),
		LogPath:  filepath.Join(t.TempDir(), "pg.log"),
		LockPath: filepath.Join(t.TempDir(), "pg.lock"),
		BaseURL:  srv.URL,
		Client:   srv.Client(),
	}

	_, err := s.Ensure(context.Background())
	c := assert.NewCollecting(t)
	c.ErrorContains(err, "set XDG_DATA_HOME")
	c.Eq(int64(0), hits.Load(), "requests despite a socket path that cannot work")
	c.NoDirExists(s.BinDir)
}

// A data directory from another major version is refused rather than opened.
// The data dir has to be short enough for its socket path (Ensure checks that
// first), so it does not come from t.TempDir().
func TestPGVersionMismatch(t *testing.T) {
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
	// A pre-populated bin/postgres is the "binaries are already installed"
	// marker, so Ensure never reaches for the network.
	if err := os.MkdirAll(filepath.Join(s.BinDir, "bin"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.BinDir, "bin", "postgres"), nil, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(s.DataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.DataDir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = s.Ensure(context.Background())
	assert.NewCollecting(t).ErrorContains(err, "major upgrades are not supported")
}

// Both DSNs have to survive libpq's own parser: a data directory containing a
// space or a quote is ordinary on macOS, and an unquoted DSN would silently
// point the daemon at a different directory. They must also name the two
// databases this server has — rafiki to serve from, postgres to create it from
// — and agree about everything else, or the database would be created on one
// server and dialled on another.
func TestDSNQuoting(t *testing.T) {
	s := Server{DataDir: filepath.Join(t.TempDir(), "a b'c d")}

	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		what     string
		dsn      string
		database string
	}{
		{"DSN", s.DSN(), Database},
		{"MaintenanceDSN", s.MaintenanceDSN(), maintenanceDatabase},
	} {
		cfg, err := pgconn.ParseConfig(tc.dsn)
		c.NoError(err, "%s must parse", tc.what)
		c.Eq(s.DataDir, cfg.Host, "%s Host", tc.what)
		c.Eq(uint16(Port), cfg.Port, "%s Port", tc.what)
		c.Eq("rafiki", cfg.User, "%s User", tc.what)
		c.Eq(tc.database, cfg.Database, "%s Database", tc.what)
	}

	c.Eq("rafiki", Database, "the database the daemon serves from")
	c.Eq("postgres", maintenanceDatabase, "the database initdb creates")
	c.Eq(s.DSN(), strings.Replace(s.MaintenanceDSN(), "dbname="+maintenanceDatabase, "dbname="+Database, 1),
		"the two DSNs may differ only in the database name")
}

// rafiki.conf is rewritten on every start, so it is the one place these settings
// live: no TCP socket, the pinned port, and a socket directory quoted for
// PostgreSQL's own string-literal rules — where both the quote and the
// backslash are escapes.
func TestRafikiConfQuotesSocketDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, `o'brien\pg`)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	s := Server{DataDir: dataDir}

	c := assert.NewCollecting(t)
	c.NoError(s.writeRafikiConf())

	body, err := os.ReadFile(filepath.Join(dataDir, "rafiki.conf"))
	c.NoError(err)
	// The escaping is spelled out literally here rather than reusing quoteConf,
	// so the test fails if the escaping changes.
	want := "# Written by rafikid on every start; edits are overwritten.\n" +
		"listen_addresses = ''\n" +
		"port = 5432\n" +
		"unix_socket_directories = '" + dir + `/o''brien\\pg'` + "\n" +
		"unix_socket_permissions = 0700\n"
	c.Eq(want, string(body), "rafiki.conf")

	info, err := os.Stat(filepath.Join(dataDir, "rafiki.conf"))
	c.NoError(err)
	c.Eq(os.FileMode(0o600), info.Mode().Perm(), "rafiki.conf mode")
}

// prepareDataDir is what makes rafiki.conf effective at all: the file itself,
// and the include that is the only way Postgres reads it. A data directory that
// already has a PG_VERSION never went through initdb here, so this has to run on
// every start — and be idempotent, because it does.
func TestPrepareDataDir(t *testing.T) {
	dataDir := t.TempDir()
	conf := filepath.Join(dataDir, "postgresql.conf")
	if err := os.WriteFile(conf, []byte("listen_addresses = 'localhost'\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s := Server{DataDir: dataDir}

	c := assert.NewCollecting(t)
	c.NoError(s.prepareDataDir())
	c.NoError(s.prepareDataDir()) // idempotent: the second start must not append again

	body, err := os.ReadFile(conf)
	c.NoError(err)
	c.Eq(1, strings.Count(string(body), "include 'rafiki.conf'"), "include count in %q", string(body))
	// The operator's own settings are still there; the include is appended.
	c.StrContains(string(body), "listen_addresses = 'localhost'")

	rafiki, err := os.ReadFile(filepath.Join(dataDir, "rafiki.conf"))
	c.NoError(err)
	c.StrContains(string(rafiki), "listen_addresses = ''")
}

// Ensure must actually call prepareDataDir — the helper being correct is not the
// same as the daemon running it. The binaries here are stubs: pg_ctl status
// exits 0, so Ensure believes the server is already up and never starts one, and
// bin/postgres exists, so nothing is downloaded. The data dir has to be short
// enough for its socket path, so it does not come from t.TempDir().
func TestEnsurePreparesDataDir(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(s.BinDir, "bin"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.BinDir, "bin", "postgres"), nil, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.BinDir, "bin", "pg_ctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(s.DataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.DataDir, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	conf := filepath.Join(s.DataDir, "postgresql.conf")
	if err := os.WriteFile(conf, []byte("# as initdb left it\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dsn, err := s.Ensure(context.Background())
	c := assert.NewCollecting(t)
	c.NoError(err)
	c.Eq(s.DSN(), dsn, "Ensure's DSN")

	body, err := os.ReadFile(conf)
	c.NoError(err)
	c.StrContains(string(body), "include 'rafiki.conf'", "postgresql.conf after Ensure")

	rafiki, err := os.ReadFile(filepath.Join(s.DataDir, "rafiki.conf"))
	c.NoError(err)
	c.StrContains(string(rafiki), "listen_addresses = ''")
}

// A caller waiting behind another process's initdb must be able to give up: the
// flock is taken without blocking, so a cancelled context ends the wait instead
// of parking for the length of someone else's download.
func TestEnsureLockWaitHonoursContext(t *testing.T) {
	s := testServer(t)
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
	_, err = s.Ensure(ctx)
	elapsed := time.Since(start)

	c := assert.NewCollecting(t)
	c.ErrorIs(err, context.Canceled)
	c.Less(2*time.Second, elapsed, "Ensure waited %s behind a held lock", elapsed)
}

// The data directory is the access control for this server — it is also the
// socket directory, and local auth is `trust` — so its mode is re-asserted on
// every start, not just when initdb happened to create it. A directory that was
// copied, restored, or chmodded by hand is exactly the case this catches.
func TestPrepareDataDirRestoresDataDirMode(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "postgresql.conf"), []byte("# as initdb left it\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(dataDir, 0o750); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	s := Server{DataDir: dataDir}

	c := assert.NewCollecting(t)
	c.NoError(s.prepareDataDir())

	info, err := os.Stat(dataDir)
	c.NoError(err)
	c.Eq(os.FileMode(0o700), info.Mode().Perm(), "data dir mode after prepareDataDir")

	rafiki, err := os.ReadFile(filepath.Join(dataDir, "rafiki.conf"))
	c.NoError(err)
	c.StrContains(string(rafiki), "unix_socket_permissions = 0700")
}

// The postmaster outlives the command that started it, and its environment is
// readable (ps eww, /proc/<pid>/environ) for as long as it runs. The daemon has
// already been started with RAFIKI_SECRETS_KEY and its provider API keys by the
// time Ensure runs, so none of them may reach a subprocess. This drives the
// helper every exec in the package is built by, so it fails if one goes back to
// inheriting os.Environ().
//
// TZ is the other half of the assertion: a whitelist that dropped it would give
// the cluster the system's timezone instead of the daemon's, and the mistake is
// invisible until a timestamp is read back.
func TestSubprocessEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("RAFIKI_SECRETS_KEY", "sentinel-secrets-key")
	t.Setenv("OPENROUTER_API_KEY", "sentinel-provider-key")
	t.Setenv("PGPASSWORD", "sentinel-pg-password")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sentinel-aws-key")
	t.Setenv("TZ", "Pacific/Auckland")

	cmd := command(context.Background(), filepath.Join(t.TempDir(), "initdb"), "-D", "staging")

	c := assert.NewCollecting(t)
	c.NotZero(len(cmd.Env), "subprocess environment is empty; PATH must survive")
	for _, kv := range cmd.Env {
		c.NotStrContains(kv, "sentinel", "subprocess environment leaked %q", kv)
	}
	// PATH is what lets the binaries find their own helpers, and TZ is what
	// makes initdb take the daemon's timezone rather than the system's.
	env := strings.Join(cmd.Env, "\n")
	c.StrContains(env, "PATH=")
	c.StrContains(env, "TZ=Pacific/Auckland")
}

// A relative data directory is refused: Postgres resolves a relative socket
// directory against its own data directory, so the path this package measures
// would not be the path the server uses.
func TestRelativeDataDirIsRefused(t *testing.T) {
	s := Server{DataDir: "relative/pg"}
	assert.NewCollecting(t).ErrorContains(s.checkDataDir(), "is not absolute")
}
