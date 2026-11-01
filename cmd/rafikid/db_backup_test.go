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

// The default format is resolved at run time, so the same command on the same
// host produces a dump where pg_dump exists and a physical copy where it does
// not — and refuses when neither is available.
func TestDbBackupFormatPriority(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		pgDump     bool
		want       string
		wantErr    string
	}{
		{name: "pg_dump present, embedded", configured: embeddedDSN, pgDump: true, want: formatDump},
		{name: "pg_dump present, configured DSN", configured: "postgres://u@h/db", pgDump: true, want: formatDump},
		{name: "no pg_dump, embedded", configured: embeddedDSN, want: formatTXZ},
		{name: "no pg_dump, configured DSN", configured: "postgres://u@h/db", wantErr: "pg_dump not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			if tc.pgDump {
				fakePgDumpDir(t, "18.3")
			} else {
				t.Setenv("PATH", t.TempDir())
			}

			got, err := resolveBackupFormat(tc.configured)
			if tc.wantErr != "" {
				c.Error(err, "want a refusal, got nil")
				c.StrContains(err.Error(), tc.wantErr, "error, got: %v", err)
				return
			}
			c.NoError(err, "resolveBackupFormat returned error")
			c.Eq(tc.want, got, "format, got")
		})
	}
}

func TestDbBackupUnknownFormat(t *testing.T) {
	c := assert.NewAborting(t)
	err := runBackup(context.Background(), "postgres://u@h/db", filepath.Join(t.TempDir(), "app"), "zip", io.Discard)
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
	err := runBackup(context.Background(), "postgres://alice:s3cret@db.example:6543/app", path, formatTXZ, io.Discard)

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
		"postgres://alice:s3cret@db.example:6543/app", path, formatDump, &stderr)

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
	err = runBackup(context.Background(), "postgres://u@h/db", path, formatDump, io.Discard)
	c.Error(err, "want the version refusal, got nil")
	c.StrContains(err.Error(), "older than the server's major version 18", "error, got: %v", err)
	entries, rerr := os.ReadDir(dir)
	c.NoError(rerr, "read the backup directory")
	c.Eq(0, len(entries), "a refused backup must leave no file behind, got: %v", entries)
}

// stubBackupOps replaces the four operations a physical backup performs, so a
// test can watch the order — in particular that the restart cannot be skipped
// and that a server that was down is never started.
func stubBackupOps(t *testing.T, running bool, stop func(context.Context, embedpg.Server) error,
	pack func(io.Writer, string, string) error, ops *[]string) {
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

// The safety property of a physical backup: a server that was running is
// stopped for the copy and started again even when the copy fails. Leaving the
// install down would make the failure worse than the backup was worth, and the
// operator would have no working database to take a second backup with.
func TestDbBackupPhysicalRestartsAfterFailedArchive(t *testing.T) {
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return nil
		},
		func(io.Writer, string, string) error {
			ops = append(ops, "pack")
			return errors.New("no space left on device")
		},
		&ops)

	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want the packing error, got nil")
	c.StrContains(err.Error(), "no space left on device", "error, got: %v", err)
	c.EqDeep([]string{"status", "stop", "pack", "start"}, ops,
		"the server must be stopped once and started again after the failure, got ops")
	entries, rerr := os.ReadDir(dir)
	c.NoError(rerr, "read the backup directory")
	c.Eq(0, len(entries), "a failed backup must leave no partial file behind, got: %v", entries)
}

