// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newServiceRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Apply an upgrade: migrate the database and restart the installed services",
		Long: `Bring the installed services onto this binary after an upgrade.

For an installed daemon service it applies the database migrations (` + "`rafikid migrate`" + `,
which the daemon does not run itself), then restarts it. For an installed
executor service it restarts it. A service whose unit launches a different
binary than this one is repointed first. With neither installed it does nothing,
so a package manager can call it after every install.

If the migration fails nothing is restarted.

An executor service is per instance: -P/--profile, else a non-empty $RAFIKI_PROFILE,
else the default instance names the one whose unit is refreshed. The
current-profile pointer is not consulted, so switching profiles cannot change
which service an upgrade restarts.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return refreshServices(cmdCtx(cmd), cmd, execRunner{}, cmdOut(cmd))
		},
	}
}

// refreshServices migrates, then restarts, whichever of the daemon and executor
// services are installed.
//
// cmd supplies the DECLARED executor instance — -P/--profile, else a non-empty
// $RAFIKI_PROFILE, else the default instance — and it is pinned before the
// executor backend is resolved. The unit, its label and its log path are per
// instance, so the pointer must not decide which one is inspected and
// restarted: `rafiki -P work service refresh` refreshes the work unit, and with
// no -P the default unit is refreshed whatever `rafiki profile use` last chose.
// (The daemon's unit is not per instance, so pinning changes nothing for it.)
func refreshServices(ctx context.Context, cmd *cobra.Command, r runner, out io.Writer) error {
	applyExecutorInstance(cmd)

	daemon, err := serviceBackendFor().Status()
	if err != nil {
		return err
	}
	if daemon.Installed {
		rafikid, err := findDaemonBinary()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "migrating the database")
		if _, stderr, err := r.Run(ctx, nil, rafikid, "migrate"); err != nil {
			return fmt.Errorf("migrate: %w\n%s\nnot restarting the daemon; fix the database and re-run `rafiki service refresh`", err, strings.TrimSpace(string(stderr)))
		}
		if err := refreshUnit(serviceBackendFor(), daemon, rafikid, "daemon", out); err != nil {
			return err
		}
	}

	executor, err := executorServiceBackendFor().Status()
	if err != nil {
		return err
	}
	if executor.Installed {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot determine this binary's path: %w", err)
		}
		return refreshUnit(executorServiceBackendFor(), executor, self, "executor", out)
	}
	return nil
}

// refreshUnit restarts the service, repointing it first when its unit launches
// a different binary than want. Repoint reloads the job, so it is the restart.
func refreshUnit(b serviceBackend, st serviceStatus, want, name string, out io.Writer) error {
	if st.Binary != "" && !sameBinary(st.Binary, want) {
		if err := b.Repoint(want); err != nil {
			return fmt.Errorf("repoint %s service: %w", name, err)
		}
		fmt.Fprintf(out, "%s service repointed to %s\n", name, want)
		return nil
	}
	if err := b.Restart(); err != nil {
		return fmt.Errorf("restart %s service: %w", name, err)
	}
	fmt.Fprintf(out, "%s service restarted\n", name)
	return nil
}
