// SPDX-License-Identifier: Apache-2.0

package integration_test

// The embedded-database end-to-end proof: RAFIKI_DB=embedded makes rafikid run
// its own PostgreSQL (pkg/embedpg), and this is the only test that exercises a
// REAL extracted server — a real migration chain applied to it, a real daemon
// serving from it, a real child turn written to it, recall search through the
// bm25 index, and the migration branch for a server without the recall
// extensions.
//
// Gated on RAFIKI_TEST_EMBEDPG=1 because the first run downloads the pinned
// artifact from the embedpg release (17-70 MB for one platform, cached under
// $XDG_CACHE_HOME/rafiki-test-embedpg so later runs reuse them). -count=1
// always: go test caching cannot see through to a daemon subprocess.
//
// Nothing here touches the suite's shared RAFIKI_TEST_DSN database: every XDG
// base points into this test's own /tmp tree, so the cluster, its socket and
// the daemon's state are thrown away with it. The one test that does reach that
// server — the dump backup round trip on a server with timescaledb installed —
// creates and drops a scratch database of its own and never reads or writes
// rafiki_test's rows.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
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
	"go.graveland.dev/rafiki/pkg/store"

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

// runRafikid runs one rafikid subcommand with env and returns its combined
// output. daemonBinary(), never binaryPath: the helper is what makes the go
// test cache see the daemon's sources.
func runRafikid(env []string, args ...string) (string, error) {
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
// real server: RAFIKI_DB=embedded starts a private PostgreSQL that ships vector
// and pg_textsearch but not timescaledb, the migration chain applies cleanly to
// it (taking the extensions-available branch of 0036), the daemon serves from
// it, and recall search finds a memory through the bm25 index.
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

	// ── the migration chain applies to a server with vector and pg_textsearch ─
	//
	// 0001/0009/0010 create plain tables instead of hypertables when
	// timescaledb is absent, and 0036 converts the embedding columns to vector
	// and builds the bm25 indexes because vector/pg_textsearch ARE available. A
	// chain that assumed timescaledb would fail here, on the first CREATE TABLE,
	// rather than in production; the branch without them is pinned by
	// TestEmbeddedPostgresMigratesWithoutExtensions.
	//
	// The SECOND run is a separate claim: Ensure must reuse the running server
	// (flock, then pg_ctl status) rather than starting a second cluster on the
	// same data directory.
	for run := 1; run <= 2; run++ {
		out, err := runRafikid(env, "migrate")
		c.NoError(err, "rafikid migrate (run %d) failed: %v\noutput: %s", run, err, out)
	}

	// ── what the server actually is ──────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn := server.DSN()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "open the embedded server")
	defer pool.Close()

	// resolveDSN hands every rafikid path — the daemon included — this DSN, and
	// it names `rafiki`, not initdb's maintenance database. An install that
	// migrated `postgres` instead would serve from the wrong one.
	var servedDB string
	c.NoError(pool.QueryRow(ctx, "SELECT current_database()").Scan(&servedDB), "current_database")
	c.Eq(embedpg.Database, servedDB, "the daemon's database must be %q", embedpg.Database)

	c.Eq(int64(0), queryInt(t, pool, "count timescaledb",
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'`),
		"the embedded server must not have timescaledb")
	c.Eq(int64(2), queryInt(t, pool, "count the extensions an embedded install ships",
		`SELECT count(*) FROM pg_extension WHERE extname IN ('vector','pg_textsearch')`),
		"the embedded server must have both vector and pg_textsearch")
	// ltree is unconditional (0036 creates it), whereas vector and pg_textsearch
	// are created only because the server lists them as available.
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
		c.Eq("vector", embeddingType[table], "conversations.%s.embedding is %q, want vector", table, embeddingType[table])
	}

	c.Eq(int64(3), queryInt(t, pool, "count the bm25 indexes",
		`SELECT count(*) FROM pg_indexes
		  WHERE indexname IN ('memory_bm25','conversation_window_bm25','conversation_summary_bm25')`),
		"all three bm25 indexes must exist: pg_textsearch is installed")

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
	out, err := runRafikid(env, "user", "create", username, "--token")
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

	const (
		memPath = "embedded_pg"
		memName = "roundtrip"
		memBody = "the embedded server keeps memories and searches them through the bm25 index"
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

	// Search is on: startRecall probed pg_extension at startup and found
	// pg_textsearch, so the memory just written is found through the bm25 index
	// rather than refused as Unavailable (what a server without the extension
	// answers). Eventually, because the daemon's recall runtime is built
	// asynchronously after the control socket opens.
	c.Eventually(30*time.Second, "recall finds the memory through bm25", func() error {
		resp, err := client.Recall(ctx, authorize(token, connect.NewRequest(&rafikiv1.RecallRequest{
			Query:   "bm25 index",
			Sources: []string{"memory"},
		})))
		if err != nil {
			return fmt.Errorf("Recall: %w", err)
		}
		for _, h := range resp.Msg.GetHits() {
			if h.GetPath() == memPath && h.GetName() == memName {
				return nil
			}
		}
		return fmt.Errorf("no hit for %s/%s among %d hit(s)", memPath, memName, len(resp.Msg.GetHits()))
	})

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

// copyEmbeddedTree copies the extracted tree at src to dst, recreating symlinks
// as symlinks, leaving out every entry skip names (a slash-separated path
// relative to src; a skipped directory is skipped whole).
func copyEmbeddedTree(t *testing.T, src, dst string, skip func(rel string) bool) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o700)
		}
		if skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		default:
			return fmt.Errorf("unsupported entry %s (%s)", rel, d.Type())
		}
	})
	assert.NewAborting(t).NoError(err, "copy the embedded tree")
}

