// SPDX-License-Identifier: Apache-2.0

package integration_test

// The embedded-database end-to-end proof: RAFIKI_DB=embedded makes rafikid run
// its own PostgreSQL (pkg/embedpg), and this is the only test that exercises a
// REAL extracted server — a real migration chain applied to it, a real daemon
// serving from it, a real child turn written to it, and the degradation that
// server's missing extensions impose on recall.
//
// Gated on RAFIKI_TEST_EMBEDPG=1 because the first run downloads the pinned
// binaries from Maven Central (~15 MB for one platform, cached under
// $XDG_CACHE_HOME/rafiki-test-embedpg so later runs reuse them). -count=1
// always: go test caching cannot see through to a daemon subprocess.
//
// Nothing here touches the suite's shared RAFIKI_TEST_DSN database: every XDG
// base points into this test's own /tmp tree, so the cluster, its socket and
// the daemon's state are thrown away with it.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/embedpg"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// embeddedDBMode is the RAFIKI_DB value that makes rafikid run its own server.
// Spelled out rather than imported: this test is the pin on the CLI's spelling
// of it, so a rename must reach here deliberately.
const embeddedDBMode = "embedded"

// embeddedDirs is the temp tree this test owns.
//
// One SHORT root under /tmp, not t.TempDir(): the server's only socket lives
// inside its data directory, and sun_path is 104 bytes on macOS — t.TempDir()
// resolves through /private/var/folders/… and would be refused by Ensure
// before it did anything. cache is shared with later runs on purpose: it is
// the downloaded binaries, refetched only when absent.
type embeddedDirs struct {
	root  string // the whole tree; removed on cleanup
	home  string // HOME and XDG_RUNTIME_DIR: the daemon's socket, config, state
	data  string // XDG_DATA_HOME: the private server's data directory
	state string // XDG_STATE_HOME: the server log and the daemon's logs
	cache string // XDG_CACHE_HOME: the extracted PostgreSQL binaries
}

func newEmbeddedDirs(t *testing.T) embeddedDirs {
	t.Helper()
	c := assert.NewAborting(t)
	// os.UserCacheDir() must be read BEFORE the test sets XDG_CACHE_HOME,
	// or it returns the scratch dir it is about to be pointed at.
	cache, err := os.UserCacheDir()
	c.NoError(err, "UserCacheDir")
	root, err := os.MkdirTemp("/tmp", "epg")
	c.NoError(err, "MkdirTemp")
	return embeddedDirs{
		root:  root,
		home:  filepath.Join(root, "home"),
		data:  filepath.Join(root, "data"),
		state: filepath.Join(root, "state"),
		cache: filepath.Join(cache, "rafiki-test-embedpg"),
	}
}

// env is what every subprocess in this test runs with.
//
// os.Environ() first and the explicit entries after: exec.Cmd.Env takes the
// LAST value for a duplicate key, so a developer's ambient RAFIKI_DB (or a
// stale XDG_CACHE_HOME pointing at another user's binaries) cannot redirect
// this test. The three XDG bases are the same ones the test process was given,
// because they are what decides WHERE the private server lives — a daemon
// booted with its own would start a second cluster and migrate nothing.
func (d embeddedDirs) env() []string {
	return append(os.Environ(),
		"HOME="+d.home,
		"XDG_DATA_HOME="+d.data,
		"XDG_STATE_HOME="+d.state,
		"XDG_CACHE_HOME="+d.cache,
		"XDG_RUNTIME_DIR="+d.home,
		"RAFIKI_DB="+embeddedDBMode,
	)
}

