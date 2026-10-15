// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
)

const (
	managedPGContainer = "rafiki-pg"
	managedPGVolume    = "rafiki-pgdata"
	// managedPGImage is pinned to an exact tag so a release that bumps it is how
	// `rafiki update` learns the container needs replacing.
	managedPGImage = "timescale/timescaledb-ha:pg18.6-ts2.30.2"
)

// dbResult is the resolved database connection.
type dbResult struct {
	DSN  string
	Mode installstate.DBMode
	Port int
}

// setupDatabase resolves how the daemon reaches postgres. Mode is `--dsn`
// (flag Changed) > st.Mode > managed. A DSN mode does no connectivity probe:
// the client must not link pgx, so migrateDatabase (which runs in rafikid) is
// the probe.
func setupDatabase(ctx context.Context, r runner, w *wizard, st installstate.DB, savedDSN string) (installstate.DB, dbResult, error) {
	mode := st.Mode
	if w.cmd.Flags().Changed("dsn") {
		mode = installstate.DBModeDSN
	}
	if mode == installstate.DBModeUnset {
		mode = installstate.DBModeManaged
	}

	if mode == installstate.DBModeDSN {
		dsn, err := w.askString("Database DSN", "dsn", savedDSN, "", true)
		if err != nil {
			return st, dbResult{}, err
		}
		st.Mode = installstate.DBModeDSN
		return st, dbResult{DSN: dsn, Mode: installstate.DBModeDSN}, nil
	}
	return setupManagedDatabase(ctx, r, w, st, savedDSN)
}

// setupManagedDatabase runs the rafiki-owned postgres container. It never
// removes the data volume: a missing RAFIKI_DB beside an existing volume is a
// refusal, not a reset.
func setupManagedDatabase(ctx context.Context, r runner, w *wizard, st installstate.DB, savedDSN string) (installstate.DB, dbResult, error) {
	st.Mode = installstate.DBModeManaged
	st.Container = managedPGContainer
	st.Volume = managedPGVolume
	st.Image = managedPGImage

	out, _, inspectErr := r.Run(ctx, nil, "docker", "inspect", "-f", "{{.State.Status}}", managedPGContainer)
	status := strings.TrimSpace(string(out))

	if inspectErr != nil {
		// No such container. An existing volume means a prior install's data.
		volumeExists := false
		if _, _, verr := r.Run(ctx, nil, "docker", "volume", "inspect", managedPGVolume); verr == nil {
			volumeExists = true
		}
		if volumeExists && savedDSN == "" {
			return st, dbResult{}, fmt.Errorf("volume %s exists but its password is not in service.env; restore RAFIKI_DB there or pass --dsn", managedPGVolume)
		}

		port := st.Port
		if port == 0 {
			p, err := freePort()
			if err != nil {
				return st, dbResult{}, err
			}
			port = p
		}

		var pw string
		if volumeExists {
			pw = passwordFromDSN(savedDSN)
		} else {
			p, err := randomPassword()
			if err != nil {
				return st, dbResult{}, err
			}
			pw = p
		}

		env := []string{"POSTGRES_PASSWORD=" + pw}
		if _, stderr, rerr := r.Run(ctx, env, dockerRunArgs(port)...); rerr != nil {
			return st, dbResult{}, fmt.Errorf("docker run %s: %s", managedPGContainer, strings.TrimSpace(string(stderr)))
		}
		st.Port = port
		if err := waitForHealthy(ctx, r); err != nil {
			return st, dbResult{}, err
		}
		dsn := managedDSN(pw, port)
		if _, err := paths.SetEnvFile(paths.ServiceEnvFile(), map[string]string{"RAFIKI_DB": dsn}, "rafiki managed database"); err != nil {
			return st, dbResult{}, err
		}
		return st, dbResult{DSN: dsn, Mode: installstate.DBModeManaged, Port: port}, nil
	}

	switch status {
	case "running":
		if savedDSN == "" {
			return st, dbResult{}, fmt.Errorf("%s is running but service.env has no RAFIKI_DB; add it or remove the container (its data is in volume %s)", managedPGContainer, managedPGVolume)
		}
		return st, dbResult{DSN: savedDSN, Mode: installstate.DBModeManaged, Port: st.Port}, nil
	case "exited", "created":
		if _, stderr, serr := r.Run(ctx, nil, "docker", "start", managedPGContainer); serr != nil {
			return st, dbResult{}, fmt.Errorf("docker start %s: %s", managedPGContainer, strings.TrimSpace(string(stderr)))
		}
		if err := waitForHealthy(ctx, r); err != nil {
			return st, dbResult{}, err
		}
		if savedDSN == "" {
			return st, dbResult{}, fmt.Errorf("%s is running but service.env has no RAFIKI_DB; add it or remove the container (its data is in volume %s)", managedPGContainer, managedPGVolume)
		}
		return st, dbResult{DSN: savedDSN, Mode: installstate.DBModeManaged, Port: st.Port}, nil
	default:
		return st, dbResult{}, fmt.Errorf("container %s is in unexpected state %q", managedPGContainer, status)
	}
}

