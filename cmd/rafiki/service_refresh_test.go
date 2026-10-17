// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

type refreshFixture struct {
	daemon, executor *fakeServiceBackend
	run              *fakeRunner
	rafikid          string
	out              bytes.Buffer
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	binDir := t.TempDir()
	f := &refreshFixture{
		daemon:   &fakeServiceBackend{},
		executor: &fakeServiceBackend{},
		run:      newFakeRunner(),
		rafikid:  filepath.Join(binDir, "rafikid"),
	}
	assert.NewAborting(t).NoError(os.WriteFile(f.rafikid, []byte("#!/bin/sh\n"), 0o755), "write rafikid")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubServiceBackend(t, f.daemon)
	orig := executorServiceBackendFor
	executorServiceBackendFor = func() serviceBackend { return f.executor }
	t.Cleanup(func() { executorServiceBackendFor = orig })
	return f
}

func (f *refreshFixture) refresh() error {
	return refreshServices(context.Background(), f.run, &f.out)
}

func TestRefreshDoesNothingWhenNothingIsInstalled(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRefreshFixture(t)

	c.NoError(f.refresh(), "refresh")

	c.Eq(0, len(f.run.runCalls))
	c.False(f.daemon.restarted)
	c.False(f.executor.restarted)
}

func TestRefreshMigratesThenRestartsBothServices(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRefreshFixture(t)
	self, err := os.Executable()
	c.NoError(err, "os.Executable")
	f.daemon.status = serviceStatus{Installed: true, Binary: f.rafikid}
	f.executor.status = serviceStatus{Installed: true, Binary: self}

	c.NoError(f.refresh(), "refresh")

	c.Eq(1, len(f.run.runCalls))
	c.EqDeep([]string{f.rafikid, "migrate"}, f.run.runCalls[0].argv)
	c.True(f.daemon.restarted, "daemon not restarted")
	c.True(f.executor.restarted, "executor not restarted")
}

func TestRefreshDoesNotRestartWhenMigrationFails(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRefreshFixture(t)
	f.daemon.status = serviceStatus{Installed: true, Binary: f.rafikid}
	f.executor.status = serviceStatus{Installed: true}
	f.run.scriptFail([]string{f.rafikid, "migrate"}, errors.New("exit 1"), "connection refused")

	err := f.refresh()

	c.Error(err, "a failed migration must fail the refresh")
	c.False(f.daemon.restarted, "daemon restarted onto an unmigrated schema")
	c.False(f.executor.restarted, "executor restarted after a failed migration")
}

func TestRefreshRepointsAUnitOnAnotherBinary(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRefreshFixture(t)
	f.daemon.status = serviceStatus{Installed: true, Binary: "/old/rafikid"}
	f.executor.status = serviceStatus{Installed: true, Binary: "/old/rafiki"}
	self, err := os.Executable()
	c.NoError(err, "os.Executable")

	c.NoError(f.refresh(), "refresh")

	c.Eq(f.rafikid, f.daemon.repointed)
	c.Eq(self, f.executor.repointed)
	c.False(f.daemon.restarted, "a repoint already reloads the job")
}

func TestRefreshOnlyExecutorInstalledSkipsMigration(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRefreshFixture(t)
	f.executor.status = serviceStatus{Installed: true}

	c.NoError(f.refresh(), "refresh")

	c.Eq(0, len(f.run.runCalls))
	c.True(f.executor.restarted, "executor not restarted")
}
