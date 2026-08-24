package main

// mayMigrate reports whether a child bound to an executor in the given
// workspace mode may be re-bound to a DIFFERENT executor.
//
// Pinned means the child fails where it stood; ephemeral means the daemon may
// move it. An unknown or absent mode is pinned -- moving a child onto a machine
// no operator marked interchangeable is worse than failing it.
//
// This does not gate re-provisioning on the SAME executor: the executor keeps
// its workspace registry in memory, so a restart loses every id while the
// machine is perfectly fine, and rebuilding in place is not a migration.
func mayMigrate(mode string) bool {
	return workspaceModeOrPinned(mode) == "ephemeral"
}
