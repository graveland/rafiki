// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
)

func stateInspectKey() []string {
	return []string{"docker", "inspect", "-f", "{{.State.Status}}", managedPGContainer}
}

func healthInspectKey() []string {
	return []string{"docker", "inspect", "-f", "{{.State.Health.Status}}", managedPGContainer}
}

func eventsKey() []string {
	return []string{
		"docker", "events",
		"--filter", "container=" + managedPGContainer,
		"--filter", "event=health_status",
		"--format", "{{.Status}}",
	}
}

func logsKey() []string {
	return []string{"docker", "logs", "--tail", "20", managedPGContainer}
}

func initLogsKey() []string {
	return []string{"docker", "logs", "-f", managedPGContainer}
}

// findStream returns the first stream call whose full argv equals argv (exact
// match, unlike findCall's prefix match).
func findStream(calls []fakeCall, argv ...string) *fakeCall {
	want := runKey(argv)
	for i := range calls {
		if runKey(calls[i].argv) == want {
			return &calls[i]
		}
	}
	return nil
}

func findCall(calls []fakeCall, argv ...string) *fakeCall {
	for i := range calls {
		if len(calls[i].argv) < len(argv) {
			continue
		}
		match := true
		for j := range argv {
			if calls[i].argv[j] != argv[j] {
				match = false
				break
			}
		}
		if match {
			return &calls[i]
		}
	}
	return nil
}

func isolatedDBTest(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// service.env resolves through RAFIKI_ENV_FILE when it is set, which would
	// point the test at the developer's real file regardless of the config home.
	t.Setenv(paths.EnvFile, "")
}

func TestSetupDBRunningContainerIsDone(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.script(stateInspectKey(), "running\n")
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	st, res, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.Mode != installstate.DBModeManaged || res.DSN != saved {
		t.Errorf("res = %+v, want managed DSN %q", res, saved)
	}
	if st.Container != managedPGContainer || st.Image != managedPGImage {
		t.Errorf("state = %+v, want managed container metadata", st)
	}
	if findCall(r.runCalls, "docker", "run") != nil {
		t.Error("a running container must not be re-created")
	}
	if findCall(r.runCalls, "docker", "start") != nil {
		t.Error("a running container must not be started")
	}
	if len(r.streamCalls) != 0 {
		t.Error("a running container needs no health wait")
	}
}

