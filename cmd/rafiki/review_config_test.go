// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
)

func strPtr(s string) *string   { return &s }
func f64Ptr(f float64) *float64 { return &f }
func i32Ptr(i int32) *int32     { return &i }
func allFieldsConfig() ReviewConfig {
	return ReviewConfig{
		Model:     strPtr("file-model"),
		Profile:   strPtr("file-profile"),
		BudgetUSD: f64Ptr(2.5),
		MinTurns:  i32Ptr(4),
	}
}

// The env override points at a nonexistent file: zero config, no error. This
// is also how every config-file test below isolates itself — the default
// path is never read by a test that points RAFIKI_REVIEW_CONFIG elsewhere.
func TestReviewConfigMissingFileIsZeroValueNoError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("RAFIKI_REVIEW_CONFIG", missing)

	cfg, err := loadReviewConfig()
	if err != nil {
		t.Fatalf("loadReviewConfig(%q): %v", missing, err)
	}
	if cfg.Model != nil || cfg.Profile != nil || cfg.BudgetUSD != nil || cfg.MinTurns != nil {
		t.Errorf("missing file: got %+v, want the zero config", cfg)
	}
	if got := reviewConfigPath(); got != missing {
		t.Errorf("reviewConfigPath() = %q, want the override %q", got, missing)
	}

	// The default path behaves the same: XDG_CONFIG_HOME is isolated
	// package-wide by TestMain and nothing writes review.json there, so the
	// read below is genuinely a miss.
	t.Setenv("RAFIKI_REVIEW_CONFIG", "")
	if got := reviewConfigPath(); got != filepath.Join(paths.ConfigDir(), "review.json") {
		t.Errorf("reviewConfigPath() = %q, want %q", got, filepath.Join(paths.ConfigDir(), "review.json"))
	}
	cfg, err = loadReviewConfig()
	if err != nil {
		t.Fatalf("loadReviewConfig(default path): %v", err)
	}
	if cfg.Model != nil || cfg.Profile != nil || cfg.BudgetUSD != nil || cfg.MinTurns != nil {
		t.Errorf("missing default file: got %+v, want the zero config", cfg)
	}
}

// A present file parses only the fields it carries, as pointers, so "absent"
// stays distinguishable from a zero value.
func TestReviewConfigParsesPresentFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(`{"model":"anthropic/claude-sonnet-5","budget_usd":0.75}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_REVIEW_CONFIG", path)

	cfg, err := loadReviewConfig()
	if err != nil {
		t.Fatalf("loadReviewConfig: %v", err)
	}
	if cfg.Model == nil || *cfg.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("Model = %v, want \"anthropic/claude-sonnet-5\"", cfg.Model)
	}
	if cfg.BudgetUSD == nil || *cfg.BudgetUSD != 0.75 {
		t.Errorf("BudgetUSD = %v, want 0.75", cfg.BudgetUSD)
	}
	if cfg.Profile != nil {
		t.Errorf("Profile = %v, want nil (absent from the file)", cfg.Profile)
	}
	if cfg.MinTurns != nil {
		t.Errorf("MinTurns = %v, want nil (absent from the file)", cfg.MinTurns)
	}
}

// A malformed file is a parse error naming the path, not a silent zero.
func TestReviewConfigMalformedFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(`{"budget_usd": "free"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_REVIEW_CONFIG", path)

	_, err := loadReviewConfig()
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the config path", err)
	}
}

// Flags win: mergeInto fills only the fields the request does not already
// carry, whatever the file says — design §3's "request field > the rest".
func TestReviewConfigMergeIntoNeverOverwritesASetField(t *testing.T) {
	flagModel := "flag-model"
	req := &rafikiv1.ConversationReviewRequest{Model: &flagModel}
	allFieldsConfig().mergeInto(req)
	if req.Model == nil || *req.Model != "flag-model" {
		t.Errorf("Model = %v, want the flag's value preserved", req.Model)
	}
	if req.Profile == nil || *req.Profile != "file-profile" {
		t.Errorf("Profile = %v, want the file's value", req.Profile)
	}
	if req.BudgetUsd == nil || *req.BudgetUsd != 2.5 {
		t.Errorf("BudgetUsd = %v, want the file's value", req.BudgetUsd)
	}
	if req.MinTurns == nil || *req.MinTurns != 4 {
		t.Errorf("MinTurns = %v, want the file's value", req.MinTurns)
	}
}

// The zero config merges nothing — the all-optional request stays untouched,
// which is what close --review sends on a machine with no review.json.
func TestReviewConfigZeroValueMergesNothing(t *testing.T) {
	req := &rafikiv1.ConversationReviewRequest{}
	ReviewConfig{}.mergeInto(req)
	if req.Model != nil || req.Profile != nil || req.BudgetUsd != nil || req.MinTurns != nil {
		t.Errorf("zero config set fields on the request: %+v", req)
	}
}
