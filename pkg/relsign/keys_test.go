// SPDX-License-Identifier: Apache-2.0

package relsign_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/relsign"
)

func TestTrustedKeysIsNotEmptyAndHasACurrentKey(t *testing.T) {
	c := assert.NewCollecting(t)
	c.GreaterOrEqual(1, len(relsign.TrustedKeys), "TrustedKeys must name at least one key")

	current := false
	for _, k := range relsign.TrustedKeys {
		if k.MaxVersion == "" {
			current = true
			break
		}
	}
	c.True(current, "TrustedKeys must include an unbounded current key")
}

func TestTrustedKeysAreWellFormed(t *testing.T) {
	c := assert.NewCollecting(t)
	seen := make(map[string]bool, len(relsign.TrustedKeys))
	for i, k := range relsign.TrustedKeys {
		c.Eq(ed25519.PublicKeySize, len(k.PublicKey), "TrustedKeys[%d].PublicKey length", i)
		c.Eq(k.ID, relsign.KeyID(k.PublicKey), "TrustedKeys[%d].ID does not match its public key", i)
		if k.MaxVersion != "" {
			_, err := relsign.ParseTag(k.MaxVersion)
			c.NoError(err, "TrustedKeys[%d].MaxVersion", i)
		}
		c.False(seen[k.ID], "TrustedKeys[%d].ID %q is duplicated", i, k.ID)
		seen[k.ID] = true
	}
}