// TestEmbeddedPostgresMigratesWithoutExtensions pins the branch of the
// migrations' extension conditionals that TestEmbeddedPostgresMigratesAndServes
// does not take: a server whose share/postgresql/extension lists neither vector
// nor pg_textsearch — what an install on plain PostgreSQL looks like — still
// migrates, keeps bytea embedding placeholders and builds no bm25 index.
//
// It works on a COPY of the cached tree with those extensions' control and SQL
// files removed, in a cache of its own: the shared cache is what every other
// test in this file reuses, and must not be edited.
func TestEmbeddedPostgresMigratesWithoutExtensions(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs the embedded-postgres end-to-end test (downloads the pinned artifact once)")
	}
	c := assert.NewAborting(t)

	dirs := newEmbeddedDirs(t)
	t.Setenv("XDG_DATA_HOME", dirs.data)
	t.Setenv("XDG_STATE_HOME", dirs.state)

	// The shared tree, fetched if this is the first run: Ensure on a throwaway
	// data directory does the download and extraction, and nothing else is kept.
	t.Setenv("XDG_CACHE_HOME", dirs.cache)
	shared := embedpg.Default().BinDir
	if _, err := os.Stat(filepath.Join(shared, "bin", "postgres")); err != nil {
		prefetch := embedpg.Server{
			BinDir:   shared,
			DataDir:  filepath.Join(dirs.root, "prefetch", "pg"),
			LogPath:  filepath.Join(dirs.root, "prefetch", "pg.log"),
			LockPath: filepath.Join(dirs.root, "prefetch", "pg.lock"),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		_, err := prefetch.Ensure(ctx)
		cancel()
		c.NoError(err, "fetch the embedded binaries")
		c.True(stopEmbeddedServer(t, prefetch), "stop the prefetch server")
		c.NoError(os.RemoveAll(filepath.Dir(prefetch.DataDir)), "remove the prefetch data directory")
	}

	// This test's own cache, holding the copy.
	dirs.cache = filepath.Join(dirs.root, "cache")
	t.Setenv("XDG_CACHE_HOME", dirs.cache)
	server := embedpg.Default()
	copyEmbeddedTree(t, shared, server.BinDir, func(rel string) bool {
		base := filepath.Base(rel)
		return filepath.Dir(rel) == "share/postgresql/extension" &&
			(strings.HasPrefix(base, "vector") || strings.HasPrefix(base, "pg_textsearch"))
	})

	t.Cleanup(func() {
		if !stopEmbeddedServer(t, server) {
			return // the data dir is what lets a human finish the job
		}
		if err := os.RemoveAll(dirs.root); err != nil {
			t.Logf("remove %s: %v", dirs.root, err)
		}
	})

	out, err := runRafikid(dirs.env(), "migrate")
	c.NoError(err, "rafikid migrate on a server without the recall extensions: %v\noutput: %s", err, out)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, server.DSN())
	c.NoError(err, "open the embedded server")
	defer pool.Close()

	c.Eq(int64(0), queryInt(t, pool, "count the extensions this server must not list",
		`SELECT count(*) FROM pg_available_extensions WHERE name IN ('vector','pg_textsearch')`),
		"the copied tree must not list vector or pg_textsearch")
	c.Eq(int64(0), queryInt(t, pool, "count the recall extensions",
		`SELECT count(*) FROM pg_extension WHERE extname IN ('vector','pg_textsearch')`),
		"neither extension may be installed")
	c.Eq(int64(1), queryInt(t, pool, "count ltree",
		`SELECT count(*) FROM pg_extension WHERE extname = 'ltree'`),
		"ltree is unconditional: 0036 creates it")

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
}

