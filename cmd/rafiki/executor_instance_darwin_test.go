//go:build darwin

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// The executor's launchd label is per instance: two instances on one OS user are
// two services (launchctl addresses a service by its label, so a shared label
// would make `stop` ambiguous), while the default instance keeps the label every
// existing install has.
func TestExecutorInstanceUnitNames(t *testing.T) {
	c := assert.NewAborting(t)

	c.Eq("dev.graveland.rafiki-executor", executorLaunchdLabelFor(paths.DefaultInstance), "default label")
	c.Eq("dev.graveland.rafiki-executor", executorLaunchdLabelFor(""), "an empty instance is the default")
	c.Eq("dev.graveland.rafiki-executor-work", executorLaunchdLabelFor("work"), "named label")

	resetInstance(t)
	c.NoError(paths.SetInstance("work"), "SetInstance(work)")
	backend, ok := newExecutorServiceBackend().(*darwinBackend)
	c.True(ok, "the executor backend is the darwin one")
	c.Eq("dev.graveland.rafiki-executor-work", backend.label, "the executor backend follows the instance")
}
