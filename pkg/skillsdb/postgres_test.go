// SPDX-License-Identifier: Apache-2.0

package skillsdb

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/usersdb/postgres_test.go's testStore rather than the DSN-and-
// DELETE-FROM pattern, so this never touches a developer's real database.
func testStore(t *testing.T) (skills.Store, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_skills_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect scratch db")
	t.Cleanup(pool.Close)

	c.NoError(store.Migrate(ctx, pool), "migrate")
	return NewPostgresStore(pool), pool
}

// fixture is one test's own namespace and source. Both are unique per run: the
// shared test database is never reset between runs, so a fixed name would read
// an earlier run's rows, and a global row count would count them.
type fixture struct {
	st   skills.Store
	pool *pgxpool.Pool
	ns   string
	key  skills.SetKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, pool := testStore(t)
	n := time.Now().UnixNano()
	f := &fixture{
		st:   st,
		pool: pool,
		ns:   fmt.Sprintf("ns-%d", n),
		key:  skills.SetKey{Source: fmt.Sprintf("test-%d", n)},
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE source = $1`, f.key.Source)
	})
	return f
}

// rec builds a want row in this fixture's namespace and source.
func (f *fixture) rec(name, body string, files ...skills.File) skills.Record {
	return skills.Record{
		Namespace: f.ns, Name: name, Description: name + " description",
		Body: body, Source: f.key.Source, Enabled: true, Files: files,
	}
}

func (f *fixture) put(t *testing.T, want ...skills.Record) bool {
	t.Helper()
	changed, err := f.st.PutSet(context.Background(), f.key, want)
	if err != nil {
		t.Fatalf("PutSet: %v", err)
	}
	return changed
}

// list is this fixture's namespace only, keyed by name — never a global count.
func (f *fixture) list(t *testing.T) map[string]skills.Record {
	t.Helper()
	rows, err := f.st.List(context.Background(), false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := make(map[string]skills.Record)
	for _, r := range rows {
		if r.Namespace == f.ns {
			out[r.Name] = r
		}
	}
	return out
}

func file(path, content string) skills.File {
	return skills.File{Path: path, Mode: 0o644, Content: []byte(content)}
}

func blobExists(t *testing.T, pool *pgxpool.Pool, content []byte) bool {
	t.Helper()
	h := skills.BlobHash(content)
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM conversations.skill_blob WHERE sha256 = $1)`, h[:]).Scan(&ok); err != nil {
		t.Fatalf("probe blob: %v", err)
	}
	return ok
}

func TestPutSetInsertsAndPrunes(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	c.True(f.put(t, f.rec("alpha", "a1"), f.rec("beta", "b1", file("ref.md", "hello"))), "first write reported no change")

	byName := f.list(t)
	c.Require().Len(byName, 2, "got %d rows, want the two written", len(byName))
	c.Eq("a1", byName["alpha"].Body, "alpha body")
	c.False(byName["alpha"].HasFiles, "alpha has no files, but HasFiles is set")
	c.True(byName["beta"].HasFiles, "beta has a file, but HasFiles is clear")

	got, err := f.st.Get(ctx, byName["beta"].ID)
	c.Require().NoError(err, "Get")
	c.Require().Len(got.Files, 1, "got %d files, want 1", len(got.Files))
	c.Eq("ref.md", got.Files[0].Path, "file path")
	c.Eq("hello", string(got.Files[0].Content), "file content")

	// A row absent from the next write belongs to this set, so it is pruned.
	c.True(f.put(t, f.rec("beta", "b1", file("ref.md", "hello")), f.rec("gamma", "g1")), "pruning write reported no change")
	byName = f.list(t)
	c.Require().Len(byName, 2, "got %d rows, want beta and gamma", len(byName))
	c.NotHasKey(byName, "alpha", "a row dropped from the set survived")
	c.HasKey(byName, "gamma", "a row added to the set is missing")
}

// A set byte-identical to what is stored writes nothing: no updated_at churn,
// and changed=false so no push follows.
func TestPutSetIdenticalIsNoop(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)

	want := []skills.Record{
		f.rec("alpha", "a1", file("x.md", "x")),
		f.rec("beta", "b1"),
	}
	c.True(f.put(t, want...), "first write reported no change")
	before := f.list(t)

	c.False(f.put(t, want...), "an identical set reported a change")

	after := f.list(t)
	c.Require().Len(after, len(before), "row count moved on a no-op write")
	for name, was := range before {
		now, ok := after[name]
		c.Require().True(ok, "%s disappeared on a no-op write", name)
		c.Eq(was.ID, now.ID, "%s: id changed", name)
		c.Eq(was.UpdatedAt, now.UpdatedAt, "%s: updated_at moved on a no-op write", name)
	}
}

