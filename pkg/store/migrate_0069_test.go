// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestMigrate0069AddsNotifyPolicy(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	var dflt string
	err := pool.QueryRow(ctx, `
		SELECT column_default FROM information_schema.columns
		WHERE table_schema = 'conversations' AND table_name = 'child' AND column_name = 'notify_policy'`).Scan(&dflt)
	c.NoError(err, "notify_policy column exists")
	c.Eq("''::text", dflt, "notify_policy default")

	var def string
	err = pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'conversations.child'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%notify_policy%'`).Scan(&def)
	c.NoError(err, "notify_policy CHECK constraint exists")
	c.True(strings.Contains(def, "'result'"), "CHECK admits 'result', got "+def)
}
