// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"go.graveland.dev/rafiki/pkg/paths"
)

// DefaultName is the profile bootstrap creates.
const DefaultName = "default"

// defaultProxy is the LLM proxy face a local daemon serves. Matches
// .env.example and the old `rafiki claude --url` default, so a bootstrapped
// profile leaves `rafiki claude` working rather than un-defaulted.
const defaultProxy = "http://localhost:8035"

// Selection is what the caller learned about which profile to use.
//
// EnvSet distinguishes an unset RAFIKI_PROFILE from one set to "". Empty is
// treated as unset — the conventional reading — but the caller reads the
// environment, so it has to pass the distinction through rather than have this
// package guess.
type Selection struct {
	Flag   string // -P/--profile
	Env    string // RAFIKI_PROFILE's value
	EnvSet bool   // whether RAFIKI_PROFILE was present at all
}

// Resolved is a profile plus the credential that belongs to it.
type Resolved struct {
	Profile
	Token string

	// Bootstrapped reports that this call created profiles.toml. The caller
	// prints the notice; this package does not write to stderr.
	Bootstrapped bool
}

// Describe names the profile and its endpoint for diagnostics. An error that
// says only "cannot reach the daemon" sends people to the wrong machine.
func (r Resolved) Describe() string {
	if r.URL != "" {
		return fmt.Sprintf("%s (%s)", r.Name, r.URL)
	}
	return fmt.Sprintf("%s (unix:%s)", r.Name, r.Socket)
}

// Resolve picks exactly one profile.
//
// There is no fallback chain past this: a client that cannot name its daemon
// must say so rather than guess, because guessing is how a destructive verb
// reaches the wrong machine.
func Resolve(sel Selection) (Resolved, error) {
	// Compute the name supplied by the caller (flag, env, or neither).
	// This is used both to decide whether to bootstrap and, if not,
	// to look up the profile.
	requestedName := sel.Flag
	if requestedName == "" && sel.EnvSet {
		requestedName = sel.Env // "" here means no explicit env, deliberately
	}

	set, err := Load()
	if errors.Is(err, ErrNoManifest) {
		// Do not bootstrap if the caller supplied an explicit selection.
		if requestedName != "" {
			return Resolved{}, fmt.Errorf(
				"profile %q requested but no profiles are configured yet; run `rafiki profile add --help`",
				requestedName)
		}
		return Bootstrap()
	}
	if err != nil {
		return Resolved{}, err
	}

	// If no explicit name from caller, try the pointer.
	name := requestedName
	if name == "" {
		name = LoadPointer()
	}
	if name == "" {
		return Resolved{}, fmt.Errorf(
			"no profile selected (known: %s); run `rafiki profile use <name>`, or pass -P <name>",
			strings.Join(set.Names(), ", "))
	}

	p, ok := set.Get(name)
	if !ok {
		return Resolved{}, fmt.Errorf(
			"unknown profile %q (known: %s); see `rafiki profile list`",
			name, strings.Join(set.Names(), ", "))
	}
	p.Proxy = EffectiveProxy(p)
	return Resolved{Profile: p, Token: ReadToken(name)}, nil
}

// EffectiveProxy returns the proxy URL that will actually be used for p: its
// own `proxy` field, or -- for a url profile with none set -- the url itself.
// One TLS listener serves both the control plane and the LLM proxy face, so
// a remote profile that never set --proxy still has a working default.
//
// This is deliberately resolve-time-only derivation, not a mutation of the
// stored Profile: it never gets written back to profiles.toml, so an
// explicit --proxy (a genuine user choice) always wins over it, and a
// profile with no `proxy` line keeps meaning "derive it" rather than
// freezing whatever the url happened to be when the profile was added.
//
// Exported so `rafiki profile show` can render the SAME value `rafiki
// claude` will actually use, rather than showing a bare "-" that implies no
// proxy is configured when one will in fact be derived.
func EffectiveProxy(p Profile) string {
	if p.Proxy != "" {
		return p.Proxy
	}
	if p.URL != "" {
		return p.URL
	}
	return ""
}

// Bootstrap creates the default profile on a machine that has none.
//
// It writes a real file rather than defaulting in memory, so the result is
// something a person can read and edit. It migrates NOTHING: an existing
// ~/.config/rafiki/token is left alone and never read again, because a
// half-migration that still half-works is worse than a one-line manual move.
func Bootstrap() (Resolved, error) {
	p := Profile{
		Name:   DefaultName,
		Socket: paths.SocketPath(),
		Proxy:  defaultProxy,
	}
	set := Set{Profiles: map[string]Profile{DefaultName: p}}
	if err := Save(set); err != nil {
		return Resolved{}, fmt.Errorf("bootstrap: %w", err)
	}
	if err := SavePointer(DefaultName); err != nil {
		return Resolved{}, fmt.Errorf("bootstrap: %w", err)
	}
	return Resolved{Profile: p, Token: ReadToken(DefaultName), Bootstrapped: true}, nil
}

// retiredEnv are the variables that used to aim the client. Each one is now a
// profile field, and each was a global that could not differ between two
// daemons — which is the fault profiles exist to fix.
//
// They are NOT removed from pkg/paths: rafikid still reads RAFIKI_URL,
// RAFIKI_TOKEN and RAFIKI_DEFAULT_MODEL from its own service environment,
// where they cannot drift apart because one operator wrote one file.
var retiredEnv = []struct{ name, replacement string }{
	{paths.URL, "the profile's `url` or `socket`"},
	{paths.Token, "the profile's token file"},
	{paths.Socket, "the profile's `socket`"},
	{paths.DefaultModel, "the profile's `model`"},
	{paths.DefaultPreset, "the profile's `preset`"},
	{paths.DefaultLabels, "the profile's `labels`"},
}

// CheckRetiredEnv fails when a variable that no longer aims the client is set.
//
// An error rather than a warning: the failure this replaces was SILENT — an
// exported RAFIKI_URL outranked --socket with no message, so scratch work
// aimed at production and the error named TCP while a socket path sat in the
// argv. A variable that no longer does anything must not look like it does.
func CheckRetiredEnv() error {
	var set []string
	for _, v := range retiredEnv {
		if _, ok := os.LookupEnv(v.name); ok && os.Getenv(v.name) != "" {
			set = append(set, fmt.Sprintf("%s (use %s)", v.name, v.replacement))
		}
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf(
		"these variables no longer configure the rafiki client: %s.\n"+
			"Unset them, then see `rafiki profile add --help`",
		strings.Join(set, "; "))
}
