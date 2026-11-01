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
