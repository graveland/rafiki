// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// listenUnixReady listens on a unix socket so that path only ever names a
// socket that is already accepting connections. net.Listen creates the socket
// file at bind() but accepts only after listen(), and a client watching for
// the file (rafiki setup's waitForSocket) that dials in that window is refused
// and never sees another event. So the socket is bound and listened on under
// a sibling temporary name and renamed into place: the rename is atomic and
// fsnotify reports it as a Create of path.
//
// The temporary name is never longer than path (readyTempPath), so a path
// that fits the platform's sun_path limit (~104 bytes on darwin) still binds.
//
// The socket is created 0600 (umask before bind, not chmod after: chmod leaves
// a window in which another local user can connect). Closing the returned
// listener removes path, as net.Listen's own listener would.
func listenUnixReady(path string) (net.Listener, error) {
	tmp := readyTempPath(path)
	// A leftover from a daemon that died between listen and rename.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot remove stale socket %s: %w", tmp, err)
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", tmp)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	ul := ln.(*net.UnixListener)
	// The listener would unlink tmp, a name it no longer has; readyUnixListener
	// removes path instead.
	ul.SetUnlinkOnClose(false)
	if err := os.Rename(tmp, path); err != nil {
		_ = ul.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("publish socket %s: %w", path, err)
	}
	return &readyUnixListener{UnixListener: ul, path: path}, nil
}

// readyTempPath names listenUnixReady's temporary socket: a hidden sibling of
// path with the same length ("controller.sock" → ".controller.soc").
func readyTempPath(path string) string {
	dir, base := filepath.Split(path)
	if len(base) < 2 {
		return dir + "~"
	}
	return dir + "." + base[:len(base)-1]
}

// readyUnixListener removes its published path on the first Close.
type readyUnixListener struct {
	*net.UnixListener
	path string
	once sync.Once
}

func (l *readyUnixListener) Close() error {
	err := l.UnixListener.Close()
	l.once.Do(func() {
		if rerr := os.Remove(l.path); rerr != nil && !os.IsNotExist(rerr) && err == nil {
			err = rerr
		}
	})
	return err
}
