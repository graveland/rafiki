//go:build darwin

package main

import (
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// The executor's launchd label is per instance: two instances on one OS user are
// two services (launchctl addresses a service by its label, so a shared label
// would make `stop` ambiguous), while the default instance keeps the label every
// existing install has.
func TestExecutorInstanceUnitNames(t *testing.T) {
	c := assert.NewAborting(t)
	resetInstance(t)

	c.Eq("dev.graveland.rafiki-executor", executorLaunchdLabelFor(paths.DefaultInstance), "default label")
	c.Eq("dev.graveland.rafiki-executor", executorLaunchdLabelFor(""), "an empty instance is the default")
	c.Eq("dev.graveland.rafiki-executor-work", executorLaunchdLabelFor("work"), "named label")

	// Every service verb resolves its backend through executorServiceBackend, so
	// -P has to reach the label a `stop` or `uninstall` acts on.
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	verb := newExecutorServiceStatusCmd()
	root.AddCommand(verb)
	c.NoError(verb.ParseFlags([]string{"-P", "work"}), "parse -P work")

	b, err := executorServiceBackend(verb)
	c.NoError(err, "executorServiceBackend")
	backend, ok := b.(*darwinBackend)
	c.True(ok, "the executor backend is the darwin one")
	c.Eq("dev.graveland.rafiki-executor-work", backend.label, "the executor backend follows the instance")
}