// TestEmbeddedPostgresBackupRestoreRoundTrip proves `rafikid db backup` and
// `rafikid db restore` on the server they exist for: a memory row written, the
// data directory archived, the row deleted, the archive put back, and the row
// read again. "The archive was written" is not the claim — that the bytes on
// disk ARE the database is, and only a read-back after the swap shows it.
//
// The server is RUNNING when the backup is taken, which is the operator's
// situation and exactly why the command needs --force: it stops the server for
// the copy and starts it again afterwards. The restore makes the same trade,
// and this is the PHYSICAL path; the dump path is proved against a migrated
// database of a server with timescaledb installed in
// TestDbBackupRestoreRoundTripOnAServerWithTimescaleInstalled.
//
// The format is xz: tgz exists for a consumer that only speaks gzip and is
// never chosen automatically, so txz is what a physical backup is by default.
func TestEmbeddedPostgresBackupRestoreRoundTrip(t *testing.T) {
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

	// Registered FIRST, so it runs LAST — after any cleanup that would have to
	// come before it. rafikid never stops the private server; the test that
	// started it is the only thing that will.
	t.Cleanup(func() {
		if !stopEmbeddedServer(t, server) {
			return // the data dir is what lets a human finish the job
		}
		if err := os.RemoveAll(dirs.root); err != nil {
			t.Logf("remove %s: %v", dirs.root, err)
		}
	})

	env := dirs.env()
	out, err := runRafikid(env, "migrate")
	c.NoError(err, "rafikid migrate: %v\noutput: %s", err, out)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// open returns a pool for ONE interaction. Each `db backup` and `db
	// restore` stops the server, and a connection killed by that stop is not
	// something pgxpool replaces behind your back: the next query on a stale
	// pool fails rather than reconnecting.
	open := func() *pgxpool.Pool {
		t.Helper()
		p, err := pgxpool.New(ctx, server.DSN())
		c.NoError(err, "open the embedded server")
		t.Cleanup(p.Close)
		return p
	}

	// ── the database rafikid serves from is `rafiki` ─────────────────────
	//
	// initdb creates only the maintenance database, so an install that migrated
	// THAT one instead of creating rafiki's would answer here — and every
	// later claim in this test would be about the wrong database.
	pool := open()
	var dbname string
	c.NoError(pool.QueryRow(ctx, "SELECT current_database()").Scan(&dbname), "current_database")
	c.Eq(embedpg.Database, dbname,
		"the embedded server must serve %q, not the maintenance database", embedpg.Database)
	c.Eq(int64(2), queryInt(t, pool, "count rafiki's database beside initdb's",
		`SELECT count(*) FROM pg_database WHERE datname IN ('rafiki','postgres')`),
		"the cluster must hold rafiki's database beside the postgres maintenance database")

	const (
		username = "epg-backup"
		memName  = "roundtrip"
		memBody  = "the row the physical backup has to bring back"
	)
	var ownerID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.users (username, token_sha256)
		VALUES ($1, $2) RETURNING id`, username, "embedded-backup-roundtrip").Scan(&ownerID),
		"insert the memory's owner")
	_, err = pool.Exec(ctx, `INSERT INTO conversations.memory (owner_user_id, path, name, body)
		VALUES ($1, 'embedded_pg', $2, $3)`, ownerID, memName, memBody)
	c.NoError(err, "write the memory")

	// ── back up a running server ─────────────────────────────────────────
	//
	// A bare path: the suffix is the command's to add, and it must be the
	// format's, not the one the operator typed.
	backupPath := filepath.Join(dirs.root, "backup")
	out, err = runRafikid(env, "db", "backup", "--format", "txz", "--force", backupPath)
	c.NoError(err, "rafikid db backup: %v\noutput: %s", err, out)
	archive := backupPath + ".txz"
	c.FileExists(archive, "db backup must write the file the format names")

	// ── delete the row, restore, read it back ────────────────────────────
	pool = open()
	_, err = pool.Exec(ctx, `DELETE FROM conversations.memory
		WHERE owner_user_id = $1 AND name = $2`, ownerID, memName)
	c.NoError(err, "delete the memory")
	c.Eq(int64(0), queryInt(t, pool, "count the memory before the restore",
		`SELECT count(*) FROM conversations.memory WHERE owner_user_id = $1`, ownerID),
		"the row must be gone before the restore, or the read-back proves nothing")

	out, err = runRafikid(env, "db", "restore", "--force", archive)
	c.NoError(err, "rafikid db restore: %v\noutput: %s", err, out)

	pool = open()
	var body string
	c.NoError(pool.QueryRow(ctx, `SELECT body FROM conversations.memory
		WHERE owner_user_id = $1 AND name = $2`, ownerID, memName).Scan(&body),
		"read the memory back after the restore")
	c.Eq(memBody, body, "the row a physical restore brings back must be the one the backup wrote")

	// The directory it replaced is kept, never deleted: it is the operator's
	// only copy of the pre-restore database, and a restore that tidied it away
	// would have destroyed the one thing it exists to preserve.
	replaced, gerr := filepath.Glob(server.DataDir + embedpg.ReplacedDataDirPrefix + "*")
	c.NoError(gerr, "glob the replaced data directories")
	c.Eq(1, len(replaced), "a restore must leave exactly one replaced data directory, got %v", replaced)
	c.FileExists(filepath.Join(server.DataDir, "PG_VERSION"), "the restored tree must be a data directory")
	if _, lerr := os.Lstat(server.RestoreInProgressPath()); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("the restore-in-progress marker %s must be gone after a successful restore: %v",
			server.RestoreInProgressPath(), lerr)
	}
}

// TestDbBackupRestoreRoundTripOnAServerWithTimescaleInstalled is the same round
// trip on a migrated database of a server that has timescaledb installed — the
// server most installs actually run. The database has no hypertable: 0068 leaves
// the three formerly-hypertable tables ordinary, so the restore needs no
// TimescaleDB hook to succeed, and this test is what says so against a real
// server rather than a stub. `pg_restore --clean` still drops and recreates every
// object the archive holds, the extension's catalog included, which is the part
// a plain-server round trip cannot reach.
//
// It uses its OWN scratch database, created and dropped here. The shared
// RAFIKI_TEST_DSN database is not reset between runs (CLAUDE.md), and a restore
// is precisely the operation that must not touch rows another run left behind.
func TestDbBackupRestoreRoundTripOnAServerWithTimescaleInstalled(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	c := assert.NewAborting(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect the test server")
	t.Cleanup(admin.Close)

	name, scratchDSN := scratchDatabase(t, admin, dsn, "rafiki_bk", "")

	scratch, err := pgxpool.New(ctx, scratchDSN)
	c.NoError(err, "connect the scratch database")
	t.Cleanup(scratch.Close)

	c.NoError(store.Migrate(ctx, scratch), "migrate the scratch database")

	// The database every later step writes to must be the scratch one. Asserted
	// because the mistake it catches is silent and destructive: a DSN that
	// still names rafiki_test would put this test's rows, and its restore, into
	// the suite's shared database.
	var dbname string
	c.NoError(scratch.QueryRow(ctx, "SELECT current_database()").Scan(&dbname), "current_database")
	c.Eq(name, dbname, "the round trip must run against the scratch database, not RAFIKI_TEST_DSN's own")

	// The extension is what this test exists for: a server without it would run
	// the restore against a database nothing distinguishes from a plain one, and
	// pass while proving nothing about the server it names.
	c.Require().Eq(int64(1), queryInt(t, scratch, "is timescaledb installed",
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'`),
		"RAFIKI_TEST_DSN's server must have the extension installed (and in template1), or this test "+
			"proves nothing about the server it names")
	// Before the backup, so a restore that silently rebuilt the table as something
	// else is a difference rather than a coincidence.
	c.Eq(int64(0), queryInt(t, scratch, "the formerly-hypertable table before the backup",
		`SELECT count(*) FROM timescaledb_information.hypertables
		  WHERE hypertable_name = 'conversation_turn'`),
		"conversation_turn must be an ordinary table before the backup")

	const (
		username = "bk-roundtrip"
		memName  = "roundtrip"
		memBody  = "the row pg_restore --clean has to bring back"
	)
	var ownerID string
	c.NoError(scratch.QueryRow(ctx, `INSERT INTO conversations.users (username, token_sha256)
		VALUES ($1, $2) RETURNING id`, username, "timescale-backup-roundtrip").Scan(&ownerID),
		"insert the memory's owner")
	_, err = scratch.Exec(ctx, `INSERT INTO conversations.memory (owner_user_id, path, name, body)
		VALUES ($1, 'timescale_pg', $2, $3)`, ownerID, memName, memBody)
	c.NoError(err, "write the memory")

	// RAFIKI_DB carries the scratch DSN as well as --db: a flag this test
	// dropped would otherwise fall through to RAFIKI_TEST_DSN, and rafiki_test
	// must never be a restore target. The DSN itself is the disposable
	// container's, not a secret.
	env := append(os.Environ(), "RAFIKI_DB="+scratchDSN)

	backupPath := filepath.Join(t.TempDir(), "scratch.dump")
	out, err := runRafikid(env, "db", "backup", "--db", scratchDSN, "--format", "dump", backupPath)
	c.NoError(err, "rafikid db backup: %v\noutput: %s", err, out)
	c.FileExists(backupPath, "db backup must write the file it was given")

	_, err = scratch.Exec(ctx, `DELETE FROM conversations.memory
		WHERE owner_user_id = $1 AND name = $2`, ownerID, memName)
	c.NoError(err, "delete the memory")
	c.Eq(int64(0), queryInt(t, scratch, "count the memory before the restore",
		`SELECT count(*) FROM conversations.memory WHERE owner_user_id = $1`, ownerID),
		"the row must be gone before the restore, or the read-back proves nothing")

	out, err = runRafikid(env, "db", "restore", "--force", "--db", scratchDSN, backupPath)
	c.NoError(err, "rafikid db restore: %v\noutput: %s", err, out)

	// A fresh pool: pg_restore --clean dropped and recreated every object the
	// archive holds, so the plans the old pool cached describe tables that no
	// longer exist.
	readback, err := pgxpool.New(ctx, scratchDSN)
	c.NoError(err, "reconnect after the restore")
	t.Cleanup(readback.Close)

	var body string
	c.NoError(readback.QueryRow(ctx, `SELECT body FROM conversations.memory
		WHERE owner_user_id = $1 AND name = $2`, ownerID, memName).Scan(&body),
		"read the memory back after the restore")
	c.Eq(memBody, body, "the row a dump restore brings back must be the one the backup wrote")

	// The extension's catalog is what `pg_restore --clean` drops and recreates along
	// with everything else the archive holds, and what a plain-server round trip
	// cannot reach. Checked AFTER the restore: the premise check above only says
	// the scratch database started with it.
	c.Eq(int64(1), queryInt(t, readback, "the extension after the restore",
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'`),
		"the archive's extension catalog must come back whole, or the database is broken")

	c.Eq(int64(0), queryInt(t, readback, "the formerly-hypertable table after the restore",
		`SELECT count(*) FROM timescaledb_information.hypertables
		  WHERE hypertable_name = 'conversation_turn'`),
		"conversation_turn must be an ordinary table after a dump restore")
}

