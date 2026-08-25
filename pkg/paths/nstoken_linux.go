//go:build linux

package paths

import "os"

// PIDNamespaceToken identifies the PID namespace this process runs in, so a
// recorded pid can be proven to belong to the same namespace before it is
// signalled.
//
// The daemon id cannot answer this: it is pinned across pod restarts on
// purpose, and a restarted pod has the same id and a fresh namespace where
// every recorded pid names an unrelated process.
//
// ok=false means the question could not be answered, and the caller must NOT
// signal. That is the safe direction: a missed orphan is reparented by the OS
// and cannot reach the new daemon, while a wrong signal kills a live process.
func PIDNamespaceToken() (string, bool) {
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil || link == "" {
		return "", false
	}
	return link, true
}
