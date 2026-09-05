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
	"go.graveland.dev/rafiki/pkg/profile"
)

// State is one document. Which document depends on the Scope it was loaded
// with: Currency lives in the global one, ModelView and LastModel in a
// profile's own. Every section is a POINTER so "never set" stays
// distinguishable from "set to its zero value".
type State struct {
	// ModelView is the cockpit's remembered model filter and ordering.
	ModelView *ModelView `json:"modelView,omitempty"`
	// LastModel is the model most recently spawned, keyed by child KIND.
	//
	// It makes RAFIKI_DEFAULT_MODEL optional rather than obsolete: the
	// variable is an explicit configuration and still outranks this, so
	// setting it keeps working exactly as before, and unsetting it falls back
	// to whatever you last chose instead of to the daemon's default.
	LastModel map[string]string `json:"lastModel,omitempty"`
	// Currency is the client's preferred display currency for cost figures
	// (TUI, `rafiki list`). Costs are still tracked and billed in USD
	// everywhere else -- this only converts the last-mile string a person
	// reads. Nil means "show USD", matching today's behavior.
	Currency *Currency `json:"currency,omitempty"`
}

// Currency is a manual, personally-maintained conversion rate. There is no
// live FX lookup: this is a display convenience, not a pricing source, and a
// stale rate is an acceptable trade for not adding a network dependency to a
// cosmetic feature.
type Currency struct {
	// Code is shown as a suffix on converted amounts (e.g. "CAD"). Not
	// validated against ISO 4217 -- it is just a label.
	Code string `json:"code,omitempty"`
	// Rate is local units per USD (e.g. 1.38 for a USD->CAD rate of 1.38).
	// Zero or negative means "not set" -- costfmt.Format falls back to USD.
	Rate float64 `json:"rate,omitempty"`
}

// Scope names which document a read or write applies to.
//
// Per-profile is the DEFAULT and global is the exception: a setting is global
// only when it is a property of the person that no daemon can influence —
// display and presentation. Everything describing how you work WITH a daemon
// belongs to a profile, because two daemons need not share a model catalog,
// and a remembered answer from one is a wrong answer for the other.
//
// The zero Scope is the global document, which is deliberate: a caller that
// forgets to say which profile it means gets the shared file, which holds only
// display preferences and can therefore never leak one daemon's state into
// another's.
type Scope struct {
	Profile string // "" = the global document
}

// path returns the file this scope reads and writes.
func (s Scope) path() string {
	if s.Profile == "" {
		return paths.ClientStateFile()
	}
	return profile.StateFile(s.Profile)
}

// LoadScoped reads a document, returning a zero State on any failure.
//
// Every failure is silent and total: a preferences file must never be able to
// stop the client starting, so a missing, unreadable or corrupt one is simply
// no preferences.
func LoadScoped(sc Scope) State {
	b, err := os.ReadFile(sc.path())
	if err != nil {
		return State{}
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}
	}
	return s
}

// SaveScoped writes a document. Best-effort and silent, for the same reason
// LoadScoped is: losing a remembered preference costs one re-selection.
func SaveScoped(sc Scope, s State) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	path := sc.path()
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

// UpdateScoped applies a change to one document and writes the result.
//
// READ-MODIFY-WRITE, and that is the whole point of the helper: a caller that
// marshalled only its own section would silently drop every other one.
func UpdateScoped(sc Scope, mutate func(*State)) {
	s := LoadScoped(sc)
	mutate(&s)
	SaveScoped(sc, s)
}

// LastModelFor returns the model most recently spawned for a profile and kind.
//
// Keyed by PROFILE because a daemon's model universe depends on its
// providers.toml — replaying the personal daemon's model onto the work daemon
// prefills a spawn that attaches and never answers. Keyed by KIND for the same
// reason one level down: a "claude" child resolves only Anthropic ids.
func LastModelFor(profileName, kind string) string {
	if profileName == "" || kind == "" {
		return ""
	}
	return LoadScoped(Scope{Profile: profileName}).LastModel[kind]
}

// RememberModel records the model a spawn actually used.
//
// An empty model is not recorded: "the daemon's default" is not a choice worth
// replaying, and storing it would pin whatever that default happened to be on
// the day.
func RememberModel(profileName, kind, model string) {
	if profileName == "" || kind == "" || model == "" {
		return
	}
	UpdateScoped(Scope{Profile: profileName}, func(s *State) {
		if s.LastModel == nil {
			s.LastModel = map[string]string{}
		}
		s.LastModel[kind] = model
	})
}