// Re-syncing content is not permission to re-enable a skill an operator turned
// off: enabled is deliberately absent from the upsert's SET list.
func TestPutSetPreservesDisabled(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	f.put(t, f.rec("alpha", "v1"))
	row := f.list(t)["alpha"]
	c.Require().NoError(f.st.SetEnabled(ctx, row.ID, false), "disable")

	f.put(t, f.rec("alpha", "v2"))
	got := f.list(t)["alpha"]
	c.Eq("v2", got.Body, "body was not refreshed")
	c.False(got.Enabled, "the write re-enabled a disabled skill")
}

// A changed set must not rewrite the rows that did not change. The upsert
// would bump updated_at on every row of the set, so editing one skill would make
// the whole corpus look freshly written.
func TestPutSetLeavesUnchangedRowsAlone(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)

	f.put(t,
		f.rec("alpha", "a", file("ref.md", "x")),
		f.rec("beta", "b"),
		f.rec("gamma", "g", file("ref.md", "v1")),
	)
	before := f.list(t)

	f.put(t,
		f.rec("alpha", "a", file("ref.md", "x")),  // identical
		f.rec("beta", "b2"),                       // body changed
		f.rec("gamma", "g", file("ref.md", "v2")), // file changed only
	)
	after := f.list(t)

	c.Eq(before["alpha"].UpdatedAt, after["alpha"].UpdatedAt, "an unchanged row's updated_at moved")
	c.NotEq(before["beta"].UpdatedAt, after["beta"].UpdatedAt, "a row whose body changed kept its updated_at")
	c.NotEq(before["gamma"].UpdatedAt, after["gamma"].UpdatedAt, "a row whose file changed kept its updated_at")
	c.Eq("b2", after["beta"].Body, "changed body")
}

// A namespace belongs to one source per layer. A second source claiming it is
// refused with the holder's name, and writes nothing at all.
func TestPutSetNamespaceHeld(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	f.put(t, f.rec("alpha", "a"))

	other := skills.SetKey{Source: f.key.Source + "-other"}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE source = $1`, other.Source)
	})
	_, err := f.st.PutSet(ctx, other, []skills.Record{{
		Namespace: f.ns, Name: "beta", Body: "b", Source: other.Source,
	}})

	held := c.Require().ErrorAs[*skills.NamespaceHeldError](err, "a second source claiming the same namespace: got")
	c.Eq(f.ns, held.Namespace, "namespace named by the error")
	c.Eq(f.key.Source, held.Holder, "holder named by the error")
	c.ErrorContains(err, f.key.Source, "error text")

	byName := f.list(t)
	c.Len(byName, 1, "the refused write left %d rows, want only the incumbent", len(byName))
	c.NotHasKey(byName, "beta", "the refused write inserted its row")
}

// The core layer is its own layer: a global (or user) set may hold the same
// namespace and name without colliding, and neither write touches the other's
// row.
func TestPutSetCoreIsItsOwnLayer(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	name := fmt.Sprintf("layer-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE namespace = $1 AND name = $2`,
			skills.DefaultNamespace, name)
	})

	core := skills.SetKey{Source: skills.CoreSource}
	if _, err := f.st.PutSet(ctx, core, []skills.Record{{
		Namespace: skills.DefaultNamespace, Name: name, Body: "core body",
	}}); err != nil {
		t.Fatalf("core PutSet: %v", err)
	}
	if _, err := f.st.PutSet(ctx, f.key, []skills.Record{{
		Namespace: skills.DefaultNamespace, Name: name, Body: "global body",
	}}); err != nil {
		t.Fatalf("global PutSet in the core's namespace: %v", err)
	}

	rows := rowsByName(t, f.st, skills.DefaultNamespace, name)
	c.Require().Len(rows, 2, "got %d rows, want the core row and the global row", len(rows))

	// A later core write refreshes only its own layer's row.
	if _, err := f.st.PutSet(ctx, core, []skills.Record{{
		Namespace: skills.DefaultNamespace, Name: name, Body: "core body v2",
	}}); err != nil {
		t.Fatalf("second core PutSet: %v", err)
	}
	bySource := map[string]skills.Record{}
	for _, r := range rowsByName(t, f.st, skills.DefaultNamespace, name) {
		bySource[r.Source] = r
	}
	c.Require().Len(bySource, 2, "a core write collapsed the layers: %v", bySource)
	c.Eq("core body v2", bySource[skills.CoreSource].Body, "core body")
	c.Eq("global body", bySource[f.key.Source].Body, "the core write rewrote the global layer's row")
}

