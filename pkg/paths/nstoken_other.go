//go:build !linux && !darwin

package paths

// PIDNamespaceToken cannot be answered on this platform, so orphan pids are
// never signalled. See the linux implementation for why that is the safe
// direction.
func PIDNamespaceToken() (string, bool) { return "", false }
