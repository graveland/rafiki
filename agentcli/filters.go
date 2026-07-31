// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"fmt"
	"time"

	"github.com/timescale/rafiki/insights"
)

// FilterVals is the flag-value bag both binaries fill from their own flag
// definitions — shared PARSING semantics without shared registration.
type FilterVals struct {
	Since, Until                                string
	Owner, Persona, Source, Model, Path, Status string
	Entrypoint, ExcludeEntrypoint               string
	MinTokens                                   int64
	Text                                        string
	Limit                                       int
}

// ParseTime accepts RFC3339 first, then time.ParseDuration (subtracted from
// time.Now()), else error. Empty string returns (nil, nil).
// RFC3339 is tried before duration because a timestamp would otherwise be
// misread or rejected by ParseDuration.
func ParseTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}

	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}

	if d, err := time.ParseDuration(s); err == nil {
		t := time.Now().Add(-d)
		return &t, nil
	}

	return nil, fmt.Errorf("invalid time %q: use RFC3339 or a duration like 24h", s)
}

// validatePath checks that a Path string is one of the valid enum values.
// This deliberately duplicates the validation in insights.Path.validate
// because that method is unexported; the enum must stay in sync.
func validatePath(p string) error {
	switch p {
	case "", "proxy", "direct":
		return nil
	default:
		return fmt.Errorf("invalid path %q: want \"proxy\" or \"direct\"", p)
	}
}

// BindStatsFilter maps FilterVals to insights.StatsFilter, parsing time strings
// and validating the Path enum. Since/Until errors are wrapped with context.
func BindStatsFilter(v FilterVals) (insights.StatsFilter, error) {
	// Validate Path enum early to avoid DB round-trip on invalid input
	if err := validatePath(v.Path); err != nil {
		return insights.StatsFilter{}, err
	}

	since, err := ParseTime(v.Since)
	if err != nil {
		return insights.StatsFilter{}, fmt.Errorf("since: %w", err)
	}

	until, err := ParseTime(v.Until)
	if err != nil {
		return insights.StatsFilter{}, fmt.Errorf("until: %w", err)
	}

	return insights.StatsFilter{
		Since:   since,
		Until:   until,
		Owner:   v.Owner,
		Persona: v.Persona,
		Source:  v.Source,
		Model:   v.Model,
		Path:    insights.Path(v.Path),
	}, nil
}

// BindSearchFilter maps FilterVals to insights.SearchFilter, parsing time strings
// and validating the Path enum. Since/Until errors are wrapped with context.
func BindSearchFilter(v FilterVals) (insights.SearchFilter, error) {
	// Validate Path enum early to avoid DB round-trip on invalid input
	if err := validatePath(v.Path); err != nil {
		return insights.SearchFilter{}, err
	}

	since, err := ParseTime(v.Since)
	if err != nil {
		return insights.SearchFilter{}, fmt.Errorf("since: %w", err)
	}

	until, err := ParseTime(v.Until)
	if err != nil {
		return insights.SearchFilter{}, fmt.Errorf("until: %w", err)
	}

	return insights.SearchFilter{
		Since:             since,
		Until:             until,
		Owner:             v.Owner,
		Persona:           v.Persona,
		Source:            v.Source,
		Model:             v.Model,
		Status:            v.Status,
		Path:              insights.Path(v.Path),
		MinTokens:         v.MinTokens,
		Text:              v.Text,
		Entrypoint:        v.Entrypoint,
		ExcludeEntrypoint: v.ExcludeEntrypoint,
		Limit:             v.Limit,
	}, nil
}
