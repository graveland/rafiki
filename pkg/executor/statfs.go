// SPDX-License-Identifier: Apache-2.0

package executor

// freeBytes reports the number of bytes available to an unprivileged user on
// the filesystem holding path.
//
// The probe itself is per-platform because unix.Statfs_t spells the same two
// numbers differently: every platform but OpenBSD has Bavail/Bsize, OpenBSD
// has F_bavail/F_bsize (see statfs_other.go and statfs_openbsd.go). Keeping the
// call here means a new platform is one file, not an edit at every call site.
func freeBytes(path string) (uint64, error) { return statfsFreeBytes(path) }