// runRafikidEmbedded runs one rafikid subcommand against the embedded database
// and returns its combined output. daemonBinary(), never binaryPath: the
// helper is what makes the go test cache see the daemon's sources.
func runRafikidEmbedded(env []string, args ...string) (string, error) {
	cmd := exec.Command(daemonBinary(), args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// bootEmbeddedDaemon starts rafikid against the embedded database d.env()
// names. It mirrors bootDaemon's isolation — scratch HOME, ephemeral proxy
// port, executors off, no real provider credentials — with one deliberate
// difference: the XDG bases come from env, not from a fresh temp HOME.
//
// extraEnv is appended last, so it wins over every default here (exec.Cmd.Env
// keeps the last value for a duplicate key).
func bootEmbeddedDaemon(t *testing.T, d embeddedDirs, extraEnv ...string) *daemon {
	t.Helper()
	c := assert.NewAborting(t)

	// paths.RuntimeDir() is $XDG_RUNTIME_DIR/rafiki when that is absolute, so
	// the socket lands inside this test's tree — and short enough for sun_path.
	socketPath := filepath.Join(d.home, "rafiki", "controller.sock")
	c.True(len(socketPath) <= 100, "socket path too long (%d bytes) for UDS: %s", len(socketPath), socketPath)

	stderr := &stderrBuf{}
	cmd := exec.Command(daemonBinary())
	cmd.Stderr = stderr
	cmd.Env = append(d.env(),
		// Executors are the CONTAINER plane, and this test spawns a fundi child,
		// which runs in-process. A pool with zero live executors would only add
		// noise to a degraded-boot diagnosis.
		"RAFIKI_EXECUTORS_ENABLED=0",
		// Ephemeral loopback port: a developer's own `rafiki serve` on :8035
		// must not be able to degrade this daemon.
		"RAFIKI_PROXY_LISTEN=127.0.0.1:0",
		"RAFIKI_DAEMON_ID=embedded-pg-test",
	)
	cmd.Env = append(cmd.Env, noRealProviderEnv()...)
	cmd.Env = append(cmd.Env, extraEnv...)

	c.NoError(cmd.Start(), "start daemon")
	daemon := &daemon{
		socketPath: socketPath,
		proc:       cmd,
		homeDir:    d.home,
		logsDir:    filepath.Join(d.state, "rafiki", "logs"), // paths.LogsDir() == StateDir/logs
		stderr:     stderr,
	}
	// Registered before the waits, so a failed boot still stops the process.
	// The temp tree itself is removed by the test's own (earlier-registered,
	// therefore later-running) cleanup.
	t.Cleanup(daemon.stopDaemonNoRemove)

	// Dialing, not stat-ing, is the only proof something is behind the socket.
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			_ = conn.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	c.NoError(lastErr, "daemon never accepted on %s: %v\nstderr:\n%s", socketPath, lastErr, stderr.tail(4000))

	// The proxy face announces itself once it is serving, and the Connect
	// listener is only wired when the face comes up — so this wait, not the
	// socket wait, is what gates the surface below.
	daemon.proxyURL = waitProxyListen(t, stderr)
	return daemon
}

// userTokenFromCreate pulls the one-time token out of `rafikid user create
// --token` output ("username: …\ntoken: …").
func userTokenFromCreate(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if tok, ok := strings.CutPrefix(strings.TrimSpace(line), "token: "); ok && tok != "" {
			return tok, true
		}
	}
	return "", false
}

// stopEmbeddedServer stops the private server and reports whether it is now
// stopped. Nothing to stop is success: a boot that never reached the start (or
// one that already stopped) must not fail the cleanup.
//
// "Nothing to stop" is asked of the data directory, not of an exit code. pg_ctl
// STOP exits 1 — "PID file ... does not exist / Is server running?" — when the
// server is not running; only pg_ctl STATUS uses 3 for that. A postmaster.pid is
// the only thing pg_ctl itself consults, so its absence is the same answer, and
// it also covers a cluster that was never initialised.
//
// A real failure FAILS the test and returns false, so the caller keeps the data
// directory. A postmaster left running over a deleted PGDATA is the one outcome
// nobody can recover from — the tree is what lets a human finish the job — so it
// must be loud rather than a log line on a green run.
//
// The stop is bounded twice: pg_ctl's own -t (it waits for the postmaster to
// exit) and a context deadline, because a postmaster that will not come down
// must not hang the suite's cleanup.
func stopEmbeddedServer(t *testing.T, server embedpg.Server) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Join(server.DataDir, "postmaster.pid")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stat the postmaster pid file: %v", err)
			return false
		}
		return true // no postmaster.pid: no server pg_ctl could stop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	pgctl := filepath.Join(server.BinDir, "bin", "pg_ctl")
	out, err := exec.CommandContext(ctx, pgctl,
		"stop", "-D", server.DataDir, "-m", "fast", "-t", "30",
	).CombinedOutput()
	if err == nil {
		return true
	}
	t.Errorf("pg_ctl stop failed: %v: %s\nleft %s in place (not removed); finish with: %s stop -D %s",
		err, strings.TrimSpace(string(out)), server.DataDir, pgctl, server.DataDir)
	return false
}

