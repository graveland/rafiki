package main

import (
	"fmt"
	"regexp"
	"strings"
)

// labelKeyRE is the allowed pattern for label keys.
// Alphanumeric plus: underscore, dot, forward-slash, hyphen.
var labelKeyRE = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

// reservedLabelPrefixes are the namespaces only the daemon may write.
//
// "rafiki/" is where every stored lineage and auto-label lives (migration 0076
// rewrote the pre-rename fundi/ spellings), so a client that could SET one
// could forge a lineage and make IsDescendant — the authority predicate behind
// every steering verb — return a false positive across agents. "fundi/" stays
// reserved for the same reason even though nothing writes it: migration 0076's
// key rewrite maps fundi/<k> onto rafiki/<k>, so a user label there would be
// silently renamed into the daemon's namespace on the next migration.
var reservedLabelPrefixes = []string{"rafiki/", "fundi/"}

// reservedLabelKeys are labels only the daemon may write. "owner" is here for
// the same reason the prefixes are: it is matched by executor admission
// selectors (admits: owner=<user>), so a client that could set it could claim
// to be any owner and land children on another user's machine.
var reservedLabelKeys = []string{"owner"}

func reservedLabelPrefix(k string) string {
	for _, p := range reservedLabelPrefixes {
		if strings.HasPrefix(k, p) {
			return p
		}
	}
	return ""
}

func reservedLabelKey(k string) bool {
	for _, r := range reservedLabelKeys {
		if k == r {
			return true
		}
	}
	return false
}

// validateLabelKey returns an error if k is empty or contains disallowed characters.
func validateLabelKey(k string) error {
	if k == "" {
		return fmt.Errorf("label key must not be empty")
	}
	if !labelKeyRE.MatchString(k) {
		return fmt.Errorf("label key %q contains invalid characters (allowed: a-z A-Z 0-9 _ . / -)", k)
	}
	return nil
}

// validateUserLabelKeys checks that all keys in m are syntactically valid and
// do not use a reserved prefix.
//
// The prefix was "fundi/" pre-rename; migration 0076 rewrites every stored
// fundi/<k> to rafiki/<k>, so a "rafiki/*" filter matches every child whatever
// version spawned it. See childstore.Session.Labels and docs/MIGRATING.md.
func validateUserLabelKeys(m map[string]string) error {
	for k := range m {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if p := reservedLabelPrefix(k); p != "" {
			return fmt.Errorf("labels with '%s' prefix are reserved (got: %s)", p, k)
		}
		if reservedLabelKey(k) {
			return fmt.Errorf("label %q is reserved and written by the daemon", k)
		}
	}
	return nil
}

// validateUserRemoveKeys checks that all keys in remove are syntactically
// valid and do not target the reserved rafiki/ namespace.
func validateUserRemoveKeys(keys []string) error {
	for _, k := range keys {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if p := reservedLabelPrefix(k); p != "" {
			return fmt.Errorf("labels with '%s' prefix are reserved (got: %s)", p, k)
		}
		if reservedLabelKey(k) {
			return fmt.Errorf("label %q is reserved and written by the daemon", k)
		}
	}
	return nil
}

// copyLabels returns a defensive copy of m. Both nil and empty inputs return nil.
func copyLabels(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
