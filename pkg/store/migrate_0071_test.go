// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// TestMigrate0071WorkspaceToolsColumns pins the pair of columns migration 0071
// adds and, more importantly, that each CHECK actually refuses a value neither
// the daemon nor a preset write may carry: the child's admits only ” and
// 'rafiki' (NOT NULL, default ”), the preset's only NULL, 'native' and
// 'rafiki' (NULL = unset, so the spawn's own choice stands).
func TestMigrate0071WorkspaceToolsColumns(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	var dflt string
	err := pool.QueryRow(ctx, `
		SELECT column_default FROM information_schema.columns
		WHERE table_schema = 'conversations' AND table_name = 'child'
		  AND column_name = 'workspace_tools'`).Scan(&dflt)
	c.NoError(err, "child.workspace_tools column exists")
	c.Eq("''::text", dflt, "child.workspace_tools default")

	var nullable string
	err = pool.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'conversations' AND table_name = 'presets'
		  AND column_name = 'workspace_tools'`).Scan(&nullable)
	c.NoError(err, "presets.workspace_tools column exists")
	c.Eq("YES", nullable, "presets.workspace_tools is nullable")

	childDef := constraintDef(t, ctx, pool, "conversations.child", "workspace_tools")
	c.True(strings.Contains(childDef, "'rafiki'"), "child CHECK admits 'rafiki', got "+childDef)
	c.False(strings.Contains(childDef, "'native'"), "child CHECK must not admit 'native', got "+childDef)

	presetDef := constraintDef(t, ctx, pool, "conversations.presets", "workspace_tools")
	c.True(strings.Contains(presetDef, "'native'") && strings.Contains(presetDef, "'rafiki'"),
		"presets CHECK admits 'native' and 'rafiki', got "+presetDef)

	// Both CHECKs refuse a value outside their vocabulary.
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, workspace_tools)
		VALUES ('c_ws_check', 'claude', 'idle', now(), 'x')`)
	c.Error(err, "child CHECK admitted 'x'")
	c.Eq("23514", sqlState(err), "child 'x' must be a check violation")

	_, err = pool.Exec(ctx,
		`INSERT INTO conversations.presets (name, workspace_tools) VALUES ('ws-check', 'x')`)
	c.Error(err, "presets CHECK admitted 'x'")
	c.Eq("23514", sqlState(err), "presets 'x' must be a check violation")

	// The values each column does admit.
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, workspace_tools)
		VALUES ('c_ws_rafiki', 'claude', 'idle', now(), 'rafiki')`)
	c.NoError(err, "child admits 'rafiki'")

	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.presets (name, workspace_tools)
		VALUES ('ws-native', 'native'), ('ws-rafiki', 'rafiki'), ('ws-null', NULL)`)
	c.NoError(err, "presets admit 'native', 'rafiki' and NULL")
}

// constraintDef returns the definition of the CHECK constraint covering
// column on table, failing the test when there is none.
func constraintDef(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column string) string {
	t.Helper()
	c := assert.NewAborting(t)
	var def string
	err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%' || $2 || '%'`, table, column).Scan(&def)
	c.NoError(err, "%s.%s CHECK constraint exists", table, column)
	return def
}
