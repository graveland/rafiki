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
// "fundi/" is here and not merely tolerated on read: childstore.labelLookup
// accepts fundi/parent and fundi/root as authoritative fallbacks for records
// written before the rename, so a client that could SET them could forge a
// lineage and make IsDescendant — the authority predicate behind every
// steering verb — return a false positive across agents. Nothing writes the
// legacy spelling any more; nothing may.
var reservedLabelPrefixes = []string{"rafiki/", "fundi/"}

func reservedLabelPrefix(k string) string {
	for _, p := range reservedLabelPrefixes {
		if strings.HasPrefix(k, p) {
			return p
		}
	}
	return ""
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
// do not use the reserved rafiki/ prefix.
//
// The prefix was "fundi/" pre-rename; a child spawned before the rename still
// carries "fundi/*" auto-labels on its persisted record (no migration
// rewrites old rows), so a "rafiki/*" filter will not match it. See
// childstore.Session.Labels and docs/MIGRATING.md.
func validateUserLabelKeys(m map[string]string) error {
	for k := range m {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if p := reservedLabelPrefix(k); p != "" {
			return fmt.Errorf("labels with '%s' prefix are reserved (got: %s)", p, k)
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
