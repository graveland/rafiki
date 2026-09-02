// SPDX-License-Identifier: Apache-2.0

// Package clientstate persists small preferences for the rafiki client and its
// TUI: things a person chose that should survive a restart, but that no other
// machine and no daemon has any business knowing.
//
// One FILE with named sections, not a file per feature. A preference store that
// grows a new path per setting ends up with a directory nobody can enumerate,
// and the sections are tiny — the whole document is a few hundred bytes.
//
// It is deliberately NOT a cache. paths.CacheDir holds things that are
// disposable and regenerable; a query somebody composed by hand is neither, so
// this lives under paths.StateDir. It is also not config: ConfigDir is for
// files a person edits, and this one is written by the UI.
package clientstate

import (
	"encoding/json"
	"os"
	"path/filepath"

	"go.graveland.dev/rafiki/pkg/paths"
)

// State is the whole document. Every section is a POINTER so "never set" stays
// distinguishable from "set to its zero value" — the same rule the wire types
// follow, and it is what lets a reader tell an absent section from one somebody
// deliberately cleared.
//
// Adding a section is one field here plus its own type. Nothing else changes:
// Update's read-modify-write means a writer that knows about one section
// preserves every section it has never heard of.
type State struct {
	// ModelView is the cockpit's remembered model filter and ordering.
	ModelView *ModelView `json:"modelView,omitempty"`
}

// Load reads the document, returning a zero State on any failure.
//
// Every failure is silent and total: a preferences file must never be able to
// stop the client starting, so a missing, unreadable or corrupt one is simply
// no preferences.
func Load() State {
	b, err := os.ReadFile(paths.ClientStateFile())
	if err != nil {
		return State{}
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}
	}
	return s
}

// Save writes the document. Best-effort and silent, for the same reason Load
// is: losing a remembered preference costs one re-selection.
func Save(s State) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	path := paths.ClientStateFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// Write-then-rename through a UNIQUE temp file: two clients can exit at
	// once, and a shared temp name makes one truncate the other's half-written
	// file. A reader would then see a corrupt document rather than either
	// version of a good one.
	tmp, err := os.CreateTemp(filepath.Dir(path), "state-*.json")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
	}
}

// Update applies a change to one section and writes the result.
//
// READ-MODIFY-WRITE, and that is the whole point of the helper: a caller that
// marshalled only its own section would silently drop every other one. Two
// clients exiting at once still resolve last-writer-wins over the whole
// document, which is the right trade for preferences — the alternative is a
// lock file guarding a few hundred bytes nobody is contending for.
func Update(mutate func(*State)) {
	s := Load()
	mutate(&s)
	Save(s)
}
