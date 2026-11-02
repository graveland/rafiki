// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// freshOwnerRow inserts the minimum conversations.users row an MCP credential
// needs for its foreign key. The scratch database is dropped at test end, so
// the id only has to be unique within the test.
func freshOwnerRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tag string) string {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	username := "mcp_mig_" + tag + "_" + id
	_, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (id, username, token_sha256) VALUES ($1::uuid, $2, $3)`,
		id, username, "sha256-"+username)
	assert.NewAborting(t).NoError(err, "insert owner row")
	return id
}

// sqlState returns the SQLSTATE of a PostgreSQL error, or "".
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// TestMcpOAuthMigrationCreatesOAuthTables pins 0067's schema: the grant and
// endpoint tables, their composite primary keys (which are also the ownership
// uniqueness rules), the all-or-nothing binding CHECKs, the named state CHECKs,
// and the partial indexes a resource logout walks.
func TestMcpOAuthMigrationCreatesOAuthTables(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.NoError(Migrate(ctx, pool), "migrate")

	for _, table := range []string{"mcp_credential", "mcp_endpoint"} {
		var exists bool
		c.NoError(pool.QueryRow(ctx,
			`SELECT to_regclass('conversations.'||$1) IS NOT NULL`, table).Scan(&exists), "probe %s", table)
		c.True(exists, "conversations.%s missing after Migrate", table)
	}

	var grantCols int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'conversations' AND table_name = 'mcp_credential'
		   AND column_name IN ('owner_user_id','resource','issuer','generation','key_id',
		       'credential_sealed','client_id','auth_style','authorization_endpoint',
		       'token_endpoint','revocation_endpoint','scopes','redirect_url','token_type',
		       'access_expiry','refresh_state','refresh_version','invalidated_at',
		       'created_at','updated_at')`).Scan(&grantCols), "probe mcp_credential columns")
	c.Eq(20, grantCols, "mcp_credential columns")

	var endpointCols int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'conversations' AND table_name = 'mcp_endpoint'
		   AND column_name IN ('owner_user_id','endpoint','generation','bound_resource',
		       'bound_issuer','bound_grant_generation','pending_resource','pending_issuer',
		       'pending_grant_generation','created_at','updated_at')`).Scan(&endpointCols),
		"probe mcp_endpoint columns")
	c.Eq(11, endpointCols, "mcp_endpoint columns")

	// The primary keys are the ownership uniqueness rules: one grant per
	// (owner, resource) and one row per (owner, endpoint).
	for _, tc := range []struct{ table, want string }{
		{"mcp_credential", "PRIMARY KEY (owner_user_id, resource)"},
		{"mcp_endpoint", "PRIMARY KEY (owner_user_id, endpoint)"},
	} {
		var def string
		c.NoError(pool.QueryRow(ctx, `
			SELECT pg_get_constraintdef(oid) FROM pg_constraint
			 WHERE conrelid = ('conversations.'||$1::text)::regclass AND contype = 'p'`,
			tc.table).Scan(&def), "probe %s primary key", tc.table)
		c.Eq(tc.want, def, "%s primary key", tc.table)
	}

	var indexes int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		 WHERE schemaname = 'conversations' AND tablename = 'mcp_endpoint'
		   AND indexname IN ('mcp_endpoint_bound_resource','mcp_endpoint_pending_resource')`).
		Scan(&indexes), "probe endpoint indexes")
	c.Eq(2, indexes, "endpoint indexes")

	owner := freshOwnerRow(t, ctx, pool, "schema")
	insertGrant := func(resource, authStyle, refreshState string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.mcp_credential
			   (owner_user_id, resource, issuer, generation, key_id, credential_sealed,
			    auth_style, refresh_state)
			VALUES ($1::uuid, $2, 'https://issuer.example.com', 1, 'k', '\x00'::bytea, $3, $4)`,
			owner, resource, authStyle, refreshState)
		return err
	}
	c.NoError(insertGrant("https://resource.example.com/", "auto", "idle"), "a valid grant row")
	c.Eq("23514", sqlState(insertGrant("https://other.example.com/", "bogus", "idle")),
		"an unmodelled auth style must be a check violation")
	c.Eq("23514", sqlState(insertGrant("https://third.example.com/", "auto", "bogus")),
		"an unmodelled refresh state must be a check violation")

	// A duplicate (owner, resource) is refused by the primary key.
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.mcp_credential
		   (owner_user_id, resource, issuer, generation, credential_sealed)
		VALUES ($1::uuid, 'https://resource.example.com/', 'i', 2, '\x00'::bytea)`, owner)
	c.Eq("23505", sqlState(err), "a second grant for one (owner, resource)")

	// The FK means an unknown owner cannot hold a credential.
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.mcp_credential
		   (owner_user_id, resource, issuer, generation, credential_sealed)
		VALUES ('00000000-0000-4000-8000-000000000000'::uuid, 'https://x.example.com/', 'i', 1, '\x00'::bytea)`)
	c.Eq("23503", sqlState(err), "a credential for an unknown owner")

	// A binding is all three columns or none: a lone bound_resource is refused.
	insertEndpoint := func(endpoint string, extra string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.mcp_endpoint (owner_user_id, endpoint`+extra+`) VALUES ($1::uuid, $2)`,
			owner, endpoint)
		return err
	}
	c.NoError(insertEndpoint("https://endpoint.example.com/mcp", ""), "an unbound endpoint row exists before login")
	c.Eq("23514", sqlState(func() error {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.mcp_endpoint
			   (owner_user_id, endpoint, bound_resource)
			VALUES ($1::uuid, 'https://lone.example.com/mcp', 'https://resource.example.com/')`, owner)
		return err
	}()), "a lone bound_resource must be refused")
	c.Eq("23514", sqlState(func() error {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.mcp_endpoint
			   (owner_user_id, endpoint, pending_grant_generation)
			VALUES ($1::uuid, 'https://pending.example.com/mcp', 0)`, owner)
		return err
	}()), "a lone pending_grant_generation must be refused")
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.mcp_endpoint
		   (owner_user_id, endpoint, bound_resource, bound_issuer, bound_grant_generation)
		VALUES ($1::uuid, 'https://bound.example.com/mcp', 'https://resource.example.com/', 'https://issuer.example.com', 1)`, owner)
	c.NoError(err, "a complete binding")
	c.Eq("23505", sqlState(func() error {
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.mcp_endpoint (owner_user_id, endpoint)
			VALUES ($1::uuid, 'https://bound.example.com/mcp')`, owner)
		return err
	}()), "a second row for one (owner, endpoint)")

	// A tombstone is a row: logout keeps the incremented generation, wipes the
	// secrets and leaves the row readable.
	_, err = pool.Exec(ctx, `
		UPDATE conversations.mcp_credential
		   SET generation = generation + 1, invalidated_at = now(),
		       key_id = '', credential_sealed = ''
		 WHERE owner_user_id = $1::uuid AND resource = 'https://resource.example.com/'`, owner)
	c.NoError(err, "tombstone the grant")
	var generation int64
	var invalidated bool
	var sealed []byte
	c.NoError(pool.QueryRow(ctx, `
		SELECT generation, invalidated_at IS NOT NULL, credential_sealed
		  FROM conversations.mcp_credential
		 WHERE owner_user_id = $1::uuid AND resource = 'https://resource.example.com/'`, owner).
		Scan(&generation, &invalidated, &sealed), "read the tombstone")
	c.Eq(int64(2), generation, "the tombstone generation")
	c.True(invalidated, "the grant is not tombstoned")
	c.Len(sealed, 0, "the tombstone kept its sealed blob")

	// The down migration must reverse every object the up migration added.
	down, err := os.ReadFile("migrations/0067_mcp_credentials.down.sql")
	c.Require().NoError(err, "read down migration")
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	for _, table := range []string{"mcp_credential", "mcp_endpoint"} {
		var exists bool
		c.NoError(pool.QueryRow(ctx,
			`SELECT to_regclass('conversations.'||$1) IS NOT NULL`, table).Scan(&exists), "probe %s after down", table)
		c.False(exists, "conversations.%s survived the down migration", table)
	}
	var leftoverIndexes int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		 WHERE schemaname = 'conversations'
		   AND indexname IN ('mcp_endpoint_bound_resource','mcp_endpoint_pending_resource')`).
		Scan(&leftoverIndexes), "probe indexes after down")
	c.Eq(0, leftoverIndexes, "an MCP endpoint index survived the down migration")
}
