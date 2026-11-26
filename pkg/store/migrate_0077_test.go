// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/claudethread"

	"github.com/multigres/testkit/assert"
)

// migrate0077ID returns a per-run unique id, so a failure names the run it came
// from. The scratch database is fresh, but ids stay distinctive for the same
// reason 0076's do.
func migrate0077ID(tag string) string {
	return fmt.Sprintf("c_m77%s%d", tag, time.Now().UnixNano())
}

// seedM77User inserts an owner so the backfilled row's owner_user_id can be
// checked against the parent's rather than against NULL.
func seedM77User(t *testing.T, ctx context.Context, pool *pgxpool.Pool, username string) string {
	t.Helper()
	var id string
	assert.NewAborting(t).Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256) VALUES ($1, $1) RETURNING id::text`,
		username).Scan(&id), "insert owner user %s", username)
	return id
}

// seedM77ParentChild inserts the parent a thread conversation is joined to. It
// carries provider/model/thinking and a closed_at so the backfill's copy arms
// have something to copy.
func seedM77ParentChild(t *testing.T, ctx context.Context, pool *pgxpool.Pool, childID, status string, ownerUserID *string, closedAt *time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.child
		       (child_id, kind, name, status, spawned_at, owner_user_id,
		        provider, model, thinking, closed_at)
		VALUES ($1, 'claude', $2, $3, now(), $4::uuid,
		        'anthropic', 'claude-sonnet-4-5-20250929', 'high', $5)`,
		childID, "parent-"+childID, status, ownerUserID, closedAt)
	assert.NewAborting(t).Require().NoError(err, "insert parent child %s", childID)
}

// seedM77Thread inserts a conversation with external_ref ref and one turn whose
// prefix_content carries the given raw `tools` value (toolsRaw "" means the key
// is absent, which is a different shape from a JSON null).
func seedM77Thread(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ref, toolsRaw string) string {
	t.Helper()
	var convID string
	assert.NewAborting(t).Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref)
		VALUES ('claude', 'client', $1) RETURNING id::text`, ref).Scan(&convID),
		"insert conversation %s", ref)
	prefix := "{}"
	if toolsRaw != "" {
		prefix = `{"tools":` + toolsRaw + `}`
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn (conversation_id, ordinal, prefix_content)
		VALUES ($1::uuid, 0, $2::jsonb)`, convID, prefix)
	assert.NewAborting(t).Require().NoError(err, "insert a turn for %s", ref)
	return convID
}

// run0077SQL applies migration 0077's own SQL again. The chain records it as
// applied, so this is the only way to re-run the statement — which is what the
// idempotence assertion needs.
func run0077SQL(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	migrations, err := loadMigrations()
	c.Require().NoError(err, "load migrations")
	for _, m := range migrations {
		if m.version == 77 {
			_, err := pool.Exec(ctx, m.sql)
			c.Require().NoError(err, "re-run 0077")
			return
		}
	}
	t.Fatal("migration 0077 is not embedded")
}