func TestSetupDBStoppedContainerIsStarted(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.script(stateInspectKey(), "exited\n")
	r.script(healthInspectKey(), "starting\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{lines: []string{"health_status: starting", "health_status: healthy"}}
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	_, res, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.DSN != saved {
		t.Errorf("DSN = %q, want %q", res.DSN, saved)
	}
	if findCall(r.runCalls, "docker", "start", managedPGContainer) == nil {
		t.Error("an exited container must be started")
	}
	if findCall(r.runCalls, "docker", "run") != nil {
		t.Error("an existing container must not be re-created")
	}
}

func TestSetupDBVolumeWithoutPasswordRefuses(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.scriptErr(stateInspectKey(), errExit())
	r.script([]string{"docker", "volume", "inspect", managedPGVolume}, "[]")
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, "")
	if err == nil {
		t.Fatal("want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "its password is not in service.env") {
		t.Errorf("error %q must explain the missing password", err.Error())
	}
	for _, c := range r.runCalls {
		joined := runKey(c.argv)
		if strings.Contains(joined, "volume rm") || strings.Contains(joined, " rm ") {
			t.Errorf("must never delete data, but ran: %s", joined)
		}
	}
}

func TestSetupDBVolumeInspectErrorRefuses(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.scriptErr(stateInspectKey(), errExit())
	r.scriptFail([]string{"docker", "volume", "inspect", managedPGVolume}, errExit(), "Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, "")
	if err == nil {
		t.Fatal("a transient volume inspect error must not be read as volume-absent")
	}
	if !strings.Contains(err.Error(), "volume inspect") {
		t.Errorf("error %q must be the wrapped inspect failure", err.Error())
	}
	if findCall(r.runCalls, "docker", "run") != nil {
		t.Error("a transient inspect error must not lead to docker run")
	}
}

func TestSetupDBVolumeWithUnusablePasswordRefuses(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.scriptErr(stateInspectKey(), errExit())
	r.script([]string{"docker", "volume", "inspect", managedPGVolume}, "[]")
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://rafiki@127.0.0.1:5432/rafiki" // userinfo without a password
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err == nil {
		t.Fatal("want a refusal when the saved DSN has no usable password")
	}
	if !strings.Contains(err.Error(), "no usable password") {
		t.Errorf("error %q must explain the missing password", err.Error())
	}
	if findCall(r.runCalls, "docker", "run") != nil {
		t.Error("must not run docker with an empty password")
	}
}

func TestSetupDBCreatePasswordNotInArgv(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.scriptErr(stateInspectKey(), errExit())
	r.scriptFail([]string{"docker", "volume", "inspect", managedPGVolume}, errExit(), "Error: No such volume: rafiki-pgdata")
	r.script(healthInspectKey(), "starting\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{lines: []string{"health_status: healthy"}}
	r.streams[runKey(initLogsKey())] = fakeStreamScript{lines: []string{initCompleteMarker, readyMarker}}
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, res, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, "")
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}

	run := findCall(r.runCalls, "docker", "run")
	if run == nil {
		t.Fatal("a fresh install must run `docker run`")
	}
	argvJoined := strings.Join(run.argv, " ")
	if !strings.Contains(argvJoined, "-e POSTGRES_PASSWORD") {
		t.Errorf("docker run argv must carry a bare -e POSTGRES_PASSWORD: %v", run.argv)
	}
	if !strings.Contains(argvJoined, "-e POSTGRES_USER") {
		t.Errorf("docker run argv must carry a bare -e POSTGRES_USER: %v", run.argv)
	}
	if !containsString(run.env, "POSTGRES_USER=rafiki") {
		t.Errorf("env = %v, want POSTGRES_USER=rafiki beside the password", run.env)
	}
	if !strings.Contains(argvJoined, "pg_isready -U rafiki -d rafiki") {
		t.Errorf("the health check must probe the rafiki role: %v", run.argv)
	}
	if !strings.HasPrefix(res.DSN, "postgres://rafiki:") {
		t.Errorf("DSN %q must connect as the rafiki role", res.DSN)
	}

	var pw string
	for _, e := range run.env {
		if v, ok := strings.CutPrefix(e, "POSTGRES_PASSWORD="); ok {
			pw = v
		}
	}
	if pw == "" {
		t.Fatal("the password must travel in the runner env")
	}
	if len(pw) != 48 {
		t.Errorf("password hex length = %d, want 48", len(pw))
	}
	if strings.Contains(argvJoined, pw) {
		t.Error("the password must never appear in argv")
	}
	if !strings.Contains(res.DSN, pw) {
		t.Errorf("DSN %q must embed the password", res.DSN)
	}

	data, err := os.ReadFile(paths.ServiceEnvFile())
	if err != nil {
		t.Fatalf("service.env not written: %v", err)
	}
	if !strings.Contains(string(data), "RAFIKI_DB=") || !strings.Contains(string(data), res.DSN) {
		t.Errorf("service.env must record RAFIKI_DB, got %q", data)
	}
}

// The managed image's own initdb runs CREATE EXTENSION timescaledb, and that
// extension cannot be created without its library preloaded: the server FATALs
// and the container exits, which makes every fresh `rafiki setup --db-mode
// managed` fail. rafiki no longer uses TimescaleDB, but the image's preload
// list must keep it (with telemetry off, since the library is loaded again) and
// keep pg_textsearch. Pinned here because nothing else asserts this argv.
func TestSetupDBCreateArgvPreloadsWhatTheImageAndRafikiNeed(t *testing.T) {
	args := dockerRunArgs(6543)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-c shared_preload_libraries=timescaledb,pg_textsearch",
		"-c timescaledb.telemetry_level=off",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run argv must carry %q, or the image's initdb cannot create its extension: %v", want, args)
		}
	}
}

