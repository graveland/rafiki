// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestActiveMarkerIsScopedToTheProfile(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	c.NoError(setActive("work", "child-w"), "setActive(work)")
	c.NoError(setActive("personal", "child-p"), "setActive(personal)")
	c.Eq("child-w", getActive("work"), "work active")
	c.Eq("child-p", getActive("personal"), "personal active")
}

func TestActiveMarkerIsEmptyForAProfileThatHasNone(t *testing.T) {
	isolateProfiles(t)
	assert.NewAborting(t).Eq("", getActive("fresh"), "getActive(fresh)")
}
