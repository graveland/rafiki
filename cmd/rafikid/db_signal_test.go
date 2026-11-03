// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/multigres/testkit/assert"
)

// A signal must CANCEL a db verb, not kill the process.
//
// Both destructive verbs finish their safety work in a defer: `db restore`
// rolls a physical restore back from it, and `db backup` restarts the embedded
// server from it. Go's default disposition for SIGINT/SIGTERM/SIGHUP skips every
// defer, so an interrupt would leave a physical restore's data directory
// displaced, or the embedded server stopped.
func TestDbVerbsCancelOnASignal(t *testing.T) {
	c := assert.NewAborting(t)

	db := newDbCmd()
	var leaf *cobra.Command
	for _, sub := range db.Commands() {
		if sub.Name() == "restore" {
			leaf = sub
		}
	}
	c.Require().NotNil(leaf, "the db group must have a restore verb")

	// A guard handler of our own keeps the process alive if the verb registered
	// NOTHING: without it, a broken implementation would kill the test binary
	// instead of failing the assertion below.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(guard) })

	// Cobra seeds the root's context with context.Background() in ExecuteC and
	// copies it onto the executing leaf before running the hooks; calling the
	// hook directly has to do the same, or NotifyContext panics on a nil parent.
	leaf.SetContext(context.Background())

	c.Require().NoError(db.PersistentPreRunE(leaf, nil), "the db group's pre-run")
	c.Require().NotNil(leaf.Context().Done(),
		"a db verb must run under a cancellable context, or its defers never run on an interrupt")

	c.Require().NoError(syscall.Kill(os.Getpid(), syscall.SIGTERM), "send SIGTERM to ourselves")
	select {
	case <-leaf.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the db verb's context: an interrupt would kill the process, " +
			"skipping the physical rollback and the server restart")
	}
}
