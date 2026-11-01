// SPDX-License-Identifier: Apache-2.0

package recalldb

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestInstalledExtensions pins the startup probe the daemon degrades by. The
// test database ships both extensions and migration 0036 creates them, so a
// scratch database answers true, true — the shape an operator's Postgres has,
// which is what makes the embedded install's false, false a real degradation
// rather than a broken probe.
func TestInstalledExtensions(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	textsearch, vector, err := st.InstalledExtensions(context.Background())
	c.NoError(err, "InstalledExtensions")
	c.True(textsearch, "pg_textsearch")
	c.True(vector, "vector")
}

// TestInstalledExtensionsAnswersByExtensionName pins the two answers the daemon
// actually branches on, which a `true, true`-only test cannot: a probe that
// hardcodes success, or that crosses the two names, passes TestInstalledExtensions
// above. Three shapes here:
//
//  1. an arbitrary name — `ltree`, which this repo never asks about — answers
//     true, and a name no server ships answers false, proving the probe can say
//     false at all and that answers are keyed by NAME, not by position;
//  2. with vector dropped from this scratch database, InstalledExtensions
//     answers (true, false) — a crossed pair would read (false, true);
//  3. with pg_textsearch dropped too, it answers (false, false): the embedded
//     install's exact shape.
//
// DROP EXTENSION ... CASCADE takes the bm25 indexes and the vector columns with
// it, which is fine here — this test only reads pg_extension afterwards, and the
// scratch database is thrown away.
func TestInstalledExtensionsAnswersByExtensionName(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()

	// 1. Per-name answers, one installed and one that no Postgres has.
	have, err := st.installedExtensions(ctx, "ltree", "rafiki_no_such_extension")
	c.NoError(err, "installedExtensions")
	c.True(have["ltree"], "ltree is installed by migration 0036: %v", have)
	c.False(have["rafiki_no_such_extension"], "a name no server ships answered true: %v", have)
	c.Len(have, 1, "the probe answered for names it was not asked about: %v", have)

	// 2. One of the two recall extensions gone: the pair flips on the RIGHT side.
	if _, err := pool.Exec(ctx, `DROP EXTENSION vector CASCADE`); err != nil {
		t.Fatalf("drop vector: %v", err)
	}
	textsearch, vector, err := st.InstalledExtensions(ctx)
	c.NoError(err, "InstalledExtensions after dropping vector")
	c.True(textsearch, "pg_textsearch after dropping only vector")
	c.False(vector, "vector after DROP EXTENSION vector")

	// 3. Both gone: what an embedded install answers.
	if _, err := pool.Exec(ctx, `DROP EXTENSION pg_textsearch CASCADE`); err != nil {
		t.Fatalf("drop pg_textsearch: %v", err)
	}
	textsearch, vector, err = st.InstalledExtensions(ctx)
	c.NoError(err, "InstalledExtensions with neither extension")
	c.False(textsearch, "pg_textsearch after DROP EXTENSION pg_textsearch")
	c.False(vector, "vector after both drops")
}