// dsnForDatabase points a test DSN at another database on the same server,
// whichever dialect it is written in — a URL or libpq keyword/value. It is
// string surgery on purpose: pgx's own Config.ConnString() returns the ORIGINAL
// connection string, so mutating ConnConfig.Database and reading it back yields
// a DSN that still names the old database.
func dsnForDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String()
	}
	return dsn + " dbname=" + name
}

// TestEmbeddedPostgresMovesOntoANewBuild pins what an upgrade needs on a real
// server: a server started from one tree is still running from it after a new
// tree lands, unless Ensure is allowed to replace it. A second tree is a copy of
// the first at another path, which is all a new build is to Ensure.
func TestEmbeddedPostgresMovesOntoANewBuild(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs the embedded-postgres end-to-end test (downloads the pinned artifact once)")
	}
	c := assert.NewAborting(t)

	dirs := newEmbeddedDirs(t)
	t.Setenv("XDG_DATA_HOME", dirs.data)
	t.Setenv("XDG_STATE_HOME", dirs.state)
	t.Setenv("XDG_CACHE_HOME", dirs.cache)
	shared := embedpg.Default().BinDir
	if _, err := os.Stat(filepath.Join(shared, "bin", "postgres")); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_, err := embedpg.Default().Ensure(ctx)
		c.NoError(err, "fetch the embedded binaries")
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(dirs.root, "data2"))
	old := embedpg.Default() // BinDir = the shared tree
	treeB := filepath.Join(dirs.root, "tree-b")
	copyEmbeddedTree(t, old.BinDir, treeB, func(string) bool { return false })

	t.Cleanup(func() {
		if !stopEmbeddedServer(t, old) {
			return // the data dir is what lets a human finish the job
		}
		if err := os.RemoveAll(dirs.root); err != nil {
			t.Logf("remove %s: %v", dirs.root, err)
		}
	})
	runningFrom := func() string {
		b, err := os.ReadFile(filepath.Join(old.DataDir, "postmaster.opts"))
		c.NoError(err, "read postmaster.opts")
		first, _, _ := strings.Cut(string(b), " \"")
		resolved, err := filepath.EvalSymlinks(first)
		c.NoError(err, "resolve the running binary")
		return resolved
	}
	resolve := func(dir string) string {
		r, err := filepath.EvalSymlinks(filepath.Join(dir, "bin", "postgres"))
		c.NoError(err, "resolve %s", dir)
		return r
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, err := old.Ensure(ctx)
	c.NoError(err, "start from the first tree")
	c.Eq(resolve(old.BinDir), runningFrom(), "started from the first tree")

	next := old
	next.BinDir = treeB

	// Not allowed to restart: left running on the old binaries, and told.
	var progress strings.Builder
	next.Progress = &progress
	_, err = next.Ensure(ctx)
	c.NoError(err, "Ensure without permission to restart")
	c.Eq(resolve(old.BinDir), runningFrom(), "without permission the server keeps its binaries")
	c.StrContains(progress.String(), "still running from", "the operator must be told")

	// Allowed: replaced, and serving.
	progress.Reset()
	next.RestartStale = func() bool { return true }
	_, err = next.Ensure(ctx)
	c.NoError(err, "Ensure with permission to restart")
	c.Eq(resolve(treeB), runningFrom(), "the server must now run from the new tree")
	c.StrContains(progress.String(), "restarting embedded PostgreSQL")
	running, err := next.Status(ctx)
	c.NoError(err, "Status")
	c.True(running, "the replaced server must be up")
}