// TestMigrate0077BackfillsSubagentThreadsOnly pins the backfill: exactly one row
// is created, for the conversation that is a thread of a real child AND whose
// turns declare a client tool. A helper thread (a server tool only), a thread
// whose only declaration carries a JSON null input_schema, and an mcp:<user>
// conversation whose first segment names no child are all left alone. Re-running
// the statement creates no duplicate.
func TestMigrate0077BackfillsSubagentThreadsOnly(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(migrateTo(ctx, pool, 76), "migrate to 0076, one short of 0077")

	parentID := migrate0077ID("parent")
	helperParentID := migrate0077ID("helperparent")
	owner := seedM77User(t, ctx, pool, "m77owner")
	seedM77ParentChild(t, ctx, pool, parentID, "idle", &owner, nil)
	// A second, unrelated parent so the helper threads hang off a real child
	// too: their exclusion must come from the tools rule, not from a missing
	// parent row.
	seedM77ParentChild(t, ctx, pool, helperParentID, "idle", &owner, nil)

	subRef := parentID + ":thread-sub"
	helperRef := parentID + ":thread-helper"
	nullSchemaRef := helperParentID + ":thread-nullschema"
	deepRef := parentID + ":thread-deep:extra"
	mcpRef := "mcp:user:0f0e1d2c-3b4a-4c5d-8e9f-0a1b2c3d4e5f"

	subConv := seedM77Thread(t, ctx, pool, subRef,
		`[{"name":"Bash","input_schema":{"type":"object"}}]`)
	seedM77Thread(t, ctx, pool, helperRef, `[{"name":"web_search","type":"web_search_20250305"}]`)
	seedM77Thread(t, ctx, pool, nullSchemaRef, `[{"name":"Bash","input_schema":null}]`)
	// A ref with THREE segments is not a thread of the child its first segment
	// names: taking it would give the row the wrong parent and a name built from
	// the second segment. It declares a client tool, so only the two-segment rule
	// excludes it.
	seedM77Thread(t, ctx, pool, deepRef, `[{"name":"Bash","input_schema":{"type":"object"}}]`)
	seedM77Thread(t, ctx, pool, mcpRef, `[{"name":"Bash","input_schema":{"type":"object"}}]`)

	c.Require().NoError(migrateTo(ctx, pool, 77), "migrate to 0077")

	// Exactly one row, and only the subagent thread has one.
	var nativeRows int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.child WHERE labels ? 'rafiki/native-subagent'`).
		Scan(&nativeRows), "count backfilled rows")
	c.Eq(1, nativeRows, "rows created by the backfill")
	for _, ref := range []string{helperRef, nullSchemaRef, deepRef, mcpRef} {
		var n int
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT count(*) FROM conversations.child WHERE child_id = $1`, ref).Scan(&n),
			"count rows for %s", ref)
		c.Eq(0, n, "%s must not be backfilled as a subagent child", ref)
	}

	var (
		convID, ownerID, kind, name, status      string
		provider, model, thinking                string
		parentCol, rootCol, nativeLabel, exitSig string
		labels                                   map[string]string
		pid                                      *int
		closedAt                                 *time.Time
		spawnedAt                                time.Time
	)
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT COALESCE(conversation_id::text, ''), COALESCE(owner_user_id::text, ''),
		       kind, COALESCE(name, ''), status,
		       COALESCE(provider, ''), COALESCE(model, ''), COALESCE(thinking, ''),
		       COALESCE(parent_id, ''), COALESCE(root_id, ''),
		       COALESCE(labels->>'rafiki/native-subagent', ''), labels, pid, closed_at,
		       COALESCE(exit_signal, ''), spawned_at
		  FROM conversations.child WHERE child_id = $1`, subRef).
		Scan(&convID, &ownerID, &kind, &name, &status,
			&provider, &model, &thinking,
			&parentCol, &rootCol, &nativeLabel, &labels, &pid, &closedAt,
			&exitSig, &spawnedAt), "read the backfilled row")

	c.Eq(subConv, convID, "conversation_id is the thread's own conversation")
	c.Eq(owner, ownerID, "owner_user_id is copied from the parent")
	c.Eq("claude", kind, "kind")
	c.Eq("task:thread-sub", name, "name is task:<thread>")
	c.Eq("idle", status, "a thread of a live parent loads idle")
	c.Eq("anthropic", provider, "provider copied from the parent")
	c.Eq("claude-sonnet-4-5-20250929", model, "model copied from the parent")
	c.Eq("high", thinking, "thinking copied from the parent")
	c.Eq(parentID, parentCol, "parent_id (generated from the label)")
	c.Eq(parentID, rootCol, "root_id is the parent's root, which for a top-level parent is itself")
	c.Eq("1", nativeLabel, "the native-subagent marker")
	c.Eq("", exitSig, "a thread that never ran a process has no signal")
	c.True(pid == nil, "pid must be NULL: a native child has no process, got %v", pid)
	c.True(closedAt == nil, "closed_at follows the parent's, which is NULL here")
	c.True(!spawnedAt.IsZero(), "spawned_at must be set (NOT NULL)")

	// The full label set, so a key that gained or lost a spelling is a failure
	// rather than something to remember to look for.
	c.EqDeep(map[string]string{
		"rafiki/parent":          parentID,
		"rafiki/root":            parentID,
		"rafiki/native-subagent": "1",
	}, labels, "labels")

	// Idempotent: the statement is ON CONFLICT (child_id) DO NOTHING, so a
	// second application adds nothing.
	run0077SQL(t, ctx, pool)
	var after int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.child WHERE child_id = $1`, subRef).Scan(&after),
		"count after re-running 0077")
	c.Eq(1, after, "re-running 0077 must not duplicate a thread's row")
}

