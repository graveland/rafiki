package main

import (
	"slices"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestDefaultTypeSetShowsTaskChangesForOneChildOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq(true, slices.Contains(defaultTypeSet(true), "tasks_changed"), "a single-child tail must include tasks_changed")
	c.Eq(true, slices.Contains(defaultTypeSet(true), "system_message"), "a single-child tail must show system rows")
	c.Eq(false, slices.Contains(defaultTypeSet(false), "tasks_changed"), "the fleet-wide default stays the rail's set")
	c.Eq(false, slices.Contains(defaultTypeSet(false), "system_message"), "the fleet-wide default stays the rail's set")
}
