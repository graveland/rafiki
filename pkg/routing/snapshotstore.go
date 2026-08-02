// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"os"
	"path/filepath"
)

// FileSnapshotStore is a SnapshotStore backed by a single JSON file. The
// caller owns the filesystem policy (which path, under whose config or cache
// directory); this package owns the snapshot's schema.
//
// This exists because every consumer of a disk-cached catalog was otherwise
// writing the same twenty lines: fundi's --model completion, sc's rafiki
// launcher, and the `fundid agent` pricer that is currently left nil precisely
// for want of one (see cmd/fundid/agent_cli.go). A third copy was the point to
// stop copying.
type FileSnapshotStore struct {
	// Path is the snapshot file. Its parent directory is created on Save.
	Path string
}

// Load returns the snapshot bytes. A missing file returns the os.ErrNotExist
// error unchanged — ModelCatalog.loadCache treats any error as "no usable
// snapshot" and proceeds to fetch, so a cold cache needs no special case here.
func (s FileSnapshotStore) Load() ([]byte, error) { return os.ReadFile(s.Path) }

// Save writes the snapshot atomically: a temp file in the destination
// directory, then a rename over the target.
//
// The atomicity is not ceremonial. Snapshot writers and readers are separate
// short-lived processes racing on the same path — a shell completion function
// runs on every TAB press while a daemon may be refreshing the same file — and
// a plain WriteFile is a truncate followed by a write, so a reader landing in
// between gets an empty or half-written file. That surfaces as a JSON decode
// error and a silently empty catalog, i.e. completion offering nothing, which
// looks exactly like "the network is down" and is very hard to attribute.
// Rename within a directory is atomic on every platform that matters.
func (s FileSnapshotStore) Save(data []byte) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(s.Path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup on every failure path below; a successful rename
	// makes this a no-op removal of a name that no longer exists.
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil { // CreateTemp makes it 0600
		return err
	}
	return os.Rename(tmp, s.Path)
}