// TestMigrate0077ThreadOfADeadParentLoadsExited pins the other half of the
// status rule: a thread whose parent is gone ended with it, so it loads exited
// and inherits the parent's close tombstone.
func TestMigrate0077ThreadOfADeadParentLoadsExited(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(migrateTo(ctx, pool, 76), "migrate to 0076, one short of 0077")

	parentID := migrate0077ID("deadparent")
	closedAt := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	seedM77ParentChild(t, ctx, pool, parentID, "exited", nil, &closedAt)
	ref := parentID + ":thread-dead"
	seedM77Thread(t, ctx, pool, ref, `[{"name":"Task","input_schema":{"type":"object"}}]`)

	c.Require().NoError(migrateTo(ctx, pool, 77), "migrate to 0077")

	var (
		status   string
		ownerID  *string
		gotClose *time.Time
	)
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT status, owner_user_id::text, closed_at FROM conversations.child WHERE child_id = $1`, ref).
		Scan(&status, &ownerID, &gotClose), "read the backfilled row")
	c.Eq("exited", status, "a thread of an exited parent ended with it")
	c.True(ownerID == nil, "an unowned parent yields an unowned thread, got %v", ownerID)
	c.Require().True(gotClose != nil, "closed_at must follow the parent's")
	c.True(gotClose.Equal(closedAt), "closed_at = %v, want the parent's %v", gotClose, closedAt)
}

// TestMigrate0077DownRemovesOnlyPositionlessNativeRows pins the down statement:
// it removes native rows with no pid and leaves everything else — a real child
// that happens to carry the label keeps its row.
func TestMigrate0077DownRemovesOnlyPositionlessNativeRows(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	nativeID := migrate0077ID("downnative")
	pidID := migrate0077ID("downpid")
	c.Require().NoError(insertMigrate0076Child(t, ctx, pool, nativeID,
		`{"rafiki/native-subagent":"1"}`), "seed a native row")
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, pid, labels)
		VALUES ($1, 'claude', 'idle', now(), 4242, '{"rafiki/native-subagent":"1"}'::jsonb)`, pidID)
	c.Require().NoError(err, "seed a labelled row that has a pid")

	down, err := os.ReadFile("migrations/0077_native_subagent_children.down.sql")
	c.Require().NoError(err, "read the down migration")
	_, err = pool.Exec(ctx, string(down))
	c.Require().NoError(err, "apply the down migration")

	for id, want := range map[string]int{nativeID: 0, pidID: 1} {
		var n int
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT count(*) FROM conversations.child WHERE child_id = $1`, id).Scan(&n),
			"count %s", id)
		c.Eq(want, n, "%s rows after the down migration", id)
	}
}

// TestBackfillPredicateMatchesDeclaresClientTools applies migration 0077's own
// statement to every body in the shared fixture table
// (pkg/claudethread/testdata/declares_client_tools.json) and requires the row to
// appear exactly when claudethread.DeclaresClientTools says the thread declares
// a client tool. The fixtures are the same table the Go rule is pinned against,
// so a change to either implementation fails here.
func TestBackfillPredicateMatchesDeclaresClientTools(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(migrateTo(ctx, pool, 76), "migrate to 0076, one short of 0077")

	raw, err := os.ReadFile("../claudethread/testdata/declares_client_tools.json")
	c.Require().NoError(err, "read the shared fixture table")
	var fixtures []struct {
		Name        string          `json:"name"`
		Why         string          `json:"why"`
		Tools       json.RawMessage `json:"tools"`
		ClientTools bool            `json:"client_tools"`
	}
	c.Require().NoError(json.Unmarshal(raw, &fixtures), "decode the shared fixture table")
	c.Require().NotEmpty(fixtures, "the shared fixture table is empty")

	refs := make([]string, len(fixtures))
	for i, f := range fixtures {
		parentID := migrate0077ID(fmt.Sprintf("fx%d", i))
		seedM77ParentChild(t, ctx, pool, parentID, "idle", nil, nil)
		refs[i] = parentID + ":thread-" + f.Name
		tools := ""
		if f.Tools != nil {
			tools = string(f.Tools)
		}
		seedM77Thread(t, ctx, pool, refs[i], tools)
	}

	c.Require().NoError(migrateTo(ctx, pool, 77), "migrate to 0077")

	for i, f := range fixtures {
		var n int
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT count(*) FROM conversations.child WHERE child_id = $1`, refs[i]).Scan(&n),
			"count the row for %s", f.Name)
		got := n == 1
		// The Go rule and the fixture's declared verdict must already agree (the
		// claudethread test pins that); asserting all three here is what makes a
		// divergence name which side moved.
		goRule := claudethread.DeclaresClientToolsFrom(f.Tools)
		c.Eq(f.ClientTools, goRule, "%s: DeclaresClientToolsFrom — %s", f.Name, f.Why)
		c.Eq(f.ClientTools, got,
			"%s: 0077's predicate must agree with DeclaresClientTools — %s", f.Name, f.Why)
	}
}
