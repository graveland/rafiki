// SPDX-License-Identifier: Apache-2.0

package protocol

// Path and repo sync are the daemon-brokered transfer of a tree or a git
// branch between two executors. These are the pure data shapes the Control
// RPCs carry and the daemon's syncer speaks; the wire mapping lives in
// pkg/connectapi and the mechanics (ReadTree/WriteTree, git bundle) in
// pkg/executor and cmd/rafikid.

// SyncEndpoint names one end of a transfer: an executor name or id and a
// path on it. SyncRepo reads the path as the repository directory.
type SyncEndpoint struct {
	Executor string
	Path     string
}

// SyncPathRequest copies Src's tree (or a single file) onto Dst. MaxBytes is
// the caller's cap; nil means no caller cap, and a present value must be > 0
// (zero meaning "unlimited" is a trap), refused by the syncer, not the
// handler.
type SyncPathRequest struct {
	Src       SyncEndpoint
	Dst       SyncEndpoint
	Overwrite bool
	MaxBytes  *int64
}

// SyncPathResult is what a SyncPath moved.
type SyncPathResult struct {
	Files int64
	Bytes int64
}

// SyncRepoRequest moves a git branch between two executors as a bundle.
type SyncRepoRequest struct {
	Src    SyncEndpoint
	Dst    SyncEndpoint
	Branch string
	Force  bool
}

// SyncRepoResult reports the destination's branch tip before and after the
// fetch: UpToDate means nothing moved, CreatedRepo that the destination had no
// repository.
type SyncRepoResult struct {
	OldOID      string
	NewOID      string
	CreatedRepo bool
	UpToDate    bool
}
