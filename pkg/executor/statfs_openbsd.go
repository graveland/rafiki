//go:build openbsd

package executor

import "golang.org/x/sys/unix"

// statfsFreeBytes reports the bytes available to an unprivileged user on the
// filesystem holding path. OpenBSD's unix.Statfs_t spells the same two numbers
// F_bavail/F_bsize, where every other platform has Bavail/Bsize.
func statfsFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	// OpenBSD's Statfs_t declares F_bavail as int64 and F_bsize as uint32.
	return uint64(st.F_bavail) * uint64(st.F_bsize), nil
}
