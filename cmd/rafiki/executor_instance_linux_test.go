//go:build linux

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// The executor's systemd unit is per instance: two instances on one OS user are
// two services with two credentials and two roots, so they cannot share a unit
// (`stop` would be ambiguous), while the default instance keeps the unit name
// every existing install has.
func TestExecutorInstanceUnitNames(t *testing.T) {
	c := assert.NewAborting(t)

	c.Eq("rafiki-executor", executorUnitName(paths.DefaultInstance), "default unit")
	c.Eq("rafiki-executor", executorUnitName(""), "an empty instance is the default")
	c.Eq("rafiki-executor-work", executorUnitName("work"), "named unit")

	resetInstance(t)
	c.NoError(paths.SetInstance("work"), "SetInstance(work)")
	backend, ok := newExecutorServiceBackend().(*linuxBackend)
	c.True(ok, "the executor backend is the linux one")
	c.Eq("rafiki-executor-work.service", backend.unit, "the executor backend follows the instance")
}
