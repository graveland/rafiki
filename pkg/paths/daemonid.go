// SPDX-License-Identifier: Apache-2.0

package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"
)

// DaemonIDFile is where a daemon's generated id is kept.
func DaemonIDFile() string { return filepath.Join(DataDir(), "daemon-id") }

// DaemonID resolves this daemon's stable identity: the value a conversation
// lease records as its holder.
//
// Env first, then a file in the data directory, generating and persisting one
// on first run. Never a hostname — on darwin a hostname changes with the
// active network interface, which is the same reason MachineName refuses one.
//
// A pod filesystem is ephemeral, so a k8s deployment MUST set the environment
// variable: a daemon that comes back with a different id cannot use the
// same-holder fast path and waits out the full lease TTL before it can reclaim
// its own children.
//
// source is "env" or the file path, so a caller can tell an operator where the
// value came from.
func DaemonID() (id, source string, err error) {
	if v := strings.TrimSpace(Get(DaemonIDVar)); v != "" {
		return v, "env", nil
	}
	p := DaemonIDFile()
	if raw, rerr := os.ReadFile(p); rerr == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v, p, nil
		}
		// A blank or whitespace-only file is corruption, not an id. Fall
		// through and replace it.
	}
	generated := ulid.Make().String()
	if werr := writeDaemonID(p, generated); werr != nil {
		return "", p, fmt.Errorf("persist daemon id: %w", werr)
	}
	return generated, p, nil
}

// writeDaemonID persists id at p via a temp file plus rename, so two
// concurrent first runs cannot leave a reader observing a truncated value.
func writeDaemonID(p, id string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "daemon-id.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(id); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}
