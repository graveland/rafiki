// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
)

// reviewConfigEnv overrides the review config file's path. Default:
// <paths.ConfigDir()>/review.json.
const reviewConfigEnv = "RAFIKI_REVIEW_CONFIG"

// ReviewConfig is ~/.config/rafiki/review.json (RAFIKI_REVIEW_CONFIG
// overrides the path). Every field is optional; a missing file or a field
// absent from it means "let the daemon decide" — this struct's zero value is
// a valid, all-optional request.
//
// The file is client-side by design (the owner runs `rafiki` wherever the
// review is asked for; a deployment mounts its configmap there — design §3).
type ReviewConfig struct {
	Model     *string  `json:"model,omitempty"`
	Profile   *string  `json:"profile,omitempty"`
	BudgetUSD *float64 `json:"budget_usd,omitempty"`
	MinTurns  *int32   `json:"min_turns,omitempty"`
}

// reviewConfigPath resolves the review config file: $RAFIKI_REVIEW_CONFIG
// when set, else the client config directory's review.json (the same
// paths.ConfigDir every other client-side config file resolves from).
func reviewConfigPath() string {
	if p := os.Getenv(reviewConfigEnv); p != "" {
		return p
	}
	return filepath.Join(paths.ConfigDir(), "review.json")
}

// loadReviewConfig reads and parses the review config file. A missing file
// returns a zero ReviewConfig and no error — this is the expected case for
// most callers, per design §3 ("the daemon defaults alone are a working
// configuration"). Any other read/parse error is returned.
func loadReviewConfig() (ReviewConfig, error) {
	path := reviewConfigPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ReviewConfig{}, nil
		}
		return ReviewConfig{}, err
	}
	var cfg ReviewConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ReviewConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// mergeInto copies every non-nil field of c into req, never overwriting a
// field the caller already set via a flag — flags win, matching design §3's
// "Request field — the client read it from the client-side config file
// (below) or a flag overrode it" resolution order.
func (c ReviewConfig) mergeInto(req *rafikiv1.ConversationReviewRequest) {
	if req == nil {
		return
	}
	if c.Model != nil && req.Model == nil {
		req.Model = c.Model
	}
	if c.Profile != nil && req.Profile == nil {
		req.Profile = c.Profile
	}
	if c.BudgetUSD != nil && req.BudgetUsd == nil {
		req.BudgetUsd = c.BudgetUSD
	}
	if c.MinTurns != nil && req.MinTurns == nil {
		req.MinTurns = c.MinTurns
	}
}

// reviewAcceptStatusText renders a ReviewAcceptStatus for a client status
// line: `rafiki conversations review` prints it for every returned accept,
// and close --review prints it as a stderr note for anything that is not a
// plain enqueue.
func reviewAcceptStatusText(s rafikiv1.ReviewAcceptStatus) string {
	switch s {
	case rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED:
		return "enqueued"
	case rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ALREADY_RUNNING:
		return "already running"
	case rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL:
		return "queue full"
	default:
		return fmt.Sprintf("unknown (%d)", int32(s))
	}
}

// renderReviewAccepts writes one `<id>: <status>` line per returned accept.
// A conversation the caller named but the response does not mention is not
// invented here — out-of-scope ids are dropped from the accepted set
// silently by design (design §6: never a permission error, never a distinct
// status naming the id).
func renderReviewAccepts(w io.Writer, accepts []*rafikiv1.ConversationReviewAccept) error {
	for _, a := range accepts {
		if _, err := fmt.Fprintf(w, "%s: %s\n", a.GetConversationId(), reviewAcceptStatusText(a.GetStatus())); err != nil {
			return err
		}
	}
	return nil
}