func TestSetupDBVolumeWithPasswordReusesIt(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.scriptErr(stateInspectKey(), errExit())
	r.script([]string{"docker", "volume", "inspect", managedPGVolume}, "[]")
	r.script(healthInspectKey(), "starting\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{lines: []string{"health_status: healthy"}}
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:secretpw@127.0.0.1:5432/rafiki?sslmode=disable"
	_, res, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	run := findCall(r.runCalls, "docker", "run")
	if run == nil {
		t.Fatal("expected docker run")
	}
	var pw string
	for _, e := range run.env {
		if v, ok := strings.CutPrefix(e, "POSTGRES_PASSWORD="); ok {
			pw = v
		}
	}
	if pw != "secretpw" {
		t.Errorf("password = %q, want the one from the existing volume's DSN", pw)
	}
	// The volume holds an already-initialised data dir, which POSTGRES_USER
	// never touches, so this container's role stays whatever the saved DSN
	// names — and the DSN handed back must keep that same role rather than
	// switching the install onto a role this data dir does not have.
	if !strings.HasPrefix(res.DSN, "postgres://postgres:") {
		t.Errorf("DSN = %q, want the saved DSN's postgres role kept", res.DSN)
	}
}

func TestSetupDBHealthAlreadyHealthy(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.script(stateInspectKey(), "exited\n")
	r.script(healthInspectKey(), "healthy\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{} // never yields
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if len(r.streamCalls) != 1 {
		t.Errorf("expected one docker events subscription, got %d", len(r.streamCalls))
	}
}

func TestSetupDBHealthFromEvents(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.script(stateInspectKey(), "exited\n")
	r.script(healthInspectKey(), "starting\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{lines: []string{"health_status: starting", "health_status: healthy"}}
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
}

func TestSetupDBHealthDeadline(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.script(stateInspectKey(), "exited\n")
	r.script(healthInspectKey(), "starting\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{} // never yields
	r.script(logsKey(), "boom log line\n")
	w := newSetupWizard(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	for _, want := range []string{"timed out", "boom log line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
}

// A fresh volume runs the timescaledb-ha entrypoint's TEMPORARY init server,
// which satisfies pg_isready while the real server is still restarting. The
// create path must therefore stream the entrypoint's logs and wait for the
// post-init marker before the health wait, or migrate races the restart.
func TestSetupDBFreshCreateWaitsForInitComplete(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.scriptErr(stateInspectKey(), errExit())
	r.scriptFail([]string{"docker", "volume", "inspect", managedPGVolume}, errExit(), "Error: No such volume: rafiki-pgdata")
	r.script(healthInspectKey(), "starting\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{lines: []string{"health_status: healthy"}}
	r.streams[runKey(initLogsKey())] = fakeStreamScript{lines: []string{
		"running bootstrap script",
		initCompleteMarker,
		"server started",
		readyMarker,
	}}
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, ""); err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if findStream(r.streamCalls, initLogsKey()...) == nil {
		t.Errorf("the fresh path must stream `docker logs -f` to observe initdb completion; stream calls = %v", r.streamCalls)
	}
}

// The temporary init server also prints readyMarker, and the init marker
// alone comes before the real server accepts: neither may end the wait — only
// a ready line AFTER the init marker. A wait that returned early would succeed
// where each negative case must time out.
func TestWaitForInitCompleteNeedsReadyAfterInit(t *testing.T) {
	for name, lines := range map[string][]string{
		"temp server ready only": {readyMarker, "shutting down"},
		"init marker only":       {readyMarker, initCompleteMarker, "starting PostgreSQL"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newFakeRunner()
			r.streams[runKey(initLogsKey())] = fakeStreamScript{lines: lines}
			// The fake stream stays open after its lines, as `docker logs -f`
			// does; a short deadline ends the wait the lines never satisfy.
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := waitForInitComplete(ctx, r); err == nil {
				t.Fatalf("waitForInitComplete returned before the real server was ready; lines = %q", lines)
			}
		})
	}
	r := newFakeRunner()
	r.streams[runKey(initLogsKey())] = fakeStreamScript{lines: []string{readyMarker, initCompleteMarker, readyMarker}}
	if err := waitForInitComplete(context.Background(), r); err != nil {
		t.Fatalf("a ready line after the init marker must end the wait: %v", err)
	}
}

// An existing volume runs no initdb, so the create path must not pay for a logs
// stream it has nothing to wait for.
func TestSetupDBExistingVolumeSkipsInitWait(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.scriptErr(stateInspectKey(), errExit())
	r.script([]string{"docker", "volume", "inspect", managedPGVolume}, "[]")
	r.script(healthInspectKey(), "starting\n")
	r.streams[runKey(eventsKey())] = fakeStreamScript{lines: []string{"health_status: healthy"}}
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:secretpw@127.0.0.1:5432/rafiki?sslmode=disable"
	if _, _, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved); err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if findStream(r.streamCalls, initLogsKey()...) != nil {
		t.Error("an existing volume runs no initdb, so `docker logs -f` must not be streamed")
	}
}

func TestMigrateDatabaseDSNInEnvNotArgv(t *testing.T) {
	r := newFakeRunner()
	dsn := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	if err := migrateDatabase(context.Background(), r, "/usr/local/bin/rafikid", dsn, installstate.DBModeDSN); err != nil {
		t.Fatalf("migrateDatabase: %v", err)
	}
	if len(r.runCalls) != 1 {
		t.Fatalf("expected one call, got %d", len(r.runCalls))
	}
	c := r.runCalls[0]
	if runKey(c.argv) != "/usr/local/bin/rafikid migrate" {
		t.Errorf("argv = %v, want [rafikid migrate]", c.argv)
	}
	if strings.Contains(strings.Join(c.argv, " "), dsn) || strings.Contains(strings.Join(c.argv, " "), "--db") {
		t.Error("the DSN must not appear in argv and there must be no --db")
	}
	if !containsString(c.env, "RAFIKI_DB="+dsn) {
		t.Errorf("env = %v, want RAFIKI_DB", c.env)
	}
}

func TestMigrateDatabaseReportsStderrAndHint(t *testing.T) {
	r := newFakeRunner()
	r.scripts[runKey([]string{"/usr/local/bin/rafikid", "migrate"})] = fakeScript{stderr: []byte("no such extension"), err: errExit()}
	err := migrateDatabase(context.Background(), r, "/usr/local/bin/rafikid", "postgres://x", installstate.DBModeDSN)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"no such extension", "bring-your-own database", "pg_textsearch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
}

// An embedded install needs none of the extensions a supplied DSN does, and its
// migrate is also the first binary download and initdb: the hint must point at
// that, never at extensions the operator cannot install.
func TestMigrateDatabaseEmbeddedHintIsNotAboutExtensions(t *testing.T) {
	r := newFakeRunner()
	r.scripts[runKey([]string{"/usr/local/bin/rafikid", "migrate"})] = fakeScript{stderr: []byte("checksum mismatch"), err: errExit()}
	err := migrateDatabase(context.Background(), r, "/usr/local/bin/rafikid", embeddedDSN, installstate.DBModeEmbedded)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"checksum mismatch", "download", paths.DataDir(), paths.StateDir()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
	for _, unwanted := range []string{"pg_textsearch", "bring-your-own database"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the embedded hint must not mention %q: %v", unwanted, err)
		}
	}
	if runKey(r.runCalls[0].argv) != "/usr/local/bin/rafikid migrate" {
		t.Errorf("argv = %v, want [rafikid migrate]", r.runCalls[0].argv)
	}
	if !containsString(r.runCalls[0].env, "RAFIKI_DB="+embeddedDSN) {
		t.Errorf("env = %v, want RAFIKI_DB=%s", r.runCalls[0].env, embeddedDSN)
	}
}

func TestSetupDBDSNModeSkipsDocker(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	w := newSetupWizard(t)
	dsn := "postgres://postgres:pw@db.example.com:5432/rafiki"
	if err := w.cmd.Flags().Set("dsn", dsn); err != nil {
		t.Fatal(err)
	}

	_, res, err := setupDatabase(context.Background(), r, w, installstate.DB{}, "")
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.Mode != installstate.DBModeDSN || res.DSN != dsn {
		t.Errorf("res = %+v, want DSN mode with %q", res, dsn)
	}
	if len(r.runCalls) != 0 || len(r.streamCalls) != 0 {
		t.Errorf("DSN mode must not touch docker: run=%v stream=%v", r.runCalls, r.streamCalls)
	}
}

// An install with nothing saved and no flags gets rafikid's own PostgreSQL: no
// docker, and service.env records the literal the daemon interprets.
func TestSetupDatabaseDefaultsToEmbedded(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, res, err := setupDatabase(ctx, r, w, installstate.DB{}, "")
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.Mode != installstate.DBModeEmbedded {
		t.Errorf("mode = %q, want embedded", res.Mode)
	}
	if res.DSN != embeddedDSN {
		t.Errorf("DSN = %q, want %q", res.DSN, embeddedDSN)
	}
	if st.Mode != installstate.DBModeEmbedded {
		t.Errorf("saved mode = %q, want embedded", st.Mode)
	}
	for _, c := range r.runCalls {
		if len(c.argv) > 0 && c.argv[0] == "docker" {
			t.Errorf("the embedded path must not touch docker, but ran %v", c.argv)
		}
	}
	if len(r.streamCalls) != 0 {
		t.Errorf("the embedded path must not stream, got %v", r.streamCalls)
	}
	if got := res.Describe(); !strings.Contains(got, "rafikid-managed PostgreSQL") {
		t.Errorf("Describe() = %q, want it to name the embedded server", got)
	}

	data, err := os.ReadFile(paths.ServiceEnvFile())
	if err != nil {
		t.Fatalf("service.env not written: %v", err)
	}
	// SetEnvFile quotes every value, so assert on what the file parses back to
	// rather than on the raw bytes a reader would see.
	env, err := readEnvFile(paths.ServiceEnvFile())
	if err != nil {
		t.Fatalf("readEnvFile: %v", err)
	}
	if env["RAFIKI_DB"] != embeddedDSN {
		t.Errorf("service.env RAFIKI_DB = %q, want %q (file: %q)", env["RAFIKI_DB"], embeddedDSN, data)
	}
}

// "Unset mode" is not "nothing saved": a real RAFIKI_DB in service.env is an
// install that predates db.mode (or lost its install.toml). Defaulting that to
// embedded would point the daemon at a fresh empty server and leave the real
// data behind, so the existing DSN is adopted untouched.
func TestSetupDatabaseUnsetModeKeepsAConfiguredDSN(t *testing.T) {
	isolatedDBTest(t)
	saved := "postgres://rafiki:secret@db.internal:5432/rafiki?sslmode=require"
	if _, err := paths.SetEnvFile(paths.ServiceEnvFile(), map[string]string{"RAFIKI_DB": saved}, "rafiki managed database"); err != nil {
		t.Fatalf("seed service.env: %v", err)
	}
	before, err := os.ReadFile(paths.ServiceEnvFile())
	if err != nil {
		t.Fatal(err)
	}

	r := newFakeRunner()
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, res, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.Mode != installstate.DBModeDSN || res.DSN != saved {
		t.Errorf("res = %+v, want dsn mode with the saved DSN %q", res, saved)
	}
	if st.Mode != installstate.DBModeDSN {
		t.Errorf("saved mode = %q, want dsn", st.Mode)
	}
	if len(r.runCalls) != 0 || len(r.streamCalls) != 0 {
		t.Errorf("an adopted DSN must not touch docker: run=%v stream=%v", r.runCalls, r.streamCalls)
	}
	after, err := os.ReadFile(paths.ServiceEnvFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("service.env must be left alone, but was rewritten:\nbefore %q\nafter  %q", before, after)
	}
}

// The other half of the unset-mode rule: service.env already says `embedded`,
// so the new default is what is already there and nothing is rewritten.
func TestSetupDatabaseUnsetModeWithSavedEmbeddedStaysEmbedded(t *testing.T) {
	isolatedDBTest(t)
	if _, err := paths.SetEnvFile(paths.ServiceEnvFile(), map[string]string{"RAFIKI_DB": embeddedDSN}, "rafiki embedded database"); err != nil {
		t.Fatalf("seed service.env: %v", err)
	}

	r := newFakeRunner()
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, res, err := setupDatabase(ctx, r, w, installstate.DB{}, embeddedDSN)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.Mode != installstate.DBModeEmbedded || res.DSN != embeddedDSN {
		t.Errorf("res = %+v, want embedded mode with %q", res, embeddedDSN)
	}
	if len(r.runCalls) != 0 {
		t.Errorf("the embedded path must not touch docker, ran %v", r.runCalls)
	}
	env, err := readEnvFile(paths.ServiceEnvFile())
	if err != nil {
		t.Fatalf("readEnvFile: %v", err)
	}
	if env["RAFIKI_DB"] != embeddedDSN {
		t.Errorf("service.env RAFIKI_DB = %q, want %q", env["RAFIKI_DB"], embeddedDSN)
	}
}

// A saved `managed` is a declaration: a re-run with no flags must keep the
// container path rather than migrating the operator onto the new default.
func TestSetupDatabaseSavedManagedStaysManaged(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.script(stateInspectKey(), "running\n")
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	_, res, err := setupDatabase(ctx, r, w, installstate.DB{Mode: installstate.DBModeManaged}, saved)
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.Mode != installstate.DBModeManaged || res.DSN != saved {
		t.Errorf("res = %+v, want the saved managed DSN %q", res, saved)
	}
	if findCall(r.runCalls, "docker", "inspect") == nil {
		t.Errorf("the managed path must inspect the container, run calls = %v", r.runCalls)
	}
}

func TestSetupDatabaseDBModeFlag(t *testing.T) {
	isolatedDBTest(t)

	t.Run("managed", func(t *testing.T) {
		r := newFakeRunner()
		r.script(stateInspectKey(), "running\n")
		w := newSetupWizard(t)
		if err := w.cmd.Flags().Set("db-mode", "managed"); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
		_, res, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
		if err != nil {
			t.Fatalf("setupDatabase: %v", err)
		}
		if res.Mode != installstate.DBModeManaged {
			t.Errorf("mode = %q, want managed", res.Mode)
		}
		if findCall(r.runCalls, "docker", "inspect") == nil {
			t.Errorf("--db-mode managed must run the container path, run calls = %v", r.runCalls)
		}
	})

	t.Run("bogus", func(t *testing.T) {
		r := newFakeRunner()
		w := newSetupWizard(t)
		if err := w.cmd.Flags().Set("db-mode", "bogus"); err != nil {
			t.Fatal(err)
		}

		_, _, err := setupDatabase(context.Background(), r, w, installstate.DB{}, "")
		if err == nil {
			t.Fatal("want an error for --db-mode bogus, got nil")
		}
		if err.Error() != "--db-mode must be embedded, managed or dsn" {
			t.Errorf("error = %q, want the exact --db-mode message", err.Error())
		}
		if len(r.runCalls) != 0 {
			t.Errorf("an invalid --db-mode must resolve nothing, ran %v", r.runCalls)
		}
	})

	// A typo is refused even when --dsn outranks the mode: silently ignoring a
	// value the operator typed is how a wrong database gets used for a month.
	t.Run("bogus beside dsn", func(t *testing.T) {
		r := newFakeRunner()
		w := newSetupWizard(t)
		if err := w.cmd.Flags().Set("dsn", "postgres://postgres:pw@db.example.com:5432/rafiki"); err != nil {
			t.Fatal(err)
		}
		if err := w.cmd.Flags().Set("db-mode", "bogus"); err != nil {
			t.Fatal(err)
		}

		_, _, err := setupDatabase(context.Background(), r, w, installstate.DB{}, "")
		if err == nil {
			t.Fatal("want an error for --db-mode bogus beside --dsn, got nil")
		}
		if err.Error() != "--db-mode must be embedded, managed or dsn" {
			t.Errorf("error = %q, want the exact --db-mode message", err.Error())
		}
		if len(r.runCalls) != 0 {
			t.Errorf("nothing must be resolved for an invalid --db-mode, ran %v", r.runCalls)
		}
	})
}

// --dsn is the most explicit answer, so it outranks --db-mode (including a VALID
// one: this is the `--dsn` + `--db-mode embedded` pair, which must stay in dsn
// mode rather than being refused).
func TestSetupDatabaseDSNFlagWins(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	w := newSetupWizard(t)
	dsn := "postgres://postgres:pw@db.example.com:5432/rafiki"
	if err := w.cmd.Flags().Set("dsn", dsn); err != nil {
		t.Fatal(err)
	}
	if err := w.cmd.Flags().Set("db-mode", "embedded"); err != nil {
		t.Fatal(err)
	}

	_, res, err := setupDatabase(context.Background(), r, w, installstate.DB{}, "")
	if err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if res.Mode != installstate.DBModeDSN || res.DSN != dsn {
		t.Errorf("res = %+v, want DSN mode with %q", res, dsn)
	}
	if len(r.runCalls) != 0 {
		t.Errorf("--dsn must not touch docker, ran %v", r.runCalls)
	}
}

func TestFreePortInRange(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if p < 5432 || p > 5499 {
		t.Errorf("freePort = %d, want 5432..5499", p)
	}
}

func errExit() error {
	// A non-nil error that stands in for a non-zero exit.
	return context.DeadlineExceeded
}
