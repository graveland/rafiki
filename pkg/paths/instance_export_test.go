package paths

// resetInstanceForTest restores the process-wide instance and clears any pin.
// Tests only: the instance is deliberately process-global (set once, before any
// path is read), so a test that names or pins one must not leave it behind for
// the next test in this package.
func resetInstanceForTest() { ResetInstanceForTest() }