// TestEmbeddedPostgresInstallsAndUpdatesExtensions pins the start-up reconcile on a
// real server: the first resolve of a fresh install CREATEs vector and
// pg_textsearch (nothing ran migrate, so only the reconcile could have), and a
// resolve after the recorded version has fallen behind the tree's runs ALTER
// EXTENSION UPDATE. Only the current base script of each extension is installed,
// so an old version cannot be truly installed: the drift is faked by rewriting
// the recorded version, which makes the test about the reconcile's decision and
// the statement it issues, not about an old-to-new schema transition (that is the
// extension's own update script's business).
//
// Everything goes through `rafikid db psql` with an empty PATH, so the clients
// are the tree's own, and each call's resolve is what runs the reconcile BEFORE
// its query.
func TestEmbeddedPostgresInstallsAndUpdatesExtensions(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs the embedded-postgres end-to-end test (downloads the pinned artifact once)")
	}
	c := assert.NewAborting(t)

	dirs := newEmbeddedDirs(t)
	t.Setenv("XDG_DATA_HOME", dirs.data)
	t.Setenv("XDG_STATE_HOME", dirs.state)
	t.Setenv("XDG_CACHE_HOME", dirs.cache)

	server := embedpg.Default()
	t.Cleanup(func() {
		if !stopEmbeddedServer(t, server) {
			return // the data dir is what lets a human finish the job
		}
		if err := os.RemoveAll(dirs.root); err != nil {
			t.Logf("remove %s: %v", dirs.root, err)
		}
	})

	env := append(dirs.env(), "PATH="+t.TempDir())
	// psql runs the SQL args through `rafikid db psql` and returns the last line of
	// its output (the target line is printed first, on stderr).
	psql := func(args ...string) string {
		t.Helper()
		out, err := runRafikid(env, append([]string{"db", "psql", "--", "-X", "-At", "-v", "ON_ERROR_STOP=1"}, args...)...)
		c.NoError(err, "rafikid db psql %v: %v\noutput: %s", args, err, out)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}
	const (
		installedSQL = `SELECT coalesce(string_agg(extname || '=' || extversion, ',' ORDER BY extname), '')
		                  FROM pg_extension WHERE extname IN ('vector', 'pg_textsearch')`
		shippedSQL = `SELECT string_agg(name || '=' || default_version, ',' ORDER BY name)
		                FROM pg_available_extensions WHERE name IN ('vector', 'pg_textsearch')`
	)

	// A fresh install: the database does not exist before this call, and no
	// migration has run, so only the reconcile can have installed anything.
	shipped := psql("-c", shippedSQL)
	c.StrContains(shipped, "vector=", "the tree must ship vector: %s", shipped)
	c.StrContains(shipped, "pg_textsearch=", "the tree must ship pg_textsearch: %s", shipped)
	c.Eq(shipped, psql("-c", installedSQL), "a fresh install must have both extensions at the shipped versions")

	// The recorded versions fall behind the tree's, as they do after a new tree
	// replaces an old one. The same call reads them back to prove the drift is
	// really there before the next resolve fixes it.
	drifted := psql("-c", `UPDATE pg_extension SET extversion = '0.8.6' WHERE extname = 'vector'`,
		"-c", `UPDATE pg_extension SET extversion = '1.5.0' WHERE extname = 'pg_textsearch'`,
		"-c", installedSQL)
	c.Eq("pg_textsearch=1.5.0,vector=0.8.6", drifted, "the drift must be in place before the next resolve")

	c.Eq(shipped, psql("-c", installedSQL),
		"the next resolve must update both extensions to the versions the tree ships")
}

