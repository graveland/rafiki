// SPDX-License-Identifier: Apache-2.0

// Package promptfile stages launch text — a claude child's system-prompt
// appendix and its MCP config JSON — as content-addressed files under the
// user's state directory, so neither text rides argv where `ps` exposes it.
package promptfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"go.graveland.dev/rafiki/pkg/paths"
)

// Dir is where launch files are staged on this machine.
func Dir() string { return filepath.Join(paths.StateDir(), "prompts") }

// Write stores text under dir as <sha256-hex(text)><ext> and returns the path.
//
// The name is the content hash, so identical text maps to the same file and a
// repeated write is a no-op: when the file already exists at the right size it
// is returned untouched and keeps its mtime. A file of the right name at the
// wrong size is a truncated earlier write and is replaced. The write lands
// through a temp file in dir and a rename, so a concurrent reader never sees a
// partial file; dir is 0700 and the file 0600.
func Write(dir, ext, text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	name := hex.EncodeToString(sum[:]) + ext
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("promptfile: write %s: %w", name, err)
	}
	path := filepath.Join(dir, name)
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(text)) {
		return path, nil
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("promptfile: write %s: %w", name, err)
	}
	if err := writeTemp(tmp, text); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("promptfile: write %s: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("promptfile: write %s: %w", name, err)
	}
	return path, nil
}

// writeTemp writes text to tmp and closes it, leaving tmp's path for the caller
// to rename or remove.
func writeTemp(tmp *os.File, text string) error {
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		return err
	}
	return tmp.Close()
}
