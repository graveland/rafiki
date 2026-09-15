// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// EnsureAnalyzerDir seeds dir from EmbeddedDefaultDir the first time it's
// used: if dir/profiles.yaml already exists, this is a no-op -- the caller's
// copy is theirs from that point on, never overwritten. Otherwise it creates
// dir (including parents) and copies every file in EmbeddedDefaultDir into
// it. Safe to call on every invocation; the existence check makes repeat
// calls cheap.
func EnsureAnalyzerDir(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "profiles.yaml")); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("analyze: seed analyzer dir: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("analyze: seed analyzer dir: %w", err)
	}

	err := fs.WalkDir(EmbeddedDefaultDir, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(EmbeddedDefaultDir, name)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("analyze: seed analyzer dir: %w", err)
	}
	return nil
}
