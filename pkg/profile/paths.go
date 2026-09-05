// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"path/filepath"

	"go.graveland.dev/rafiki/pkg/paths"
)

// The manifest and the pointer are separate files on purpose. `rafiki profile
// use` writes the pointer constantly; profiles.toml is hand-edited. Folding
// the pointer into the manifest as a `current =` key would make every switch
// rewrite a hand-edited TOML, and Go's TOML encoders do not preserve comments.

// ProfilesFile is the hand-edited manifest.
func ProfilesFile() string { return filepath.Join(paths.ConfigDir(), "profiles.toml") }

// PointerFile holds one line: the selected profile's name.
func PointerFile() string { return filepath.Join(paths.ConfigDir(), "current-profile") }

// Dir is a profile's own config directory. Created 0700; the token inside it
// is 0600, matching the credential handling paths.TokenFile used to provide.
func Dir(name string) string { return filepath.Join(paths.ConfigDir(), "profiles", name) }

// TokenFile is a profile's control-plane credential.
func TokenFile(name string) string { return filepath.Join(Dir(name), "token") }

// PresetsFile is a profile's presets. Per-profile because a preset is a model
// plus labels, and two daemons' model universes need not overlap.
func PresetsFile(name string) string { return filepath.Join(Dir(name), "presets.json") }

// ActiveFile is a profile's active-child marker. RuntimeDir, beside the
// sockets, because it is runtime state — matching where paths.ActiveFile put
// the unscoped one.
func ActiveFile(name string) string {
	return filepath.Join(paths.RuntimeDir(), "profiles", name, "active")
}

// StateFile is a profile's own client-state document: the sections the UI
// writes as a side effect of use (last model, model view). The global
// document at paths.ClientStateFile() keeps only what a person set
// deliberately and no daemon influences.
func StateFile(name string) string {
	return filepath.Join(paths.StateDir(), "profiles", name, "client-state.json")
}