// A backup must not change whether the install is up: a server that was DOWN is
// never started, so a cold install stays cold and the copy is of a quiet
// cluster either way.
func TestDbBackupPhysicalLeavesAColdInstallAlone(t *testing.T) {
	var ops []string
	stubBackupOps(t, false,
		func(context.Context, embedpg.Server) error {
			t.Error("stop was called for a server that was not running")
			return nil
		},
		func(w io.Writer, _ string, _ string) error {
			ops = append(ops, "pack")
			_, err := io.WriteString(w, "archive bytes")
			return err
		},
		&ops)

	path := filepath.Join(t.TempDir(), "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, io.Discard)

	c := assert.NewAborting(t)
	c.NoError(err, "runBackup returned error")
	c.EqDeep([]string{"status", "pack"}, ops,
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
		pack: func(io.Writer, string, string) error {
			t.Error("pack was called with no data directory")
			return nil
		},
	}

	path := filepath.Join(t.TempDir(), "app")
	err := runBackup(context.Background(), embeddedDSN, path, formatTXZ, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want a refusal for a missing data directory, got nil")
	c.StrContains(err.Error(), missing, "the refusal must name the directory, got: %v", err)
	c.StrContains(err.Error(), "no data directory", "the refusal must say what is wrong, got: %v", err)
	_, statErr := os.Stat(path + ".txz")
	c.True(os.IsNotExist(statErr), "a refused backup must write nothing, got stat: %v", statErr)
	c.True(!filepathExists(missing), "the refusal must not create the data directory, got: %s", missing)

	// The same refusal covers a path that exists but is not a directory.
	c.NoError(os.WriteFile(missing, []byte("not a directory"), 0o600), "write the blocker file")
	err = runBackup(context.Background(), embeddedDSN, path, formatTXZ, io.Discard)
	c.Error(err, "want a refusal for a non-directory data dir, got nil")
	c.StrContains(err.Error(), "is not a directory", "error, got: %v", err)
}

// filepathExists reports whether path exists at all.
func filepathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A successful physical backup has the same shape, and reports the format it
// actually wrote.
func TestDbBackupPhysicalReportsTheArchive(t *testing.T) {
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return nil
		},
		func(w io.Writer, dir, format string) error {
			ops = append(ops, "pack "+format)
			_, err := io.WriteString(w, "archive bytes")
			return err
		},
		&ops)

	path := filepath.Join(t.TempDir(), "app")
	var stderr bytes.Buffer
	err := runBackup(context.Background(), embeddedDSN, path, formatTGZ, &stderr)

	c := assert.NewAborting(t)
	c.NoError(err, "runBackup returned error: %s", stderr.String())
	c.EqDeep([]string{"status", "stop", "pack " + formatTGZ, "start"}, ops, "ops, got")
	data, rerr := os.ReadFile(path + ".tgz")
	c.NoError(rerr, "the archive must be at the suffixed path")
	c.Eq("archive bytes", string(data), "archive contents, got")
	c.StrContains(stderr.String(), "format "+formatTGZ, "stderr must name the format, got: %s", stderr.String())
	c.StrContains(stderr.String(), path+".tgz", "stderr must name the file, got: %s", stderr.String())
}

// When the STOP fails the server was never down, so there is nothing to start:
// starting it again would be a second start of a running server.
func TestDbBackupPhysicalDoesNotRestartAfterFailedStop(t *testing.T) {
	var ops []string
	stubBackupOps(t, true,
		func(context.Context, embedpg.Server) error {
			ops = append(ops, "stop")
			return errors.New("pg_ctl: cannot stop the server")
		},
		func(io.Writer, string, string) error {
			ops = append(ops, "pack")
			return nil
		},
		&ops)

	err := runBackup(context.Background(), embeddedDSN, filepath.Join(t.TempDir(), "app"), formatTXZ, io.Discard)

	c := assert.NewAborting(t)
	c.Error(err, "want the stop error, got nil")
	c.StrContains(err.Error(), "cannot stop the server", "error, got: %v", err)
	c.EqDeep([]string{"status", "stop"}, ops, "a failed stop must not be followed by a restart, got ops")
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
			c.NoError(writeDataDirArchive(&buf, dir, format), "writeDataDirArchive returned error")

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
	err := writeDataDirArchive(&buf, dir, formatTGZ)

	c := assert.NewAborting(t)
	c.Error(err, "want a refusal for a fifo in the data directory, got nil")
	c.StrContains(err.Error(), fifo, "the refusal must name the entry, got: %v", err)
}

// The group is what the root registers: its verbs, and one --db for all of them.
func TestDbCmdHasItsVerbs(t *testing.T) {
	cmd := newDbCmd()

	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	c := assert.NewAborting(t)
	for _, want := range []string{"psql", "backup"} {
		c.True(got[want], "db must register %q, got: %v", want, got)
	}
	c.Eq(2, len(got), "db must register exactly psql and backup, got: %v", got)

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
	err := runBackup(context.Background(), "postgres://u@h/db", path, formatDump, &stderr)

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