// dockerRunArgs is the `docker run` argv. POSTGRES_PASSWORD is a bare -e so the
// value never lands in argv; the caller passes it through the runner's env.
func dockerRunArgs(port int) []string {
	return []string{
		"docker", "run", "-d",
		"--name", managedPGContainer,
		"--restart", "unless-stopped",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"-v", managedPGVolume + ":/home/postgres/pgdata",
		"-e", "POSTGRES_PASSWORD",
		"-e", "POSTGRES_DB=rafiki",
		"--health-cmd", "pg_isready -U postgres -d rafiki",
		"--health-interval", "1s",
		"--health-retries", "120",
		managedPGImage,
		"postgres",
		"-c", "shared_preload_libraries=timescaledb,pg_textsearch",
		"-c", "timescaledb.telemetry_level=off",
	}
}

// waitForHealthy subscribes to docker's health events before inspecting, so a
// transition that happens between the inspect and the first event is not lost.
// It blocks, via the stream, rather than polling.
func waitForHealthy(ctx context.Context, r runner) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	reader, err := r.Stream(ctx,
		"docker", "events",
		"--filter", "container="+managedPGContainer,
		"--filter", "event=health_status",
		"--format", "{{.Status}}",
	)
	if err != nil {
		return fmt.Errorf("docker events: %w", err)
	}
	defer func() { _ = reader.Close() }()

	out, _, _ := r.Run(ctx, nil, "docker", "inspect", "-f", "{{.State.Health.Status}}", managedPGContainer)
	if strings.TrimSpace(string(out)) == "healthy" {
		return nil
	}

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "health_status: healthy" {
			return nil
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("timed out waiting for %s to become healthy: %w\n%s", managedPGContainer, ctx.Err(), lastDockerLogs(ctx, r))
	}
	return fmt.Errorf("docker events stream ended before %s became healthy\n%s", managedPGContainer, lastDockerLogs(ctx, r))
}

// lastDockerLogs returns the tail of the container's logs for a failure
// message. It uses a fresh context: the caller's may already be expired, and a
// cancelled exec would return nothing.
func lastDockerLogs(ctx context.Context, r runner) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	out, _, err := r.Run(ctx, nil, "docker", "logs", "--tail", "20", managedPGContainer)
	if err != nil {
		return ""
	}
	return string(out)
}

// migrateDatabase runs rafikid migrate against dsn. The DSN travels in the
// environment (RAFIKI_DB), never in argv.
func migrateDatabase(ctx context.Context, r runner, rafikidPath, dsn string) error {
	_, stderr, err := r.Run(ctx, []string{"RAFIKI_DB=" + dsn}, rafikidPath, "migrate")
	if err != nil {
		return fmt.Errorf("%w\n%s\nhint: a bring-your-own database needs the timescaledb, vector and pg_textsearch extensions and shared_preload_libraries=timescaledb,pg_textsearch", err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// managedDSN builds the connection string for the rafiki-owned container.
func managedDSN(password string, port int) string {
	return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/rafiki?sslmode=disable", password, port)
}

// passwordFromDSN extracts the password from a postgres DSN, so an existing
// volume's container can be recreated with the same credentials.
func passwordFromDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

// randomPassword returns 24 random bytes hex-encoded.
func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// freePort returns the first port in 5432..5499 that can be listened on.
func freePort() (int, error) {
	for p := 5432; p <= 5499; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		_ = l.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free port in 5432..5499")
}
