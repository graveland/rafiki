// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ulikunitz/xz"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/embedpg"
)

// fakePgDumpDir puts a pg_dump on PATH that answers --version with version and,
// for a real dump, writes a fake archive to whatever -f names. It records its
// argv and the PG* variables it saw in files the test can read — which is how
// the two properties that matter (no DSN in argv, the DSN in the environment)
// are checked against a real exec rather than against a builder.
func fakePgDumpDir(t *testing.T, version string) (argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")
	envFile = filepath.Join(dir, "env.txt")

	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo "pg_dump (PostgreSQL) ` + version + `"
	exit 0
fi
printf '%s\n' "$@" > "$FAKE_PGDUMP_ARGV"
printf 'PGHOST=%s\nPGDATABASE=%s\nPGUSER=%s\nPGPASSWORD=%s\n' \
	"$PGHOST" "$PGDATABASE" "$PGUSER" "$PGPASSWORD" > "$FAKE_PGDUMP_ENV"
prev=""
out=""
for a in "$@"; do
	if [ "$prev" = "-f" ]; then out="$a"; fi
	prev="$a"
done
if [ -z "$out" ]; then exit 2; fi
printf 'PGDMP fake archive' > "$out"
`
	if err := os.WriteFile(filepath.Join(dir, "pg_dump"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pg_dump: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FAKE_PGDUMP_ARGV", argvFile)
	t.Setenv("FAKE_PGDUMP_ENV", envFile)
	return argvFile, envFile
}

// stubServerMajor makes the dump path see a server of the given major version
// without a database: the gate's subject is the comparison, not the connection.
func stubServerMajor(t *testing.T, major int) {
	t.Helper()
	orig := serverMajorVersion
	t.Cleanup(func() { serverMajorVersion = orig })
	serverMajorVersion = func(context.Context, string) (int, error) { return major, nil }
}

// The default format is a logical dump wherever one can be taken. An embedded
// install always can: the pg_dump in its own tree is built with the server, so
// whatever is (or is not) on PATH is irrelevant to it — an old or unreadable
// PATH pg_dump must neither be chosen nor cause a fall back to a physical copy.
// A configured DSN has to find pg_dump on PATH and has no data directory to fall
// back to, so a missing one is the answer, and an old one is still the dump (the
// run-time gate asks the server's own version and refuses if it has to).
func TestDbBackupFormatPriority(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		pgDump     string // "" = nothing on PATH, otherwise the --version it reports
		want       string
		wantErr    string
	}{
		{name: "embedded, usable pg_dump on PATH", configured: embeddedDSN, pgDump: "18.3", want: formatDump},
		{name: "embedded, no pg_dump on PATH: its own tree has one", configured: embeddedDSN, want: formatDump},
		{name: "embedded, older pg_dump on PATH: not consulted", configured: embeddedDSN, pgDump: "16.4", want: formatDump},
		{name: "embedded, unreadable pg_dump on PATH: not consulted", configured: embeddedDSN, pgDump: "not a version", want: formatDump},
		{name: "configured DSN, usable pg_dump", configured: "postgres://u@h/db", pgDump: "18.3", want: formatDump},
		{name: "configured DSN, no pg_dump", configured: "postgres://u@h/db", wantErr: "pg_dump not found"},
		{
			name:       "configured DSN, older pg_dump: the run-time gate owns it",
			configured: "postgres://u@h/db",
			pgDump:     "16.4",
			want:       formatDump,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			if tc.pgDump != "" {
				fakePgDumpDir(t, tc.pgDump)
			} else {
				t.Setenv("PATH", t.TempDir())
			}

			got, err := backupFormatPriority(context.Background(), tc.configured)
			if tc.wantErr != "" {
				c.Error(err, "want a refusal, got nil")
				c.StrContains(err.Error(), tc.wantErr, "error, got: %v", err)
				return
			}
			c.NoError(err, "backupFormatPriority returned error")
			c.Eq(tc.want, got, "format, got")
		})
	}
}

func TestDbBackupUnknownFormat(t *testing.T) {
	c := assert.NewAborting(t)
	err := runBackup(context.Background(), "postgres://u@h/db", filepath.Join(t.TempDir(), "app"), "zip", false, io.Discard)
	c.Error(err, "want an error for an unknown format, got nil")
	c.StrContains(err.Error(), `unknown format "zip"`, "error, got: %v", err)
}

// A bare path gets the format's suffix; a path the operator already suffixed is
// left alone, whatever the format.
func TestDbBackupExtension(t *testing.T) {
	cases := []struct{ path, suffix, want string }{
		{"/tmp/app", ".dump", "/tmp/app.dump"},
		{"/tmp/app.dump", ".dump", "/tmp/app.dump"},
		{"/tmp/app.txz", ".dump", "/tmp/app.txz"},
		{"/tmp/app.tar.gz", ".tgz", "/tmp/app.tar.gz"},
		{"/tmp/app.", ".dump", "/tmp/app.dump"},
		{"/tmp/.backup", ".dump", "/tmp/.backup.dump"},
		{"/tmp/a.b/app", ".txz", "/tmp/a.b/app.txz"},
	}
	c := assert.NewAborting(t)
	for _, tc := range cases {
		c.Eq(tc.want, withExtension(tc.path, tc.suffix), "withExtension(%q, %q), got", tc.path, tc.suffix)
	}
}

// A physical copy is the embedded server's data directory. A configured DSN
// names a server on some other host, which has no data directory here, so the
// refusal has to happen BEFORE anything is started, stopped or written — and
// without echoing the DSN, which may hold a password.
func TestDbBackupPhysicalRefusesConfiguredDSN(t *testing.T) {
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		t.Fatal("an embedded server was built for a configured DSN")
		return embedpg.Server{}
	}

	path := filepath.Join(t.TempDir(), "app")
	err := runBackup(context.Background(), "postgres://alice:s3cret@db.example:6543/app", path, formatTXZ, true, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want a refusal for a physical format on a configured DSN, got nil")
	c.StrContains(err.Error(), "embedded", "the refusal must say why, got: %v", err)
	c.StrContains(err.Error(), formatDump, "the refusal must name the format that does work, got: %v", err)
	c.NotStrContains(err.Error(), "s3cret", "the refusal must not echo the DSN, got: %v", err)
	c.NotStrContains(err.Error(), "db.example", "the refusal must not echo the DSN, got: %v", err)

	_, statErr := os.Stat(path + ".txz")
	c.True(os.IsNotExist(statErr), "a refused backup must write nothing, got stat: %v", statErr)
}

// The whole dump path against a real subprocess: the connection reaches pg_dump
// through PG* only, argv holds nothing the operator did not write, and the file
// is where the command said it would be.
func TestDbBackupDumpPutsConnectionInEnvNotArgv(t *testing.T) {
	argvFile, envFile := fakePgDumpDir(t, "18.3")
	stubServerMajor(t, 18)

	dir := t.TempDir()
	path := filepath.Join(dir, "app") // no suffix: .dump is appended
	var stderr bytes.Buffer

	err := runBackup(context.Background(),
		"postgres://alice:s3cret@db.example:6543/app", path, formatDump, false, &stderr)

	c := assert.NewAborting(t)
	c.NoError(err, "runBackup returned error: %s", stderr.String())

	final := path + ".dump"
	data, err := os.ReadFile(final)
	c.NoError(err, "the dump must be at the suffixed path")
	c.Eq("PGDMP fake archive", string(data), "dump contents, got")

	// argv: what this command adds, and nothing from the DSN.
	argv, err := os.ReadFile(argvFile)
	c.NoError(err, "the fake pg_dump did not record its argv")
	for _, want := range []string{"-Fc", "-f", ".partial-"} {
		c.StrContains(string(argv), want, "pg_dump argv must carry %q, got: %s", want, argv)
	}
	for _, bad := range []string{"s3cret", "db.example", "alice"} {
		c.NotStrContains(string(argv), bad, "pg_dump argv must not carry %q, got: %s", bad, argv)
	}

	// env: the connection, exactly.
	env, err := os.ReadFile(envFile)
	c.NoError(err, "the fake pg_dump did not record its environment")
	for _, want := range []string{"PGHOST=db.example", "PGDATABASE=app", "PGUSER=alice", "PGPASSWORD=s3cret"} {
		c.StrContains(string(env), want, "pg_dump environment must carry %q, got: %s", want, env)
	}

	// The report goes to stderr, and it names the real path — the temp name is
	// an implementation detail.
	c.StrContains(stderr.String(), final, "stderr must name the finished file, got: %s", stderr.String())
	c.StrContains(stderr.String(), "format "+formatDump, "stderr must name the format, got: %s", stderr.String())
	c.NotStrContains(stderr.String(), ".partial-", "stderr must not name the temporary file, got: %s", stderr.String())
}

// pg_dump cannot dump a server newer than itself: the refusal names both
// versions and writes nothing.
func TestDbBackupRefusesOlderPgDump(t *testing.T) {
	c := assert.NewAborting(t)

	cases := []struct {
		versionLine string
		serverMajor int
		wantErr     bool
	}{
		{versionLine: "pg_dump (PostgreSQL) 16.4 (Ubuntu 16.4-1.pgdg22.04+1)", serverMajor: 18, wantErr: true},
		{versionLine: "pg_dump (PostgreSQL) 9.6.24", serverMajor: 10, wantErr: true},
		{versionLine: "pg_dump (PostgreSQL) 18.3", serverMajor: 18},
		{versionLine: "pg_dump (PostgreSQL) 19.0", serverMajor: 18},
		{versionLine: "not a version line", serverMajor: 18, wantErr: true},
	}
	for _, tc := range cases {
		err := comparePgDumpVersion(tc.versionLine, tc.serverMajor)
		if !tc.wantErr {
			c.NoError(err, "comparePgDumpVersion(%q, %d) returned error", tc.versionLine, tc.serverMajor)
			continue
		}
		c.Error(err, "want a refusal for %q against a server at %d, got nil", tc.versionLine, tc.serverMajor)
	}

	// Both versions appear in the message: "PG 16" and "major version 18".
	err := comparePgDumpVersion("pg_dump (PostgreSQL) 16.4", 18)
	c.Error(err, "want a refusal, got nil")
	c.StrContains(err.Error(), "PostgreSQL 16", "the refusal must name the client's version, got: %v", err)
	c.StrContains(err.Error(), "major version 18", "the refusal must name the server's version, got: %v", err)
	c.StrContains(err.Error(), "postgresql-client-18", "the refusal must say which client to install, got: %v", err)

	// End to end: a real (fake) pg_dump that reports 16 against a server at 18
	// stops before running the dump at all.
	fakePgDumpDir(t, "16.4")
	stubServerMajor(t, 18)
	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	err = runBackup(context.Background(), "postgres://u@h/db", path, formatDump, false, io.Discard)
	c.Error(err, "want the version refusal, got nil")
	c.StrContains(err.Error(), "older than the server's major version 18", "error, got: %v", err)
	entries, rerr := os.ReadDir(dir)
	c.NoError(rerr, "read the backup directory")
	c.Eq(0, len(entries), "a refused backup must leave no file behind, got: %v", entries)
}

// stubBackupOps replaces the operations a physical backup performs, so a test can
// watch the order — in particular that the lock spans the whole sequence, that
// the restart cannot be skipped, and that a server that was down is never started.
func stubBackupOps(t *testing.T, running bool, stop func(context.Context, embedpg.Server) error,
	pack func(context.Context, io.Writer, string, string) error, ops *[]string) {
	t.Helper()

	// A real data directory: the command stats it before asking anything, and a
	// backup may not create one.
	dataDir := t.TempDir()
	origServer := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = origServer })
	newEmbeddedServer = func() embedpg.Server {
		return embedpg.Server{BinDir: "/fake/bin", DataDir: dataDir}
	}

	orig := backupEmbedded
	t.Cleanup(func() { backupEmbedded = orig })
	backupEmbedded = embeddedBackupOps{
		lock: func(context.Context, embedpg.Server) (func(), error) {
			*ops = append(*ops, "lock")
			return func() { *ops = append(*ops, "unlock") }, nil
		},
		status: func(context.Context, embedpg.Server) (bool, error) {
			*ops = append(*ops, "status")
			return running, nil
		},
		stop: stop,
		start: func(context.Context, embedpg.Server) error {
			*ops = append(*ops, "start")
			return nil
		},
		pack: pack,
	}
}

// The safety property of a physical backup: a server that was running is stopped
// for the copy and started again even when the copy fails. Leaving the install
// down would make the failure worse than the backup was worth, and the operator
// would have no working database to take a second backup with.
func TestDbBackupPhysicalRestartsAfterFailedArchive(t *testing.T) {
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return nil
		},
		func(context.Context, io.Writer, string, string) error {
			ops = append(ops, "pack")
			return errors.New("no space left on device")
		},
		&ops)

	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, true, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want the packing error, got nil")
	c.StrContains(err.Error(), "no space left on device", "error, got: %v", err)
	c.EqDeep([]string{"lock", "status", "stop", "pack", "unlock", "start"}, ops,
		"the server must be locked throughout, stopped once and started again after the failure, got ops")
	entries, rerr := os.ReadDir(dir)
	c.NoError(rerr, "read the backup directory")
	c.Eq(0, len(entries), "a failed backup must leave no partial file behind, got: %v", entries)
}

// A stop that FAILED does not mean the server is up: pg_ctl can give up while the
// postmaster goes on to shut down. The restart is therefore registered before the
// stop and runs anyway — Ensure is idempotent, so a server that really is up is a
// no-op, and one that is not is the whole point.
func TestDbBackupPhysicalRestartsEvenAfterAFailedStop(t *testing.T) {
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return errors.New("pg_ctl: server does not shut down")
		},
		func(context.Context, io.Writer, string, string) error {
			ops = append(ops, "pack")
			return nil
		},
		&ops)

	err := runBackup(context.Background(), embeddedDSN, filepath.Join(t.TempDir(), "app"), formatTXZ, true, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want the stop error, got nil")
	c.StrContains(err.Error(), "does not shut down", "error, got: %v", err)
	c.EqDeep([]string{"lock", "status", "stop", "unlock", "start"}, ops,
		"a failed stop must still be followed by a restart, got ops")
}

// Stopping a running server takes the database away from whatever is using it —
// a live daemon, every turn in flight — for the whole copy plus compression, so
// it is a downtime the operator has to accept explicitly.
func TestDbBackupPhysicalRefusesToStopARunningServerWithoutForce(t *testing.T) {
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			t.Error("stop was called without --force")
			return nil
		},
		func(context.Context, io.Writer, string, string) error {
			t.Error("pack was called without --force")
			return nil
		},
		&ops)

	path := filepath.Join(t.TempDir(), "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, false, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want a refusal for a running server without --force, got nil")
	c.StrContains(err.Error(), "--force", "the refusal must say how to proceed, got: %v", err)
	c.StrContains(err.Error(), "--format dump", "the refusal must name the alternative that needs no downtime, got: %v", err)
	c.StrContains(err.Error(), "in flight", "the refusal must say what is lost, got: %v", err)
	c.EqDeep([]string{"lock", "status", "unlock"}, ops,
		"nothing may be stopped, packed or started, and the lock must be released, got ops")
	_, statErr := os.Stat(path + ".txz")
	c.True(os.IsNotExist(statErr), "a refused backup must write nothing, got stat: %v", statErr)
}

// A backup must not change whether the install is up: a server that was DOWN is
// never started, so a cold install stays cold and the copy is of a quiet cluster
// either way. It needs no --force: nothing is stopped.
func TestDbBackupPhysicalLeavesAColdInstallAlone(t *testing.T) {
	var ops []string
	stubBackupOps(t, false,
		func(context.Context, embedpg.Server) error {
			t.Error("stop was called for a server that was not running")
			return nil
		},
		func(_ context.Context, w io.Writer, _ string, _ string) error {
			ops = append(ops, "pack")
			_, err := io.WriteString(w, "archive bytes")
			return err
		},
		&ops)

	path := filepath.Join(t.TempDir(), "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, false, io.Discard)

	c := assert.NewAborting(t)
	c.NoError(err, "runBackup returned error")
	c.EqDeep([]string{"lock", "status", "pack", "unlock"}, ops,
		"a stopped server must be packed as it is, with no stop and no start, got ops")
	data, rerr := os.ReadFile(path + ".txz")
	c.NoError(rerr, "the archive must be at the suffixed path")
	c.Eq("archive bytes", string(data), "archive contents, got")
}

// A data directory that does not exist is an error: a backup copies what is
// there, and creating the directory would produce an empty archive that looks
// like a success. Nothing is asked of the server, let alone started.
func TestDbBackupPhysicalRefusesAMissingDataDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-pg")

	origServer := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = origServer })
	newEmbeddedServer = func() embedpg.Server {
		return embedpg.Server{BinDir: "/fake/bin", DataDir: missing}
	}
	orig := backupEmbedded
	t.Cleanup(func() { backupEmbedded = orig })
	backupEmbedded = embeddedBackupOps{
		lock: func(context.Context, embedpg.Server) (func(), error) {
			t.Error("the lock was taken for a service whose data directory does not exist")
			return func() {}, nil
		},
		status: func(context.Context, embedpg.Server) (bool, error) {
			t.Error("status was asked about a service whose data directory does not exist")
			return false, nil
		},
		stop: func(context.Context, embedpg.Server) error {
			t.Error("stop was called with no data directory")
			return nil
		},
		start: func(context.Context, embedpg.Server) error {
			t.Error("start was called with no data directory")
			return nil
		},
		pack: func(context.Context, io.Writer, string, string) error {
			t.Error("pack was called with no data directory")
			return nil
		},
	}

	path := filepath.Join(t.TempDir(), "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, true, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want a refusal for a missing data directory, got nil")
	c.StrContains(err.Error(), missing, "the refusal must name the directory, got: %v", err)
	c.StrContains(err.Error(), "no data directory", "the refusal must say what is wrong, got: %v", err)
	_, statErr := os.Stat(path + ".txz")
	c.True(os.IsNotExist(statErr), "a refused backup must write nothing, got stat: %v", statErr)
	c.True(!filepathExists(missing), "the refusal must not create the data directory, got: %s", missing)

	// The same refusal covers a path that exists but is not a directory.
	c.NoError(os.WriteFile(missing, []byte("not a directory"), 0o600), "write the blocker file")
	err = runBackup(context.Background(), embeddedDSN, path, formatTXZ, true, io.Discard)
	c.Error(err, "want a refusal for a non-directory data dir, got nil")
	c.StrContains(err.Error(), "is not a directory", "error, got: %v", err)
}

// filepathExists reports whether path exists at all.
func filepathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The archive is written next to <path> as a .partial-* file, so a path inside
// the data directory would make the archiver walk into the file it is writing.
func TestDbBackupPhysicalRefusesAPathInsideTheDataDir(t *testing.T) {
	var ops []string
	stubBackupOps(t, false,
		func(context.Context, embedpg.Server) error {
			t.Error("stop was called for a path refusal")
			return nil
		},
		func(context.Context, io.Writer, string, string) error {
			t.Error("pack was called for a path refusal")
			return nil
		},
		&ops)

	dataDir := newEmbeddedServer().DataDir
	err := runBackup(context.Background(), embeddedDSN, filepath.Join(dataDir, "backups", "app"), formatTXZ, false, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want a refusal for a backup inside the data directory, got nil")
	c.StrContains(err.Error(), "inside the embedded data directory", "error, got: %v", err)
	c.StrContains(err.Error(), dataDir, "the refusal must name the data directory, got: %v", err)
	c.Eq(0, len(ops), "nothing may be locked, stopped or packed, got ops: %v", ops)
}

// The restart runs on a DETACHED context: Ctrl-C during the copy must not be able
// to leave the install down. A stub start that records its context is what pins
// it — dropping WithoutCancel fails this test.
func TestDbBackupPhysicalRestartSurvivesACancelledContext(t *testing.T) {
	var ops []string
	ctx, cancel := context.WithCancel(context.Background())
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return nil
		},
		func(context.Context, io.Writer, string, string) error {
			ops = append(ops, "pack")
			cancel() // the operator's Ctrl-C, arriving once the server is down
			return errors.New("interrupted")
		},
		&ops)

	var startCtxErr error
	backupEmbedded.start = func(startCtx context.Context, _ embedpg.Server) error {
		ops = append(ops, "start")
		startCtxErr = startCtx.Err()
		return nil
	}

	err := runBackup(ctx, embeddedDSN, filepath.Join(t.TempDir(), "app"), formatTXZ, true, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want the packing error, got nil")
	c.StrContains(err.Error(), "interrupted", "error, got: %v", err)
	c.EqDeep([]string{"lock", "status", "stop", "pack", "unlock", "start"}, ops, "ops, got")
	c.NoError(startCtxErr, "the restart must not see a cancelled context, got: %v", startCtxErr)
}

// A restart that FAILS is the operator's business even when the copy succeeded:
// the install is down and the command must say so.
func TestDbBackupPhysicalSurfacesAFailedRestart(t *testing.T) {
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return nil
		},
		func(_ context.Context, w io.Writer, _ string, _ string) error {
			ops = append(ops, "pack")
			_, err := io.WriteString(w, "archive bytes")
			return err
		},
		&ops)
	backupEmbedded.start = func(context.Context, embedpg.Server) error {
		ops = append(ops, "start")
		return errors.New("pg_ctl: could not start server")
	}

	path := filepath.Join(t.TempDir(), "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, true, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "a failed restart must be reported, got nil")
	c.StrContains(err.Error(), "restarting the embedded database", "error, got: %v", err)
	c.StrContains(err.Error(), "could not start server", "the cause must survive, got: %v", err)
	c.EqDeep([]string{"lock", "status", "stop", "pack", "unlock", "start"}, ops, "ops, got")
	// The copy itself is complete and on disk: the failure is the restart, not
	// the archive.
	if _, statErr := os.Stat(path + ".txz"); statErr != nil {
		t.Fatalf("the archive must still be there: %v", statErr)
	}
}

// The archive is a real tar stream: names are relative to the data directory
// with no wrapper, modes and symlinks survive, and both compressions read back.
func TestDbBackupPhysicalArchiveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := assert.NewAborting(t)

	c.NoError(os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18\n"), 0o600), "write PG_VERSION")
	c.NoError(os.MkdirAll(filepath.Join(dir, "base", "1"), 0o700), "mkdir base/1")
	c.NoError(os.WriteFile(filepath.Join(dir, "base", "1", "rel"), []byte("relation"), 0o600), "write rel")
	c.NoError(os.Symlink("PG_VERSION", filepath.Join(dir, "link")), "symlink")

	for _, format := range []string{formatTXZ, formatTGZ} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			c := assert.NewAborting(t)
			c.NoError(writeDataDirArchive(context.Background(), &buf, dir, format), "writeDataDirArchive returned error")

			var r io.Reader
			switch format {
			case formatTXZ:
				xr, err := xz.NewReader(&buf)
				c.NoError(err, "xz.NewReader returned error")
				r = xr
			case formatTGZ:
				gr, err := gzip.NewReader(&buf)
				c.NoError(err, "gzip.NewReader returned error")
				r = gr
			}

			files := map[string]string{}
			links := map[string]string{}
			tr := tar.NewReader(r)
			for {
				hdr, err := tr.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				c.NoError(err, "reading the archive")
				if hdr.Typeflag == tar.TypeSymlink {
					links[hdr.Name] = hdr.Linkname
					continue
				}
				if hdr.Typeflag != tar.TypeReg {
					continue
				}
				body, err := io.ReadAll(tr)
				c.NoError(err, "reading %s from the archive", hdr.Name)
				files[hdr.Name] = string(body)
			}

			c.EqDeep(map[string]string{
				"PG_VERSION": "18\n",
				"base/1/rel": "relation",
			}, files, "archived files, got")
			c.EqDeep(map[string]string{"link": "PG_VERSION"}, links, "archived symlinks, got")
		})
	}
}

// A socket or a fifo cannot be copied, and skipping it would produce an archive
// that restores as a corrupt database: the archiver refuses instead.
func TestDbBackupArchiveRefusesAFifo(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, ".s.PGSQL.5432")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	var buf bytes.Buffer
	err := writeDataDirArchive(context.Background(), &buf, dir, formatTGZ)

	c := assert.NewAborting(t)
	c.Error(err, "want a refusal for a fifo in the data directory, got nil")
	c.StrContains(err.Error(), fifo, "the refusal must name the entry, got: %v", err)
}

// A symlink the restore would refuse is refused at ARCHIVE time instead: an
// absolute target, or one that climbs with `..` (`db restore` unpacks through
// pkg/embedpg's ExtractTar, which refuses exactly those). Recording one would
// produce a backup that fails the moment it is needed, so the archiver names the
// entry and writes nothing.
func TestDbBackupArchiveRefusesAnEscapingSymlink(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{name: "an absolute target", target: "/var/lib/postgresql/wal", want: "absolute path"},
		{name: "a climbing target", target: "../../wal", want: "climbs"},
		{name: "a target that only looks like it stays", target: "base/../base", want: "climbs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			link := filepath.Join(dir, "pg_wal")
			c := assert.NewAborting(t)
			c.NoError(os.Symlink(tc.target, link), "symlink %s -> %s", link, tc.target)

			var buf bytes.Buffer
			err := writeDataDirArchive(context.Background(), &buf, dir, formatTGZ)

			c.Error(err, "want a refusal for a symlink target the restore refuses, got nil")
			c.StrContains(err.Error(), link, "the refusal must name the entry, got: %v", err)
			c.StrContains(err.Error(), tc.want, "the refusal must say what is wrong with the target, got: %v", err)
		})
	}

	// A relative target that stays inside the tree is archived as it is: the rule
	// is escape, not relativity.
	dir := t.TempDir()
	c := assert.NewAborting(t)
	c.NoError(os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18\n"), 0o600), "write PG_VERSION")
	c.NoError(os.Symlink("PG_VERSION", filepath.Join(dir, "version-link")), "symlink")

	var buf bytes.Buffer
	c.NoError(writeDataDirArchive(context.Background(), &buf, dir, formatTGZ), "a sibling symlink must be archived, got error")
}

// The cobra wiring, not just the function: one path and no more, an unknown
// --format refused before anything is created, and --force carried through to the
// physical path (the only place the downtime is accepted).
func TestDbBackupCmdWiring(t *testing.T) {
	c := assert.NewAborting(t)

	for _, args := range [][]string{{"backup"}, {"backup", "one", "two"}} {
		cmd := newDbCmd()
		cmd.SetArgs(args)
		c.Error(cmd.Execute(), "db backup %v must be a usage error", args)
	}

	dir := t.TempDir()
	cmd := newDbCmd()
	cmd.SetArgs([]string{"backup", filepath.Join(dir, "app"), "--format", "zip", "--db", embeddedDSN, "--force"})
	err := cmd.Execute()
	c.Error(err, "want an unknown-format refusal, got nil")
	c.StrContains(err.Error(), `unknown format "zip"`, "error, got: %v", err)
	entries, rerr := os.ReadDir(dir)
	c.NoError(rerr, "read the directory")
	c.Eq(0, len(entries), "a rejected format must create nothing, got: %v", entries)

	// --force is what lets a running server be stopped: through cobra it must
	// reach runPhysicalBackup, or the copy would be refused.
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return nil
		},
		func(_ context.Context, w io.Writer, _ string, _ string) error {
			ops = append(ops, "pack")
			_, werr := io.WriteString(w, "archive bytes")
			return werr
		},
		&ops)

	var stderr bytes.Buffer
	cmd = newDbCmd()
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"backup", filepath.Join(dir, "forced"), "--format", "txz", "--db", embeddedDSN, "--force"})
	c.NoError(cmd.Execute(), "db backup --force: %s", stderr.String())
	c.EqDeep([]string{"lock", "status", "stop", "pack", "unlock", "start"}, ops,
		"--force must reach the physical path, got ops")
	if _, statErr := os.Stat(filepath.Join(dir, "forced.txz")); statErr != nil {
		t.Fatalf("the archive must be at the suffixed path: %v", statErr)
	}
}

// The group is what the root registers: its verbs, and one --db for all of them.
func TestDbCmdHasItsVerbs(t *testing.T) {
	cmd := newDbCmd()

	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	c := assert.NewAborting(t)
	for _, want := range []string{"psql", "backup", "restore"} {
		c.True(got[want], "db must register %q, got: %v", want, got)
	}
	c.Eq(3, len(got), "db must register exactly psql, backup and restore, got: %v", got)

	// --db is persistent, so every verb resolves the database the same way and
	// no verb declares a flag of that name itself.
	flag := cmd.PersistentFlags().Lookup("db")
	c.True(flag != nil, "--db must be a persistent flag of db")
	for _, sub := range cmd.Commands() {
		c.True(sub.Flags().Lookup("db") == nil, "%s must not declare its own --db", sub.Name())
	}
}

// The suffix rules and the argv rule are one interface: a path that gets a
// suffix must still be the file the backup writes.
func TestDbBackupDumpKeepsAnOperatorSuffix(t *testing.T) {
	argvFile, _ := fakePgDumpDir(t, "18.3")
	stubServerMajor(t, 18)

	path := filepath.Join(t.TempDir(), "app.backup")
	var stderr bytes.Buffer
	err := runBackup(context.Background(), "postgres://u@h/db", path, formatDump, false, &stderr)

	c := assert.NewAborting(t)
	c.NoError(err, "runBackup returned error: %s", stderr.String())
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the dump must be at the path the operator gave, got: %v", statErr)
	}
	argv, rerr := os.ReadFile(argvFile)
	c.NoError(rerr, "the fake pg_dump did not record its argv")
	c.NotStrContains(string(argv), "app.backup.dump", "the suffix must not be appended twice, got: %s", argv)
}

// The priority list and the formats it can name must stay in step with the
// suffix table: a format with no suffix would write an extensionless file.
func TestDbBackupFormatSuffixesCoverFormats(t *testing.T) {
	c := assert.NewAborting(t)
	for _, format := range []string{formatDump, formatTXZ, formatTGZ} {
		_, ok := backupFormatExt[format]
		c.True(ok, "format %q has no suffix in backupFormatExt", format)
	}
	c.Eq(3, len(backupFormatExt), "backupFormatExt must hold exactly the three formats, got: %v", backupFormatExt)
}

// An interrupt during an archive STOPS it. The server is stopped while this
// runs, so a copy that ignored a Ctrl-C would hold the database down for its
// whole length (minutes on a large data directory) and invite the SIGKILL that
// leaves the server down for good.
func TestDbBackupArchiveStopsWhenTheContextIsCancelled(t *testing.T) {
	c := assert.NewAborting(t)

	dir := t.TempDir()
	writeDataDirTree(t, dir, "original")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	err := writeDataDirArchive(ctx, &buf, dir, formatTGZ)

	c.Error(err, "want the cancellation, got nil")
	c.True(errors.Is(err, context.Canceled), "the archive must stop with the context, got: %v", err)
}

// An interrupt that arrived BEFORE the stop is not acted on at all: the server
// is still up, so the cheapest safe answer is to leave it up rather than stop it
// and hand the operator a restart they did not ask for.
func TestDbBackupDoesNotStopAServerAfterAnInterrupt(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return nil
		},
		func(context.Context, io.Writer, string, string) error {
			ops = append(ops, "pack")
			return nil
		},
		&ops)

	dir := t.TempDir()
	err := runBackup(ctx, embeddedDSN, filepath.Join(dir, "app"), formatTXZ, true, io.Discard)

	c.Error(err, "want the cancellation, got nil")
	c.True(errors.Is(err, context.Canceled), "error, got: %v", err)
	c.StrContains(err.Error(), "was not stopped", "the error must say what was left alone, got: %v", err)
	c.EqDeep([]string{"lock", "status", "unlock"}, ops,
		"an interrupted run must not stop or start a server it never touched, got ops")
	entries, rerr := os.ReadDir(dir)
	c.NoError(rerr, "read the backup directory")
	c.Eq(0, len(entries), "an interrupted backup must leave no file behind, got: %v", entries)
}
