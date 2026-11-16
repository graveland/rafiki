package paths

// resetInstanceForTest restores the process-wide instance. Tests only: the
// instance is deliberately process-global (SetInstance is called once, at
// startup, before any path is read), so a test that names one must not leave it
// behind for the next test in this package.
func resetInstanceForTest() {
	instanceMu.Lock()
	instanceName = DefaultInstance
	instanceMu.Unlock()
}