// TestEmbeddedPostgresDumpRoundTripUsesTheBundledClients is the default backup of
// an embedded install, on a real server with NOTHING on PATH: `db backup` with no
// --format takes a logical dump with the pg_dump in the server's own tree, and
// `db restore` puts it back with that tree's pg_restore. The PATH is an empty
// directory so a client the host happened to have installed cannot be what ran.
func TestEmbeddedPostgresDumpRoundTripUsesTheBundledClients(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs the embedded-postgres end-to-end test (downloads the pinned artifact once)")
	}
	c := assert.NewAborting(t)

	dirs := newEmbeddedDirs(t)
	t.Setenv("XDG_DATA_HOME", dirs.data)
	t.Setenv("XDG_STATE_HOME", dirs.state)
	t.Setenv("XDG_CACHE_HOME", dirs.cache)

	server := embedpg.Default()
	t.Cleanup(func() {
		if !stopEmbeddedServer(t, server) {
			return // the data dir is what lets a human finish the job
		}
		if err := os.RemoveAll(dirs.root); err != nil {
			t.Logf("remove %s: %v", dirs.root, err)
		}
	})

	// Last entry wins (exec.Cmd.Env), so this replaces any PATH os.Environ held.
	env := append(dirs.env(), "PATH="+t.TempDir())
	out, err := runRafikid(env, "migrate")
	c.NoError(err, "rafikid migrate: %v\noutput: %s", err, out)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, server.DSN())
	c.NoError(err, "open the embedded server")
	defer pool.Close()

	const (
		username = "epg-dump"
		memName  = "roundtrip"
		memBody  = "the row the logical dump has to bring back"
	)
	var ownerID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.users (username, token_sha256)
		VALUES ($1, $2) RETURNING id`, username, "embedded-dump-roundtrip").Scan(&ownerID),
		"insert the memory's owner")
	_, err = pool.Exec(ctx, `INSERT INTO conversations.memory (owner_user_id, path, name, body)
		VALUES ($1, 'embedded_pg', $2, $3)`, ownerID, memName, memBody)
	c.NoError(err, "write the memory")

	// No --format and no --force: a dump needs neither. The default must be the
	// dump, and its suffix the dump's.
	backupPath := filepath.Join(dirs.root, "backup")
	out, err = runRafikid(env, "db", "backup", backupPath)
	c.NoError(err, "rafikid db backup: %v\noutput: %s", err, out)
	archive := backupPath + ".dump"
	c.FileExists(archive, "the default embedded backup must be a logical dump")
	head, err := os.ReadFile(archive)
	c.NoError(err, "read the archive")
	c.True(len(head) > 5 && string(head[:5]) == "PGDMP", "the archive must be a pg_dump custom archive")

	_, err = pool.Exec(ctx, `DELETE FROM conversations.memory WHERE owner_user_id = $1 AND name = $2`, ownerID, memName)
	c.NoError(err, "delete the memory")
	c.Eq(int64(0), queryInt(t, pool, "count the memory before the restore",
		`SELECT count(*) FROM conversations.memory WHERE owner_user_id = $1`, ownerID),
		"the row must be gone before the restore, or the read-back proves nothing")

	// A restore drops and recreates everything the archive holds, so it is gated
	// on --force; the backup above was not.
	out, err = runRafikid(env, "db", "restore", "--force", archive)
	c.NoError(err, "rafikid db restore: %v\noutput: %s", err, out)

	// The restore replaced the schema under the open pool's feet; a fresh pool.
	pool.Close()
	pool, err = pgxpool.New(ctx, server.DSN())
	c.NoError(err, "reopen the embedded server")
	defer pool.Close()
	var body string
	c.NoError(pool.QueryRow(ctx, `SELECT body FROM conversations.memory
		WHERE owner_user_id = $1 AND name = $2`, ownerID, memName).Scan(&body),
		"read the memory back after the restore")
	c.Eq(memBody, body, "the row a dump restore brings back must be the one the dump took")
}

// TestEmbeddedPostgresDaemonReconnectsAfterAPhysicalBackup pins what an operator
// sees when they accept a physical backup's downtime on a live install: the
// server goes away under the daemon, and the daemon reconnects on its own. It
// does not sit without a database until rafikid itself is restarted, which is
// what makes `db backup --force` (and a physical restore) usable there rather
// than a stop-the-world operation nobody would run.
//
// The row is written and read THROUGH the daemon, so the claim is about the
// daemon's pool. A client dialling afresh would prove nothing about it, and a
// one-shot assertion would be a race: the pool discards a dead connection when
// it next needs one, so the read is retried until the daemon answers.
func TestEmbeddedPostgresDaemonReconnectsAfterAPhysicalBackup(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs the embedded-postgres end-to-end test (downloads ~15 MB once)")
	}
	c := assert.NewAborting(t)

	dirs := newEmbeddedDirs(t)
	t.Setenv("XDG_DATA_HOME", dirs.data)
	t.Setenv("XDG_STATE_HOME", dirs.state)
	t.Setenv("XDG_CACHE_HOME", dirs.cache)
	server := embedpg.Default()
	t.Cleanup(func() {
		if !stopEmbeddedServer(t, server) {
			return
		}
		if err := os.RemoveAll(dirs.root); err != nil {
			t.Logf("remove %s: %v", dirs.root, err)
		}
	})

	env := dirs.env()
	out, err := runRafikid(env, "migrate")
	c.NoError(err, "rafikid migrate: %v\noutput: %s", err, out)
	out, err = runRafikid(env, "user", "create", "epg-live", "--token")
	c.NoError(err, "rafikid user create: %v\noutput: %s", err, out)
	token, ok := userTokenFromCreate(out)
	c.Require().True(ok, "no token in `rafikid user create --token` output: %s", out)

	d := bootEmbeddedDaemon(t, dirs)
	client := d.control(t)

	const (
		memPath = "embedded_pg_live"
		memName = "backup"
		memBody = "written before the backup stopped the server"
	)
	call := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := client.PutMemory(ctx, authorize(token, connect.NewRequest(&rafikiv1.PutMemoryRequest{
			Path: memPath, Name: memName, Body: memBody,
		})))
		return err
	}
	c.NoError(call(), "PutMemory before the backup")

	backupPath := filepath.Join(dirs.root, "live")
	out, err = runRafikid(env, "db", "backup", "--format", "txz", "--force", backupPath)
	c.NoError(err, "rafikid db backup against a live daemon: %v\noutput: %s", err, out)

	c.Eventually(60*time.Second, "the daemon serves again after the shared server restarted", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		got, err := client.GetMemory(ctx, authorize(token, connect.NewRequest(&rafikiv1.GetMemoryRequest{
			Path: memPath, Name: memName,
		})))
		if err != nil {
			return err
		}
		if body := got.Msg.GetMemory().GetBody(); body != memBody {
			return fmt.Errorf("the memory body came back as %q, want %q", body, memBody)
		}
		return nil
	})
}

// scratchDatabase creates a database on the RAFIKI_TEST_DSN server and returns
// its name and a DSN pointing at it. template is the template database to copy,
// or "" for the server's default; template0 is how a test asks for a database
// carrying none of template1's extensions.
//
// The DSN is rewritten, never built from pgxpool.Config.ConnString(): that
// returns the ORIGINAL string (pgconn.ConnConfig keeps it), so assigning
// ConnConfig.Database leaves the DSN naming RAFIKI_TEST_DSN's own database —
// which is how a restore ends up aimed at rafiki_test.
func scratchDatabase(t *testing.T, admin *pgxpool.Pool, dsn, prefix, template string) (name, scratchDSN string) {
	t.Helper()
	c := assert.NewAborting(t)

	name = fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	create := "CREATE DATABASE " + name
	if template != "" {
		create += " TEMPLATE " + template
	}
	_, err := admin.Exec(context.Background(), create)
	c.NoError(err, "create scratch database %s", name)
	// Registered after the caller's admin.Close, so it runs BEFORE it (cleanups
	// are LIFO) and the pool it uses is still open.
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database %s: %v", name, err)
		}
	})
	return name, dsnForDatabase(t, dsn, name)
}

// TestDbRestoreIntoAFreshDatabaseWithoutTheExtension pins the restore path whose
// target lacks the extension altogether: timescaledb is available on the server
// but not installed in the database. The archive's own CREATE EXTENSION creates
// it, so --clean finds nothing to drop, and the archive carries no hypertable to
// trip over either (0068 leaves conversation_turn an ordinary table) — which is
// what makes the documented rehearsal ("restore the dump into a new database")
// work.
//
// It still runs as a superuser, deliberately: the same archive creates the
// UNTRUSTED vector and pg_textsearch extensions, and a non-superuser cannot
// CREATE EXTENSION them, so a fresh-database restore is a superuser step.
//
// The target is created from template0 ON PURPOSE: rafiki's test container puts
// timescaledb in template1, so an ordinary new database would have it installed
// and this would be the in-place case again.
func TestDbRestoreIntoAFreshDatabaseWithoutTheExtension(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	c := assert.NewAborting(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect the test server")
	t.Cleanup(admin.Close)

	// The source is an ordinary database: migrated, with a row to find again.
	_, sourceDSN := scratchDatabase(t, admin, dsn, "rafiki_bk_src", "")
	source, err := pgxpool.New(ctx, sourceDSN)
	c.NoError(err, "connect the source database")
	t.Cleanup(source.Close)
	c.NoError(store.Migrate(ctx, source), "migrate the source database")

	const (
		memBody = "the row a restore into a fresh database has to bring back"
		memName = "roundtrip"
	)
	var ownerID string
	c.NoError(source.QueryRow(ctx, `INSERT INTO conversations.users (username, token_sha256)
		VALUES ('bk-fresh', 'fresh') RETURNING id`).Scan(&ownerID), "insert the memory's owner")
	_, err = source.Exec(ctx, `INSERT INTO conversations.memory (owner_user_id, path, name, body)
		VALUES ($1, 'timescale_pg', $2, $3)`, ownerID, memName, memBody)
	c.NoError(err, "write the memory")

	env := append(os.Environ(), "RAFIKI_DB="+sourceDSN)
	backupPath := filepath.Join(t.TempDir(), "fresh.dump")
	out, err := runRafikid(env, "db", "backup", "--db", sourceDSN, "--format", "dump", backupPath)
	c.NoError(err, "rafikid db backup: %v\noutput: %s", err, out)

	// The target: empty, and without the extension.
	_, targetDSN := scratchDatabase(t, admin, dsn, "rafiki_bk_dst", "template0")
	target, err := pgxpool.New(ctx, targetDSN)
	c.NoError(err, "connect the fresh database")
	t.Cleanup(target.Close)
	c.Require().Eq(int64(0), queryInt(t, target, "is timescaledb absent",
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'`),
		"the fresh database must NOT have the extension installed, or this is the in-place case")
	c.Require().Eq(int64(1), queryInt(t, target, "is timescaledb available",
		`SELECT count(*) FROM pg_available_extensions WHERE name = 'timescaledb'`),
		"the fresh database must be on a server that can create the extension")

	out, err = runRafikid(append(os.Environ(), "RAFIKI_DB="+targetDSN),
		"db", "restore", "--force", "--db", targetDSN, backupPath)
	c.NoError(err, "rafikid db restore into a fresh database: %v\noutput: %s", err, out)

	// The archive carried the extension, so the database has it now, and the
	// restored schema is whole — every object the archive holds came back.
	var body string
	c.NoError(target.QueryRow(ctx, `SELECT body FROM conversations.memory
		WHERE owner_user_id = $1 AND name = $2`, ownerID, memName).Scan(&body),
		"read the memory back from the fresh database")
	c.Eq(memBody, body, "the row the backup wrote must be the row the fresh database holds")
	c.Eq(int64(1), queryInt(t, target, "the extension the archive created",
		`SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'`),
		"the archive's CREATE EXTENSION must have installed timescaledb")
	c.Eq(int64(0), queryInt(t, target, "the formerly-hypertable table in the fresh database",
		`SELECT count(*) FROM timescaledb_information.hypertables
		  WHERE hypertable_name = 'conversation_turn'`),
		"conversation_turn must be an ordinary table in the restored database")
}

