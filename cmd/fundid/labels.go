package main

import (
	"fmt"
	"regexp"
	"strings"
)

// labelKeyRE is the allowed pattern for label keys.
// Alphanumeric plus: underscore, dot, forward-slash, hyphen.
var labelKeyRE = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

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
// do not use the reserved fundi/ prefix.
func validateUserLabelKeys(m map[string]string) error {
	for k := range m {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if strings.HasPrefix(k, "fundi/") {
			return fmt.Errorf("labels with 'fundi/' prefix are reserved (got: %s)", k)
		}
	}
	return nil
}

// validateUserRemoveKeys checks that all keys in remove are syntactically
// valid and do not target the reserved fundi/ namespace.
func validateUserRemoveKeys(keys []string) error {
	for _, k := range keys {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if strings.HasPrefix(k, "fundi/") {
			return fmt.Errorf("labels with 'fundi/' prefix are reserved (got: %s)", k)
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