// The sweep removes exactly the blobs this set stopped referencing: a replaced
// file's bytes go, and a hash another skill still points at stays.
func TestPutSetSweepsBlobs(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	oldContent := []byte("old content")
	shared := []byte("shared content")
	f.put(t,
		f.rec("alpha", "a", file("old.md", string(oldContent))),
		f.rec("beta", "b", file("shared.md", string(shared))),
	)

	// A second set, in its own namespace, referencing the same blob.
	other := skills.SetKey{Source: f.key.Source + "-other"}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE source = $1`, other.Source)
	})
	if _, err := f.st.PutSet(ctx, other, []skills.Record{{
		Namespace: f.ns + "-other", Name: "gamma", Body: "g",
		Files: []skills.File{{Path: "shared.md", Mode: 0o644, Content: shared}},
	}}); err != nil {
		t.Fatalf("second set PutSet: %v", err)
	}

	c.True(blobExists(t, f.pool, oldContent), "the seeded blob is missing before the write")
	c.True(blobExists(t, f.pool, shared), "the shared blob is missing before the write")

	newContent := []byte("new content")
	f.put(t,
		f.rec("alpha", "a", file("old.md", string(newContent))),
		f.rec("beta", "b", file("shared.md", string(shared))),
	)

	c.False(blobExists(t, f.pool, oldContent), "the replaced file's blob survived the sweep")
	c.True(blobExists(t, f.pool, newContent), "the new blob was swept")
	c.True(blobExists(t, f.pool, shared), "a blob another skill still references was swept")
}

// Two writers whose sets overlap and share a blob must not race the blob sweep
// against the skill_file foreign key. The advisory lock is what makes this
// safe; without it the loser's DELETE can strand the winner's INSERT.
func TestPutSetConcurrent(t *testing.T) {
	f := newFixture(t)
	shared := []byte("shared blob")

	// Two DIFFERENT sets that share one blob. They use different sources and
	// namespaces, so the two writers never contend on a skills row — the only
	// object they share is the blob, which is precisely what the advisory lock
	// protects. Two writers over the SAME set cannot fail this way: their
	// prunes lock the same rows and serialise the transactions for free.
	other := skills.SetKey{Source: f.key.Source + "-b"}
	otherNS := f.ns + "-b"
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE source = $1`, other.Source)
	})

	const iterations = 20
	for i := 0; i < iterations; i++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)

		// The writers flip the shared blob in OPPOSITE phase: on even rounds
		// only "a" references it, on odd rounds only "b" does. So every round
		// one writer is dropping its last reference to the blob (its sweep
		// targets it) while the other is inserting a fresh one — and neither
		// writer's committed rows stand in the way of the sweep. Without the
		// lock the two statements collide on the same blob row: the sweep
		// deletes a blob the other is about to reference, and the skill_file
		// foreign key fails.
		aFiles := []skills.File{{Path: "f.md", Mode: 0o644, Content: []byte(fmt.Sprintf("a-%d", i))}}
		bFiles := []skills.File{
			{Path: "own.md", Mode: 0o644, Content: []byte(fmt.Sprintf("b-%d", i))},
		}
		if i%2 == 0 {
			aFiles = []skills.File{{Path: "f.md", Mode: 0o644, Content: shared}}
		} else {
			bFiles = append(bFiles, skills.File{Path: "shared.md", Mode: 0o644, Content: shared})
		}

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := f.st.PutSet(context.Background(), f.key, []skills.Record{{
				Namespace: f.ns, Name: "alpha", Body: "a", Files: aFiles,
			}}); err != nil {
				errs <- fmt.Errorf("a/%d: %w", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			// "b" rewrites own.md every round, so its write is never the no-op
			// that would skip the skill_file insert this test needs.
			if _, err := f.st.PutSet(context.Background(), other, []skills.Record{{
				Namespace: otherNS, Name: "beta", Body: "b", Files: bFiles,
			}}); err != nil {
				errs <- fmt.Errorf("b/%d: %w", i, err)
			}
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("concurrent PutSet: %v", err)
		}
	}
}

