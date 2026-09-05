// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrNoManifest means profiles.toml does not exist at all. Callers turn this
// into a bootstrap, never into a silent default — see Resolve.
var ErrNoManifest = errors.New("no profiles configured")

// Load reads and validates the manifest.
func Load() (Set, error) {
	b, err := os.ReadFile(ProfilesFile())
	if errors.Is(err, os.ErrNotExist) {
		return Set{}, fmt.Errorf("%s: %w", ProfilesFile(), ErrNoManifest)
	}
	if err != nil {
		return Set{}, fmt.Errorf("read %s: %w", ProfilesFile(), err)
	}
	return Parse(b)
}

// Save writes the manifest atomically at 0600.
//
// 0600 rather than 0644: the file names hosts this client holds credentials
// for, and there is no reason for another account on the machine to enumerate
// them.
func Save(s Set) error {
	var buf bytes.Buffer
	buf.WriteString("# rafiki client profiles. Hand-edit freely.\n" +
		"# Each profile needs exactly one of `socket` or `url`.\n" +
		"# The selected profile is named in the `current-profile` file beside this one.\n\n")

	f := tomlFile{Profile: make(map[string]Profile, len(s.Profiles))}
	for name, p := range s.Profiles {
		p.Name = "" // never encoded; the table key carries it
		f.Profile[name] = p
	}
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return fmt.Errorf("encode profiles: %w", err)
	}
	return writeFileAtomic(ProfilesFile(), buf.Bytes(), 0o700, 0o600)
}

// LoadPointer returns the selected profile's name, or "" for any failure.
// Absent, unreadable and empty are all "nothing selected" — a pointer file is
// not worth failing a command over when the caller has a precedence chain to
// fall through.
func LoadPointer() string {
	b, err := os.ReadFile(PointerFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SavePointer records the selected profile.
func SavePointer(name string) error {
	return writeFileAtomic(PointerFile(), []byte(name+"\n"), 0o700, 0o600)
}

// ReadToken returns a profile's credential, trimmed, or "" if there is none.
func ReadToken(name string) string {
	b, err := os.ReadFile(TokenFile(name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteToken stores a profile's credential at 0600 inside a 0700 directory.
func WriteToken(name, token string) error {
	return writeFileAtomic(TokenFile(name), []byte(strings.TrimSpace(token)+"\n"), 0o700, 0o600)
}

// writeFileAtomic writes via a unique temp file in the destination directory
// and renames over the target.
//
// A unique temp name (os.CreateTemp) rather than "<path>.tmp": two clients can
// write at once, and a shared temp name lets one truncate the other's
// half-written bytes so a reader sees a corrupt file rather than either good
// version. This mirrors pkg/clientstate.Save.
func writeFileAtomic(path string, data []byte, dirMode, fileMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(name, fileMode); err != nil {
		os.Remove(name)
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("rename %s -> %s: %w", name, path, err)
	}
	return nil
}