// queryStrings runs a two-column query into a map, failing the test on error.
func queryStrings(t *testing.T, pool *pgxpool.Pool, what, sql string) map[string]string {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, sql)
	c.NoError(err, "%s", what)
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var k, v string
		c.NoError(rows.Scan(&k, &v), "%s", what)
		got[k] = v
	}
	c.NoError(rows.Err(), "%s", what)
	return got
}

// queryInt runs a one-column integer query.
func queryInt(t *testing.T, pool *pgxpool.Pool, what, sql string, args ...any) int64 {
	t.Helper()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var n int64
	c.NoError(pool.QueryRow(ctx, sql, args...).Scan(&n), "%s", what)
	return n
}

// TestEmbeddedPostgresMigratesAndServes is the design's end-to-end claim, on a
// real server: RAFIKI_DB=embedded starts a private PostgreSQL that has none of
// timescaledb/vector/pg_textsearch, the migration chain still applies cleanly
// to it, the daemon serves from it, recall search degrades to Unavailable
// naming the missing extension, and memories keep working.
func TestEmbeddedPostgresMigratesAndServes(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs the embedded-postgres end-to-end test (downloads ~15 MB once)")
	}
	c := assert.NewAborting(t)

	dirs := newEmbeddedDirs(t)

	// The test process must compute the same paths as every subprocess:
	// embedpg.Default() resolves all three bases through these variables.
	t.Setenv("XDG_DATA_HOME", dirs.data)
	t.Setenv("XDG_STATE_HOME", dirs.state)
	t.Setenv("XDG_CACHE_HOME", dirs.cache)

	server := embedpg.Default()

	// Registered FIRST, so it runs LAST — after the daemon's cleanup has
	// stopped the daemon. rafikid never stops the private server; the test
	// that started it is the only thing that will.
	t.Cleanup(func() {
		if !stopEmbeddedServer(t, server) {
			return // the data dir is what lets a human finish the job
		}
		if err := os.RemoveAll(dirs.root); err != nil {
			t.Logf("remove %s: %v", dirs.root, err)
		}
	})

	env := dirs.env()

	// ── the migration chain applies to a server with no extensions ────────
	//
	// 0001/0009/0010 create plain tables instead of hypertables when
	// timescaledb is absent, and 0036 keeps bytea embedding placeholders and
	// skips the bm25 indexes when vector/pg_textsearch are. A chain that still
	// assumed them would fail here, on the first CREATE TABLE, rather than in
	// production.
	//
	// The SECOND run is a separate claim: Ensure must reuse the running server
	// (flock, then pg_ctl status) rather than starting a second cluster on the
	// same data directory.
	for run := 1; run <= 2; run++ {
		out, err := runRafikidEmbedded(env, "migrate")
		c.NoError(err, "rafikid migrate (run %d) failed: %v\noutput: %s", run, err, out)
	}

	// ── what the server actually is ──────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn := server.DSN()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "open the embedded server")
	defer pool.Close()

	c.Eq(int64(0), queryInt(t, pool, "count the extensions an embedded install must not have",
		`SELECT count(*) FROM pg_extension WHERE extname IN ('timescaledb','vector','pg_textsearch')`),
		"the embedded server must have none of timescaledb, vector or pg_textsearch")
	// ltree is unconditional (0036 creates it), so its presence is what makes
	// the migrations' conditional blocks a real test of the conditionals rather
	// than of a database that creates no extension at all.
	c.Eq(int64(1), queryInt(t, pool, "count ltree",
		`SELECT count(*) FROM pg_extension WHERE extname = 'ltree'`),
		"ltree must be installed: 0036 creates it unconditionally")

	// The pinned binaries are really PostgreSQL 18 — the major the data
	// directory's PG_VERSION is checked against.
	c.Eq(int64(18), queryInt(t, pool, "server major version",
		`SELECT current_setting('server_version_num')::int / 10000`),
		"the embedded server must be PostgreSQL 18")

	// The chain ran HERE, on this server, and it ran whole: one row per applied
	// version, contiguous from the baseline, so the row count IS the head
	// version (CLAUDE.md: the chain is 1..N with no gaps).
	applied := queryInt(t, pool, "applied migrations", `SELECT count(*) FROM public.rafiki_schema_migrations`)
	c.True(applied > 0, "the migration chain recorded nothing")
	c.Eq(applied, queryInt(t, pool, "highest applied version", `SELECT max(version) FROM public.rafiki_schema_migrations`),
		"the applied chain is not contiguous from the baseline")

	relkind := queryStrings(t, pool, "relkind of the three formerly-hypertable tables", `
		SELECT n.nspname || '.' || c.relname, c.relkind::text
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE (n.nspname, c.relname) IN (
		       ('conversations', 'conversation_turn'),
		       ('openrouter',    'broadcast'),
		       ('conversations', 'raw_http_request'))`)
	c.Eq(3, len(relkind), "expected all three tables to exist, got %v", relkind)
	for _, name := range []string{
		"conversations.conversation_turn",
		"openrouter.broadcast",
		"conversations.raw_http_request",
	} {
		c.Eq("r", relkind[name], "%s is relkind %q, want a plain table 'r'", name, relkind[name])
	}

	// relkind 'r' alone cannot tell a plain table from a hypertable's parent
	// table, so the absence of the catalog is what makes the loop above a claim
	// about plain tables rather than a restatement of the extension count.
	c.Eq(int64(0), queryInt(t, pool, "count the timescaledb catalog schema",
		`SELECT count(*) FROM pg_namespace WHERE nspname = '_timescaledb_catalog'`),
		"no timescaledb catalog schema may exist: nothing here is a hypertable")

	embeddingType := queryStrings(t, pool, "embedding column types", `
		SELECT c.relname, format_type(a.atttypid, a.atttypmod)
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'conversations'
		   AND a.attname = 'embedding'
		   AND c.relname IN ('memory', 'conversation_window', 'conversation_summary')`)
	c.Eq(3, len(embeddingType), "expected an embedding column on all three recall tables, got %v", embeddingType)
	for _, table := range []string{"memory", "conversation_window", "conversation_summary"} {
		c.Eq("bytea", embeddingType[table], "conversations.%s.embedding is %q, want the bytea placeholder", table, embeddingType[table])
	}

	c.Eq(int64(0), queryInt(t, pool, "count the bm25 indexes",
		`SELECT count(*) FROM pg_indexes
		  WHERE indexname IN ('memory_bm25','conversation_window_bm25','conversation_summary_bm25')`),
		"no bm25 index may exist without the pg_textsearch access method")

	// The data directory is the access control for this server, and the
	// socket inside it is the only way in.
	fi, err := os.Stat(server.DataDir)
	c.NoError(err, "stat the data dir")
	c.Eq(os.FileMode(0o700), fi.Mode().Perm(), "the data dir is the access control and must be 0700")
	c.FileExists(filepath.Join(server.DataDir, ".s.PGSQL."+strconv.Itoa(embedpg.Port)),
		"the unix socket must live in the data dir")
	var listen string
	c.NoError(pool.QueryRow(ctx, "SHOW listen_addresses").Scan(&listen), "SHOW listen_addresses")
	c.Eq("", listen, "the server must listen on no TCP socket")

	// ── a user identity, because a memory has an owner ───────────────────
	//
	// The memory verbs are owner-scoped and the wire carries no owner: an
	// anonymous caller cannot write one. This is the same recovery CLI an
	// operator uses — it opens the DSN itself (resolveDSN, so it reuses the
	// running server) and prints the token exactly once.
	const username = "epg"
	out, err := runRafikidEmbedded(env, "user", "create", username, "--token")
	c.NoError(err, "rafikid user create %s: %v\noutput: %s", username, err, out)
	token, ok := userTokenFromCreate(out)
	c.Require().True(ok, "no token in `rafikid user create --token` output: %s", out)

	// ── the daemon serves from it ────────────────────────────────────────
	//
	// The model is the suite's fake Anthropic SSE server, wired through a
	// providers.toml the daemon reads from RAFIKI_PROVIDERS, so nothing here can
	// reach a real provider. release is closed so every request is answered
	// immediately — the same fake blocks instead, elsewhere.
	fake := newBlockingFakeLLM(t)
	close(fake.release)
	d := bootEmbeddedDaemon(t, dirs, "RAFIKI_PROVIDERS="+writeFakeSSEProviders(t, fake.srv.URL))
	client := d.control(t)

	// Search is off: startRecall probed pg_extension once at startup, so the
	// refusal is a decision about THIS database, not a failed query. The code
	// is Unavailable (the deployment's gap, not the caller's) and the text
	// names the extension, which is the whole remedy.
	_, err = client.Recall(ctx, authorize(token, connect.NewRequest(&rafikiv1.RecallRequest{
		Query: "embedded postgres",
	})))
	c.Error(err, "Recall must be refused on a database without pg_textsearch")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err),
		"Recall code = %v, want %v: %v", connect.CodeOf(err), connect.CodeUnavailable, err)
	c.ErrorContains(err, "pg_textsearch", "the refusal must name the missing extension: %v", err)

	// Memories are plain SQL and do not go through search, so they keep
	// working on the same database — this is the degradation's other half.
	const (
		memPath = "embedded_pg"
		memName = "roundtrip"
		memBody = "the embedded server keeps memories even though it cannot search"
	)
	_, err = client.PutMemory(ctx, authorize(token, connect.NewRequest(&rafikiv1.PutMemoryRequest{
		Path: memPath, Name: memName, Body: memBody,
	})))
	c.NoError(err, "PutMemory on the embedded server")
	got, err := client.GetMemory(ctx, authorize(token, connect.NewRequest(&rafikiv1.GetMemoryRequest{
		Path: memPath, Name: memName,
	})))
	c.NoError(err, "GetMemory on the embedded server")
	c.Eq(memBody, got.Msg.GetMemory().GetBody(), "the memory body did not round-trip")

	// ── a child spawns and completes a turn ──────────────────────────────
	//
	// Everything above is the schema; this is the RUNTIME writing to it, which is
	// where a Timescale-only assumption would break first: a write-ahead
	// conversation_turn row (PRIMARY KEY (id, created_at)), the conversation
	// tree, the child row, and — because spawnResumeChild sets RecordRequests and
	// the daemon builds its raw-trace store whenever it has a pool — a
	// raw_http_request row (source 'fundi'). None of it needs timescaledb.
	childID := spawnResumeChild(t, d, "fakellm/mini")
	waitForStatus(t, d, childID, "idle", 30*time.Second)
	sendPrompt(t, client, childID, "say hi")

	// Poll for the ROWS, not for the status: the child is idle both before and
	// after its turn, so a status wait would pass instantly on the wrong edge.
	// The turn row is the proof the turn happened at all, and the raw trace
	// insert runs on its own goroutine (recordRawTrace detaches it), so neither
	// can be read once.
	countFor := func(sql string) (int64, error) {
		var n int64
		err := pool.QueryRow(ctx, sql, childID).Scan(&n)
		return n, err
	}
	turnSQL := `SELECT count(*) FROM conversations.conversation_turn t
	              JOIN conversations.child ch ON ch.conversation_id = t.conversation_id
	             WHERE ch.child_id = $1`
	c.Eventually(30*time.Second, "the turn is recorded in conversation_turn", func() error {
		n, err := countFor(turnSQL)
		if err != nil {
			return err
		}
		if n < 1 {
			return fmt.Errorf("no conversation_turn row for child %s yet", childID)
		}
		return nil
	})
	// The fundi writer of the raw trace (pkg/llm's recordRawTrace) is what a
	// --record-requests spawn turns on; the row is the evidence that the plain
	// raw_http_request table accepts its insert.
	rawSQL := `SELECT count(*) FROM conversations.raw_http_request r
	             JOIN conversations.child ch ON ch.conversation_id = r.conversation_id
	            WHERE ch.child_id = $1`
	c.Eventually(30*time.Second, "the request is recorded in raw_http_request", func() error {
		n, err := countFor(rawSQL)
		if err != nil {
			return err
		}
		if n < 1 {
			return fmt.Errorf("no raw_http_request row for child %s yet", childID)
		}
		return nil
	})
	// Exactly one turn: it closed cleanly (the child is back to idle, not
	// failed) and nothing wrote the row twice.
	waitForStatus(t, d, childID, "idle", 30*time.Second)
	c.Eq(int64(1), queryInt(t, pool, "turns recorded for the child's conversation", turnSQL, childID),
		"the fundi turn must be recorded exactly once in conversation_turn")
}
