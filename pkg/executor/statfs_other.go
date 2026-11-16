//go:build !openbsd

package executor

import "golang.org/x/sys/unix"

// statfsFreeBytes reports the bytes available to an unprivileged user on the
// filesystem holding path.
func statfsFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
