package embedpg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// The DSN has to survive libpq's own parser: a data directory containing a
// space or a quote is ordinary on macOS, and an unquoted DSN would silently
// point the daemon at a different directory.
func TestDSNQuoting(t *testing.T) {
	s := Server{DataDir: filepath.Join(t.TempDir(), "a b'c d")}

	cfg, err := pgconn.ParseConfig(s.DSN())
	c := assert.NewCollecting(t)
	c.NoError(err)
	c.Eq(s.DataDir, cfg.Host, "Host")
	c.Eq(uint16(Port), cfg.Port, "Port")
	c.Eq("rafiki", cfg.User, "User")
	c.Eq("postgres", cfg.Database, "Database")
}

// rafiki.conf is rewritten on every start, so it is the one place these settings
// live: no TCP socket, the pinned port, and a socket directory quoted for
// PostgreSQL's own string-literal rules.
func TestRafikiConfQuotesSocketDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "o'brien")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	s := Server{DataDir: dataDir}

	c := assert.NewCollecting(t)
	c.NoError(s.writeRafikiConf())

	body, err := os.ReadFile(filepath.Join(dataDir, "rafiki.conf"))
	c.NoError(err)
	want := "# Written by rafikid on every start; edits are overwritten.\n" +
		"listen_addresses = ''\n" +
		"port = 5432\n" +
		"unix_socket_directories = '" + strings.ReplaceAll(dataDir, "'", "''") + "'\n"
	c.Eq(want, string(body), "rafiki.conf")

	info, err := os.Stat(filepath.Join(dataDir, "rafiki.conf"))
	c.NoError(err)
	c.Eq(os.FileMode(0o600), info.Mode().Perm(), "rafiki.conf mode")
}
