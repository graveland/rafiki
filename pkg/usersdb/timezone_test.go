// SPDX-License-Identifier: Apache-2.0

package usersdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// uniqueUsername names a user unique to this run, so a shared test database
// (or a scratch one) can never collide.
func uniqueUsername(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("tz_%s_%d", t.Name(), time.Now().UnixNano())
}

// TestSetTimezoneRoundTrip pins the column contract: a zone stored via
// SetTimezone reads back through raw SQL, and a nil pointer clears it to NULL
// (never the empty string — an empty zone means "the machine's zone", which a
// schedule must never resolve to).
func TestSetTimezoneRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	store, pool := testStore(t)
	ts := NewTimezoneStore(pool)

	username := uniqueUsername(t)
	if _, _, err := store.Create(ctx, users.NewUser{Username: username}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	zone := "Europe/Paris"
	c.Require().NoError(ts.SetTimezone(ctx, username, &zone), "set zone")

	var stored *string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT timezone FROM conversations.users WHERE username = $1`, username).Scan(&stored), "read back")
	c.Require().NotNil(stored, "timezone was not written")
	c.Eq(zone, *stored, "stored timezone")

	c.Require().NoError(ts.SetTimezone(ctx, username, nil), "clear zone")
	stored = nil
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT timezone FROM conversations.users WHERE username = $1`, username).Scan(&stored), "re-read")
	c.Nil(stored, "clearing must store NULL, not an empty string")
}

// TestSetTimezoneUnknownUser pins the refusal: an unknown name and a
// tombstoned name are both ErrNotFound, so the CLI can say "no user named …"
// rather than silently succeeding on a row that is not there.
func TestSetTimezoneUnknownUser(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	store, pool := testStore(t)
	ts := NewTimezoneStore(pool)

	zone := "UTC"
	c.ErrorIs(ts.SetTimezone(ctx, "nobody", &zone), users.ErrNotFound, "unknown user")

	// A tombstoned user is no more settable than an unknown one.
	username := uniqueUsername(t)
	if _, _, err := store.Create(ctx, users.NewUser{Username: username}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	c.Require().NoError(store.Delete(ctx, username), "tombstone user")
	c.ErrorIs(ts.SetTimezone(ctx, username, &zone), users.ErrNotFound, "tombstoned user")
}
