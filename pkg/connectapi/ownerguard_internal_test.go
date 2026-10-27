// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestSetOwnerGuardSourceNilIsRefused pins the same rule as
// TestSetSkillManagerNilIsRefused and the Wave-2 seams: storing &src for a
// nil func would make Load() return a non-nil pointer whose call panics, and
// every handler consults this seam.
func TestSetOwnerGuardSourceNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetOwnerGuardSource(nil)
	assert.NewAborting(t).Nil(s.guards.Load(), "SetOwnerGuardSource(nil) stored a pointer")
}
