//go:build darwin

package paths

import (
	"fmt"
	"syscall"
)

// PIDNamespaceToken returns a per-boot token on darwin, which has no PID
// namespaces and no containers — a daemon restart on the same boot shares one
// pid space, and a reboot invalidates every recorded pid.
func PIDNamespaceToken() (string, bool) {
	bt, err := syscall.Sysctl("kern.boottime")
	if err != nil || bt == "" {
		return "", false
	}
	return fmt.Sprintf("darwin-boot:%x", bt), true
}
