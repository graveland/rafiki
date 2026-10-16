// SPDX-License-Identifier: Apache-2.0

package main

import (
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
}

func TestSetupDBRunningContainerIsDone(t *testing.T) {
	isolatedDBTest(t)
	r := newFakeRunner()
	r.script(stateInspectKey(), "running\n")
	w := newSetupWizard(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	st, res, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
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
	_, res, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
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

	_, _, err := setupDatabase(ctx, r, w, installstate.DB{}, "")
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

	_, _, err := setupDatabase(ctx, r, w, installstate.DB{}, "")
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
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
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

	_, res, err := setupDatabase(ctx, r, w, installstate.DB{}, "")
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
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
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
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
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
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
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
	_, _, err := setupDatabase(ctx, r, w, installstate.DB{}, saved)
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

	if _, _, err := setupDatabase(ctx, r, w, installstate.DB{}, ""); err != nil {
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
	if _, _, err := setupDatabase(ctx, r, w, installstate.DB{}, saved); err != nil {
		t.Fatalf("setupDatabase: %v", err)
	}
	if findStream(r.streamCalls, initLogsKey()...) != nil {
		t.Error("an existing volume runs no initdb, so `docker logs -f` must not be streamed")
	}
}

func TestMigrateDatabaseDSNInEnvNotArgv(t *testing.T) {
	r := newFakeRunner()
	dsn := "postgres://postgres:pw@127.0.0.1:5432/rafiki?sslmode=disable"
	if err := migrateDatabase(context.Background(), r, "/usr/local/bin/rafikid", dsn); err != nil {
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
	err := migrateDatabase(context.Background(), r, "/usr/local/bin/rafikid", "postgres://x")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"no such extension", "bring-your-own database", "pg_textsearch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
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
