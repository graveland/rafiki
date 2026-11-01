// Package embedpg runs a private PostgreSQL 18 for a single-machine rafiki
// install: it downloads and verifies the server binaries, initialises a data
// directory, and starts the server listening on a unix socket only. It is
// daemon-only: cmd/rafiki must never import it.
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
	"syscall"

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
	BaseURL  string       // Maven base; DefaultBaseURL when empty
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

// DSN is the libpq keyword/value string that reaches this server. The host is
// the data directory — a path, not a hostname — which is how libpq names a unix
// socket directory; sslmode=disable because a socket is not a network.
func (s Server) DSN() string {
	return "host=" + quoteConninfo(s.DataDir) +
		" port=" + strconv.Itoa(Port) +
		" user=rafiki dbname=postgres sslmode=disable"
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
func (s Server) Ensure(ctx context.Context) (string, error) {
	if err := os.MkdirAll(filepath.Dir(s.LockPath), 0o700); err != nil {
		return "", fmt.Errorf("embedpg: %w", err)
	}
	lock, err := os.OpenFile(s.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("embedpg: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("embedpg: locking %s: %w", s.LockPath, err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	// The socket path is a hard limit of the platform, and it is checked before
	// anything is written or downloaded: discovering it after a 100 MB download
	// would be a wasted download on every attempt.
	if err := s.checkSocketPath(); err != nil {
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
	if err := s.writeRafikiConf(); err != nil {
		return "", err
	}
	running, err := s.status(ctx)
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
	staging := s.DataDir + ".init"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.DataDir), 0o700); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}

	initdb := filepath.Join(s.BinDir, "bin", "initdb")
	out, err := exec.CommandContext(ctx, initdb,
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
	_, werr := conf.WriteString("\ninclude 'rafiki.conf'\n")
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

// writeRafikiConf writes the settings this package owns, overwriting whatever a
// previous run (or a curious operator) left there.
func (s Server) writeRafikiConf() error {
	body := "# Written by rafikid on every start; edits are overwritten.\n" +
		"listen_addresses = ''\n" +
		"port = " + strconv.Itoa(Port) + "\n" +
		// A single quote in a PostgreSQL string literal is doubled, and the
		// data directory is where the only socket lives.
		"unix_socket_directories = '" + strings.ReplaceAll(s.DataDir, "'", "''") + "'\n"

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

// status reports whether the server is already running. pg_ctl's exit codes are
// the interface: 0 means running, 3 means not running, anything else is a
// failure worth reporting.
func (s Server) status(ctx context.Context) (bool, error) {
	pgctl := filepath.Join(s.BinDir, "bin", "pg_ctl")
	out, err := exec.CommandContext(ctx, pgctl, "status", "-D", s.DataDir).CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return false, nil
	}
	return false, fmt.Errorf("embedpg: %s status failed: %w: %s", pgctl, err, strings.TrimSpace(string(out)))
}

// start runs pg_ctl start, which waits for the server to accept connections
// (-w) up to 60 seconds. A failure is reported with the server's own log tail,
// since pg_ctl itself usually says only that the server did not start.
func (s Server) start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o700); err != nil {
		return fmt.Errorf("embedpg: %w", err)
	}
	pgctl := filepath.Join(s.BinDir, "bin", "pg_ctl")
	out, err := exec.CommandContext(ctx, pgctl,
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