// Deleting a skill must sweep the blobs its files were the last reference to.
// Its skill_file rows go by cascade, so without capturing the hashes first (and
// sweeping under the write lock) a deleted manual/import skill leaks its blobs
// forever — PutSet only sweeps what its own set stopped referencing.
func TestDeleteSweepsBlobs(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	gone := []byte("only this skill's content")
	shared := []byte("shared with another skill")
	f.put(t, f.rec("alpha", "a",
		file("gone.md", string(gone)),
		file("shared.md", string(shared)),
	))
	alpha := f.list(t)["alpha"]

	// A second set, in its own namespace, still pointing at the shared blob.
	other := skills.SetKey{Source: f.key.Source + "-other"}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM conversations.skills WHERE source = $1`, other.Source)
	})
	if _, err := f.st.PutSet(ctx, other, []skills.Record{{
		Namespace: f.ns + "-other", Name: "gamma", Body: "g",
		Files: []skills.File{{Path: "shared.md", Mode: 0o644, Content: shared}},
	}}); err != nil {
		t.Fatalf("second set PutSet: %v", err)
	}

	c.True(blobExists(t, f.pool, gone), "the seeded blob is missing before the delete")
	c.True(blobExists(t, f.pool, shared), "the shared blob is missing before the delete")

	c.Require().NoError(f.st.Delete(ctx, alpha.ID), "Delete")
	c.False(blobExists(t, f.pool, gone), "the deleted skill's own blob survived the delete")
	c.True(blobExists(t, f.pool, shared), "a blob another skill still references was swept")

	_, err := f.st.Get(ctx, alpha.ID)
	c.ErrorIs(err, skills.ErrNotFound, "Get after delete: got")
	c.ErrorIs(f.st.Delete(ctx, alpha.ID), skills.ErrNotFound, "deleting an already-deleted skill: got")
}

func TestGetReturnsFiles(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	f.put(t, f.rec("alpha", "a",
		skills.File{Path: "ref/notes.md", Mode: 0o644, Content: []byte("notes")},
		skills.File{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")},
	))

	row := f.list(t)["alpha"]
	got, err := f.st.Get(ctx, row.ID)
	c.Require().NoError(err, "Get")
	c.Eq(row.ID, got.ID, "id")
	c.Require().Len(got.Files, 2, "got %d files, want 2", len(got.Files))
	c.Eq("ref/notes.md", got.Files[0].Path, "files must be sorted by path: first is")
	c.Eq(uint32(0o644), got.Files[0].Mode, "mode of ref/notes.md")
	c.Eq("notes", string(got.Files[0].Content), "content of ref/notes.md")
	c.Eq("run.sh", got.Files[1].Path, "second file")
	c.Eq(uint32(0o755), got.Files[1].Mode, "mode of run.sh")

	_, err = f.st.Get(ctx, "00000000-0000-0000-0000-000000000000")
	c.ErrorIs(err, skills.ErrNotFound, "Get of an absent id: got")
}

func TestListFilesBatch(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFixture(t)
	ctx := context.Background()

	f.put(t,
		f.rec("alpha", "a", file("a.md", "a-content")),
		f.rec("beta", "b", file("b.md", "b-content"), file("b2.md", "b2-content")),
	)
	byName := f.list(t)
	ids := []string{byName["alpha"].ID, byName["beta"].ID}

	files, err := f.st.Files(ctx, ids)
	c.Require().NoError(err, "Files")
	c.Require().Len(files, 2, "got %d entries, want both skills", len(files))
	c.Require().Len(files[byName["alpha"].ID], 1, "alpha file count")
	c.Eq("a.md", files[byName["alpha"].ID][0].Path, "alpha file")
	c.Require().Len(files[byName["beta"].ID], 2, "beta file count")
	c.Eq("b.md", files[byName["beta"].ID][0].Path, "beta files must be sorted by path: first is")
	c.Eq("b2.md", files[byName["beta"].ID][1].Path, "beta second file")

	// A skill with no files is absent from the map, so a caller's lookup is nil.
	f.put(t,
		f.rec("alpha", "a", file("a.md", "a-content")),
		f.rec("beta", "b", file("b.md", "b-content"), file("b2.md", "b2-content")),
		f.rec("gamma", "g"),
	)
	gamma := f.list(t)["gamma"]
	files, err = f.st.Files(ctx, []string{gamma.ID})
	c.Require().NoError(err, "Files for a file-less skill")
	c.NotHasKey(files, gamma.ID, "a file-less skill must not appear in the map")

	files, err = f.st.Files(ctx, nil)
	c.Require().NoError(err, "Files with no ids")
	c.Empty(files, "Files(nil) returned")
}

func rowsByName(t *testing.T, st skills.Store, namespace, name string) []skills.Record {
	t.Helper()
	rows, err := st.List(context.Background(), false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []skills.Record
	for _, r := range rows {
		if r.Namespace == namespace && r.Name == name {
			out = append(out, r)
		}
	}
	return out
}