// dsnAsUser points a test DSN at another role on the same server. It is the
// same string surgery as dsnForDatabase, for the same reason: pgx's
// Config.ConnString() returns the ORIGINAL connection string.
func dsnAsUser(t *testing.T, dsn, user string) string {
	t.Helper()
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.User = url.User(user)
		return u.String()
	}
	return dsn + " user=" + user
}

// TestDbRestoreKeepsObjectOwnershipForTheDaemonRole is what an in-place restore
// is FOR, and the one thing every other test here cannot see: they all run as a
// superuser end to end.
//
// pg_restore --no-owner makes the CONNECTING user the owner of everything it
// creates. That is right for the daemon's own unprivileged role, which can only
// own what it creates, and catastrophic for the superuser restore a fresh
// database needs: the whole schema would come back owned by the superuser, and
// the daemon would get permission denied on its first query. So
// this restores AS superuser and then READS as a separate unprivileged role —
// the role a real install's daemon runs as.
func TestDbRestoreKeepsObjectOwnershipForTheDaemonRole(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	c := assert.NewAborting(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect the test server")
	t.Cleanup(admin.Close)

	// The daemon's role. Registered FIRST so it is dropped LAST: a role cannot
	// go while it owns anything, and the databases that hold its objects are
	// dropped by their own (later-registered) cleanups.
	role := fmt.Sprintf("rafiki_own_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER")
	c.NoError(err, "create the unprivileged role %s", role)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+role); err != nil {
			t.Logf("drop role %s: %v", role, err)
		}
	})

	// The source: migrated, one row, and owned the way an install owns it — by
	// the unprivileged role, not by postgres.
	_, sourceDSN := scratchDatabase(t, admin, dsn, "rafiki_own_src", "")
	source, err := pgxpool.New(ctx, sourceDSN)
	c.NoError(err, "connect the source database")
	t.Cleanup(source.Close)
	c.NoError(store.Migrate(ctx, source), "migrate the source database")

	const memBody = "the row the restore has to keep readable for the daemon"
	var ownerID string
	c.NoError(source.QueryRow(ctx, `INSERT INTO conversations.users (username, token_sha256)
		VALUES ('own', 'own') RETURNING id`).Scan(&ownerID), "insert the memory's owner")
	_, err = source.Exec(ctx, `INSERT INTO conversations.memory (owner_user_id, path, name, body)
		VALUES ($1, 'ownership', 'owned', $2)`, ownerID, memBody)
	c.NoError(err, "write the memory")
	for _, stmt := range []string{
		"ALTER SCHEMA conversations OWNER TO " + role,
		"ALTER TABLE conversations.users OWNER TO " + role,
		"ALTER TABLE conversations.memory OWNER TO " + role,
	} {
		_, err = source.Exec(ctx, stmt)
		c.NoError(err, "%s", stmt)
	}

	env := append(os.Environ(), "RAFIKI_DB="+sourceDSN)
	backupPath := filepath.Join(t.TempDir(), "ownership.dump")
	out, err := runRafikid(env, "db", "backup", "--db", sourceDSN, "--format", "dump", backupPath)
	c.NoError(err, "rafikid db backup: %v\noutput: %s", err, out)

	_, targetDSN := scratchDatabase(t, admin, dsn, "rafiki_own_dst", "template0")
	out, err = runRafikid(append(os.Environ(), "RAFIKI_DB="+targetDSN),
		"db", "restore", "--force", "--db", targetDSN, backupPath)
	c.NoError(err, "rafikid db restore as superuser: %v\noutput: %s", err, out)

	// The owner is the archive's, not the connecting superuser's.
	target, err := pgxpool.New(ctx, targetDSN)
	c.NoError(err, "connect the restored database")
	t.Cleanup(target.Close)
	owners := queryStrings(t, target, "restored object owners", `
		SELECT c.relname, pg_get_userbyid(c.relowner)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'conversations' AND c.relname IN ('users','memory')`)
	c.Eq(role, owners["memory"], "conversations.memory must belong to %q, not to the restoring superuser", role)
	c.Eq(role, owners["users"], "conversations.users must belong to %q, not to the restoring superuser", role)

	// And the point of all of it: that role can read what was restored.
	app, err := pgxpool.New(ctx, dsnAsUser(t, targetDSN, role))
	c.NoError(err, "connect as %s", role)
	t.Cleanup(app.Close)
	var body string
	c.NoError(app.QueryRow(ctx, `SELECT body FROM conversations.memory WHERE name = 'owned'`).Scan(&body),
		"the daemon's own role must be able to read the restored table")
	c.Eq(memBody, body, "the restored row must be the one the backup wrote")
}
