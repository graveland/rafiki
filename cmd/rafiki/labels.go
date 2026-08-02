package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// cliLabelKeyRE is the allowed character set for label keys.
var cliLabelKeyRE = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

// validateCLILabelKey validates a single label key from CLI input.
// Rejects empty keys, disallowed characters, and the reserved rafiki/ prefix.
//
// The prefix was "fundi/" pre-rename; a child spawned before the rename still
// carries "fundi/*" auto-labels on its persisted record (no migration
// rewrites old rows), so a "rafiki/*" filter will not match it. See
// childstore.Session.Labels and docs/MIGRATING.md.
func validateCLILabelKey(k string) error {
	if k == "" {
		return fmt.Errorf("label key must not be empty")
	}
	if !cliLabelKeyRE.MatchString(k) {
		return fmt.Errorf("label key %q contains invalid characters (allowed: a-z A-Z 0-9 _ . / -)", k)
	}
	if strings.HasPrefix(k, "rafiki/") {
		return fmt.Errorf("labels with 'rafiki/' prefix are reserved (got: %s)", k)
	}
	return nil
}

// parseLabelPairs parses a slice of "k=v" strings into a map.
// Returns an error if any entry is malformed or has an invalid key.
func parseLabelPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			return nil, fmt.Errorf("label %q is not in k=v format", pair)
		}
		k := pair[:idx]
		v := pair[idx+1:]
		if err := validateCLILabelKey(k); err != nil {
			return nil, err
		}
		result[k] = v
	}
	return result, nil
}

// parseLabelFilterPairs parses "k=v" strings for use in label filter queries
// (e.g. rafiki tail --label).  Unlike parseLabelPairs, this does NOT reject the
// rafiki/ prefix, since filtering by auto-labels (e.g. rafiki/model=...) is valid.
func parseLabelFilterPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		idx := strings.IndexByte(pair, '=')
		if idx < 0 {
			return nil, fmt.Errorf("label %q is not in k=v format", pair)
		}
		k := pair[:idx]
		v := pair[idx+1:]
		if k == "" {
			return nil, fmt.Errorf("label key must not be empty")
		}
		if !cliLabelKeyRE.MatchString(k) {
			return nil, fmt.Errorf("label key %q contains invalid characters (allowed: a-z A-Z 0-9 _ . / -)", k)
		}
		result[k] = v
	}
	return result, nil
}

// parseLabelFilterKeys validates keys for label filter queries (--has-label).
// Allows any valid key including the rafiki/ prefix, since filtering by
// auto-labels (e.g. --has-label rafiki/model) is valid.
func parseLabelFilterKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	for _, k := range keys {
		if k == "" {
			return nil, fmt.Errorf("label key must not be empty")
		}
		if !cliLabelKeyRE.MatchString(k) {
			return nil, fmt.Errorf("label key %q contains invalid characters (allowed: a-z A-Z 0-9 _ . / -)", k)
		}
	}
	return keys, nil
}

// parseEnvLabels parses a comma-separated "k=v,k2=v2" string (e.g. RAFIKI_DEFAULT_LABELS).
// Empty parts are silently skipped.
func parseEnvLabels(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	var pairs []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			pairs = append(pairs, part)
		}
	}
	return parseLabelPairs(pairs)
}

// mergeLabels merges multiple label maps left-to-right; later maps win on key conflicts.
// Returns nil when the combined result is empty.
func mergeLabels(maps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// formatLabels returns a compact "k=v,k2=v2" string for table display.
// Keys are sorted for deterministic output. Truncates to maxLen if non-zero.
//
// When includeAutoLabels is false (default for `rafiki list`), labels whose key
// starts with the reserved "rafiki/" prefix are omitted — they're auto-derived
// from data already shown in adjacent columns (MODEL, etc.) or in `rafiki get`,
// and they otherwise dominate the column width and crowd out user labels.
func formatLabels(labels map[string]string, maxLen int, includeAutoLabels bool) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if !includeAutoLabels && strings.HasPrefix(k, "rafiki/") {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "-"
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	s := b.String()
	if maxLen > 0 && len(s) > maxLen {
		return s[:maxLen-1] + "…"
	}
	return s
}
